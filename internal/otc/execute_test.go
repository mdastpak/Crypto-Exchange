package otc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/verification"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// execFixture is the user of dbFixture, with 100,000 of the quote currency
// and none of the base, a market of the two in the database, and an
// executor of the quotes.
type execFixture struct {
	dbFixture
	store    *Store
	exec     *Executor
	marketID string
}

func newExecFixture(t *testing.T) execFixture {
	t.Helper()
	f := newDBFixture(t)
	ctx := context.Background()
	require.NoError(t, f.DB.QueryRow(`SELECT b.info->>'symbol', q.info->>'symbol' FROM currencies b, currencies q
		WHERE b.hk = $1 AND q.hk = $2`, f.market.BaseHK, f.market.QuoteHK).Scan(&f.market.Base, &f.market.Quote))
	f.market.Symbol = f.market.Base + "/" + f.market.Quote
	var marketID string
	require.NoError(t, f.DB.QueryRow(`INSERT INTO markets (base_currency_hk, quote_currency_hk, status, taker_fee_bps)
		VALUES ($1, $2, 'ACTIVE', 20) RETURNING id::text`, f.market.BaseHK, f.market.QuoteHK).Scan(&marketID))
	require.NoError(t, rowseal.Reseal(ctx, f.DB, rowseal.Markets, "id = $1", marketID))
	store, err := NewStore(testRedis(t), f.keys)
	require.NoError(t, err)
	exec := &Executor{DB: f.DB, Store: store, Ledger: ledger.Ledger{DB: f.DB}, Verification: f.checks.Verification}
	return execFixture{dbFixture: f, store: store, exec: exec, marketID: marketID}
}

// quote stores a quote of the user at price, as the service gives it.
func (f execFixture) quote(t *testing.T, s Side, quantity, price, fee string) Quote {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	q := Quote{
		ID: shared.NewUUID().String(), UserHK: f.user, Market: f.market.Symbol, Side: s,
		Quantity: shared.MustNewDecimalFromString(quantity), Price: shared.MustNewDecimalFromString(price),
		Total: shared.Decimal(dec(quantity).Mul(dec(price))), Fee: shared.MustNewDecimalFromString(fee), FeeBPS: 20,
		FeeCurrency: f.market.Quote, CreatedAt: now, ExpiresAt: now.Add(8 * time.Second),
		Components: Components{Reference: dec(price), MarginBPS: dec("20"), OverrideID: "override-7"},
	}
	require.NoError(t, f.store.Save(context.Background(), q))
	return q
}

func (f execFixture) execute(q Quote, key string) (Execution, error) {
	return f.exec.Execute(context.Background(), ExecuteRequest{Account: Account{UserHK: f.user}, QuoteID: q.ID, IdempotencyKey: key},
		audit.Event{IP: "192.0.2.1"})
}

// balance returns available and locked of the user in currency.
func (f execFixture) balance(t *testing.T, currencyHK string) [2]string {
	t.Helper()
	b, err := ledger.Ledger{DB: f.DB}.Balance(context.Background(), ledger.Account{UserHK: mustUUID(t, f.user)}, mustUUID(t, currencyHK))
	require.NoError(t, err)
	return [2]string{b.Available.String(), b.Locked.String()}
}

// counts returns the trades, the outbox events, and the ledger entries of
// the trades of the user.
func (f execFixture) counts(t *testing.T) [3]int {
	t.Helper()
	var c [3]int
	require.NoError(t, f.DB.QueryRow(`SELECT
		(SELECT count(*) FROM otc_trades WHERE user_id = $1),
		(SELECT count(*) FROM outbox_events WHERE topic IN ($2, $3) AND payload->>'user_hk' = $1::text),
		(SELECT count(*) FROM ledger_entries e WHERE e.reference_type = $4
			AND EXISTS (SELECT 1 FROM balance_changes c WHERE c.entry_id = e.id AND c.user_id = $1))`,
		f.user, TopicTradeExecuted, TopicBalanceChanged, RefTrade).Scan(&c[0], &c[1], &c[2]))
	return c
}

// position returns the inventory's position in currency, "" without one.
func (f execFixture) position(t *testing.T, currencyHK string) string {
	t.Helper()
	var p string
	err := f.DB.QueryRow(`SELECT position::text FROM otc_house_positions WHERE currency_hk = $1`, currencyHK).Scan(&p)
	if err != nil {
		return ""
	}
	return shared.MustNewDecimalFromString(p).String()
}

// TestExecutor_SettlesABuyAndASell covers #62: a buy pays the total and
// the fee from the quote currency and receives the quantity; a sell gives
// the quantity and receives the total less the fee, netted. The books, the
// trade, its events, and the inventory's positions agree.
func TestExecutor_SettlesABuyAndASell(t *testing.T) {
	f := newExecFixture(t)
	buy := f.quote(t, SideBuy, "0.5", "60000", "60")
	out, err := f.execute(buy, "buy-1")
	require.NoError(t, err)
	assert.False(t, out.Replayed)
	tr := out.Trade
	assert.Equal(t, buy.ID, tr.QuoteID)
	assert.Equal(t, f.market.Symbol, tr.Market)
	assert.Equal(t, "30060", tr.Give.Amount.String(), "the total and the fee")
	assert.Equal(t, f.market.Quote, tr.Give.Currency)
	assert.Equal(t, "0.5", tr.Receive.Amount.String())
	assert.Equal(t, f.market.Base, tr.Receive.Currency)
	assert.WithinDuration(t, time.Now(), tr.ExecutedAt, time.Minute)
	assert.Equal(t, [2]string{"69940", "0"}, f.balance(t, f.market.QuoteHK), "the hold is ended, nothing stays locked")
	assert.Equal(t, [2]string{"0.5", "0"}, f.balance(t, f.market.BaseHK))
	assert.Equal(t, "30000", f.position(t, f.market.QuoteHK), "the inventory took the total")
	assert.Equal(t, "-0.5", f.position(t, f.market.BaseHK), "and gave the quantity: a short")

	var components []byte
	var override string
	require.NoError(t, f.DB.QueryRow(`SELECT components, override_id FROM otc_trades WHERE id = $1`, tr.ID).Scan(&components, &override))
	var parts Components
	require.NoError(t, json.Unmarshal(components, &parts))
	assertDec(t, "60000", parts.Reference, "the parts of the price are recorded")
	assert.Equal(t, "override-7", override)

	sell := f.quote(t, SideSell, "0.2", "59000", "23.6")
	out, err = f.execute(sell, "sell-1")
	require.NoError(t, err)
	assert.Equal(t, "0.2", out.Trade.Give.Amount.String())
	assert.Equal(t, "11776.4", out.Trade.Receive.Amount.String(), "11800 less the fee")
	assert.Equal(t, [2]string{"81716.4", "0"}, f.balance(t, f.market.QuoteHK))
	assert.Equal(t, [2]string{"0.3", "0"}, f.balance(t, f.market.BaseHK))
	assert.Equal(t, "18200", f.position(t, f.market.QuoteHK))
	assert.Equal(t, "-0.3", f.position(t, f.market.BaseHK))
	assert.Equal(t, [3]int{2, 6, 4}, f.counts(t), "two trades, each with its event and two balances, a lock and a settlement")

	report, err := ledger.Ledger{DB: f.DB}.Reconcile(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, report.Mismatches)
	for _, c := range report.Currencies {
		switch c.Currency.String() {
		case f.market.QuoteHK:
			assert.Equal(t, "-18200", c.Accounts[ledger.AccountInventory].Neg().String(), "the ledger's inventory is the position")
			assert.Equal(t, "83.6", c.Accounts[ledger.AccountFees].String(), "both fees")
		case f.market.BaseHK:
			assert.Equal(t, "-0.3", c.Accounts[ledger.AccountInventory].String())
		}
	}

	var event TradeExecuted
	var payload []byte
	require.NoError(t, f.DB.QueryRow(`SELECT payload FROM outbox_events WHERE topic = $1 AND payload->>'id' = $2`,
		TopicTradeExecuted, tr.ID).Scan(&payload))
	require.NoError(t, json.Unmarshal(payload, &event))
	assert.Equal(t, "otc_trade:"+tr.ID, event.EventID, "consumers drop a duplicate by it")
	assert.Equal(t, f.user, event.UserHK)
}

// TestExecutor_IsIdempotent covers #62: the same key returns the first
// trade, also when two requests of it run at once, and a key of another
// quote is refused.
func TestExecutor_IsIdempotent(t *testing.T) {
	f := newExecFixture(t)
	q := f.quote(t, SideBuy, "0.1", "60000", "12")
	first, err := f.execute(q, "key-1")
	require.NoError(t, err)
	again, err := f.execute(q, "key-1")
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, first.Trade, again.Trade)
	_, err = f.execute(f.quote(t, SideBuy, "0.1", "60000", "12"), "key-1")
	assert.Equal(t, CodeIdempotencyKeyReused, refusalCode(t, err))

	q2 := f.quote(t, SideBuy, "0.1", "60000", "12")
	var wg sync.WaitGroup
	results := make([]Execution, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = f.execute(q2, "key-2")
		}()
	}
	wg.Wait()
	replayed := 0
	for i := range results {
		require.NoError(t, errs[i])
		assert.Equal(t, results[0].Trade.ID, results[i].Trade.ID, "every request of the key has the one trade")
		if results[i].Replayed {
			replayed++
		}
	}
	assert.Equal(t, len(results)-1, replayed)
	assert.Equal(t, 2, f.counts(t)[0])
}

// TestExecutor_TradesAQuoteOnce covers #62: requests of one quote under
// different keys, at once, make exactly one trade.
func TestExecutor_TradesAQuoteOnce(t *testing.T) {
	f := newExecFixture(t)
	q := f.quote(t, SideBuy, "0.1", "60000", "12")
	var wg sync.WaitGroup
	var mu sync.Mutex
	traded, notFound := 0, 0
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.execute(q, "once-"+string(rune('a'+i)))
			mu.Lock()
			defer mu.Unlock()
			var r *Refusal
			switch {
			case err == nil:
				traded++
			case errors.As(err, &r) && r.Code == CodeQuoteNotFound:
				notFound++
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, traded)
	assert.Equal(t, 9, notFound)
	assert.Equal(t, [3]int{1, 3, 2}, f.counts(t))
}

// TestExecutor_FailureInjection covers the first criterion of #62: a
// failure at any step leaves no trade, no balance change, no event, and no
// position, and the quote, once taken, is not traded again.
func TestExecutor_FailureInjection(t *testing.T) {
	for _, step := range []string{stepLocked, stepSettled, stepRecorded, stepPositions, stepEvents} {
		t.Run(step, func(t *testing.T) {
			f := newExecFixture(t)
			injected := errors.New("injected at " + step)
			f.exec.fail = func(s string) error {
				if s == step {
					return injected
				}
				return nil
			}
			q := f.quote(t, SideBuy, "0.5", "60000", "60")
			_, err := f.execute(q, "fail-1")
			require.ErrorIs(t, err, injected)
			assert.Equal(t, [3]int{0, 0, 0}, f.counts(t))
			assert.Equal(t, [2]string{"100000", "0"}, f.balance(t, f.market.QuoteHK))
			assert.Equal(t, [2]string{"0", "0"}, f.balance(t, f.market.BaseHK))
			assert.Empty(t, f.position(t, f.market.BaseHK))

			f.exec.fail = nil
			_, err = f.execute(q, "fail-2")
			assert.Equal(t, CodeQuoteNotFound, refusalCode(t, err), "a taken quote is gone")
			out, err := f.execute(f.quote(t, SideBuy, "0.5", "60000", "60"), "fail-1")
			require.NoError(t, err, "the key of the failed request is free")
			assert.False(t, out.Replayed)
		})
	}
}

// TestExecutor_Refusals checks the controls again in the transaction.
func TestExecutor_Refusals(t *testing.T) {
	ctx := context.Background()
	f := newExecFixture(t)

	expired := f.quote(t, SideBuy, "0.1", "60000", "12")
	f.exec.Now = func() time.Time { return expired.ExpiresAt.Add(time.Millisecond) }
	_, err := f.execute(expired, "expired")
	assert.Equal(t, CodeQuoteExpired, refusalCode(t, err))
	f.exec.Now = nil

	_, err = f.execute(f.quote(t, SideBuy, "2", "60000", "240"), "too-much")
	assert.Equal(t, CodeInsufficientBalance, refusalCode(t, err))
	_, err = f.execute(f.quote(t, SideSell, "0.1", "60000", "12"), "no-base")
	assert.Equal(t, CodeInsufficientBalance, refusalCode(t, err), "no BTC to sell")

	var other string
	require.NoError(t, f.DB.QueryRow(`INSERT INTO users (status) VALUES ('ACTIVE') RETURNING hk::text`).Scan(&other))
	require.NoError(t, rowseal.Reseal(ctx, f.DB, rowseal.Users, "hk = $1", other))
	_, err = f.exec.Execute(ctx, ExecuteRequest{Account: Account{UserHK: other}, QuoteID: f.quote(t, SideBuy, "0.1", "60000", "12").ID,
		IdempotencyKey: "foreign"}, audit.Event{})
	assert.Equal(t, CodeQuoteNotFound, refusalCode(t, err), "another user's quote")

	_, err = f.execute(Quote{ID: "not-a-uuid"}, "bad")
	assert.Equal(t, CodeInvalidQuoteRequest, refusalCode(t, err))
	_, err = f.execute(f.quote(t, SideBuy, "0.1", "60000", "12"), "a key with spaces")
	assert.Equal(t, CodeInvalidQuoteRequest, refusalCode(t, err))

	// The market stopped trading after the quote.
	q := f.quote(t, SideBuy, "0.1", "60000", "12")
	_, err = f.DB.Exec(`UPDATE markets SET status = 'SUSPENDED' WHERE id = $1`, f.marketID)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, f.DB, rowseal.Markets, "id = $1", f.marketID))
	_, err = f.execute(q, "suspended")
	assert.Equal(t, CodeMarketInactive, refusalCode(t, err))
	_, err = f.DB.Exec(`UPDATE markets SET status = 'ACTIVE' WHERE id = $1`, f.marketID)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, f.DB, rowseal.Markets, "id = $1", f.marketID))

	// The documents the user has not accepted.
	f.exec.Legal = func() config.LegalConfig {
		return config.LegalConfig{Documents: []config.LegalDocument{{Kind: "terms", Version: "2026-11", URL: "https://example.com/terms"}}}
	}
	_, err = f.execute(f.quote(t, SideBuy, "0.1", "60000", "12"), "terms")
	assert.Equal(t, CodeAgreementsPending, refusalCode(t, err))
	f.exec.Legal = nil

	// A restriction of the quote currency, which a buy gives.
	admin := shared.NewUUID().String()
	_, err = restrictions.Create(ctx, f.DB, restrictions.New{UserHK: f.user, Scope: restrictions.ScopeAsset, CurrencyHK: f.market.QuoteHK,
		Operations: []restrictions.Operation{restrictions.OpSell}, Reason: restrictions.ReasonAMLReview, CreatedBy: admin},
		audit.Event{ActorType: audit.ActorAdmin, ActorID: admin})
	require.NoError(t, err)
	var held *restrictions.Refusal
	_, err = f.execute(f.quote(t, SideBuy, "0.1", "60000", "12"), "restricted")
	require.ErrorAs(t, err, &held)

	// A user below the level of OTC trades.
	var level0 string
	require.NoError(t, f.DB.QueryRow(`INSERT INTO users (status) VALUES ('ACTIVE') RETURNING hk::text`).Scan(&level0))
	require.NoError(t, rowseal.Reseal(ctx, f.DB, rowseal.Users, "hk = $1", level0))
	q = f.quote(t, SideBuy, "0.1", "60000", "12")
	q.ID, q.UserHK = shared.NewUUID().String(), level0
	require.NoError(t, f.store.Save(ctx, q))
	_, err = f.exec.Execute(ctx, ExecuteRequest{Account: Account{UserHK: level0}, QuoteID: q.ID, IdempotencyKey: "level0"}, audit.Event{})
	var refusal *verification.Refusal
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, [3]int{0, 0, 0}, f.counts(t), "nothing was traded")
}

// TestExecutor_InventoryLimits bounds the inventory's position per
// currency: a trade beyond it is refused, and one back toward zero passes.
func TestExecutor_InventoryLimits(t *testing.T) {
	f := newExecFixture(t)
	short, long := decimal.RequireFromString("0.5"), decimal.RequireFromString("100000")
	f.exec.Limits = map[string]InventoryLimit{f.market.Base: {MaxShort: &short}, f.market.Quote: {MaxLong: &long}}
	_, err := f.execute(f.quote(t, SideBuy, "0.4", "60000", "48"), "in")
	require.NoError(t, err)
	_, err = f.execute(f.quote(t, SideBuy, "0.2", "60000", "24"), "beyond")
	assert.Equal(t, CodeInventoryLimit, refusalCode(t, err), "a short of 0.6 is beyond 0.5")
	assert.Equal(t, "-0.4", f.position(t, f.market.BaseHK), "unchanged")
	assert.Equal(t, [2]string{"0.4", "0"}, f.balance(t, f.market.BaseHK))
	_, err = f.execute(f.quote(t, SideSell, "0.3", "60000", "36"), "back")
	require.NoError(t, err, "a sell brings the short back")
	assert.Equal(t, "-0.1", f.position(t, f.market.BaseHK))

	// A release build trades no currency without both bounds, such as one
	// renamed after the configuration was written.
	f.exec.RequireLimits = true
	_, err = f.execute(f.quote(t, SideBuy, "0.01", "60000", "1.2"), "unbounded")
	assert.Equal(t, CodeInventoryLimit, refusalCode(t, err), "the quote currency has no max_short")
	f.exec.Limits[f.market.Quote] = InventoryLimit{MaxShort: &long, MaxLong: &long}
	f.exec.Limits[f.market.Base] = InventoryLimit{MaxShort: &short, MaxLong: &long}
	_, err = f.execute(f.quote(t, SideBuy, "0.01", "60000", "1.2"), "bounded")
	require.NoError(t, err)
}

// TestExecutor_Precision refuses a quote finer than its currencies (#422).
func TestExecutor_Precision(t *testing.T) {
	f := newExecFixture(t)
	_, err := f.DB.Exec(`INSERT INTO currency_networks (currency_hk, network, decimals, custody_mode) VALUES ($1, 'tron', 2, 'OFF_CHAIN')`,
		f.market.QuoteHK)
	require.NoError(t, err)
	_, err = f.execute(f.quote(t, SideBuy, "0.001", "60000.123", "0.13"), "fine")
	assert.Equal(t, CodeQuoteNotSettleable, refusalCode(t, err), "60.000123 has more than 2 decimals")
	assert.Equal(t, [2]string{"100000", "0"}, f.balance(t, f.market.QuoteHK))
}

// TestExecutor_Seals covers #404 for the trades: a trade changed in SQL is
// never replayed, and a position changed in SQL refuses the trades of its
// currency in enforce mode.
func TestExecutor_Seals(t *testing.T) {
	f := newExecFixture(t)
	out, err := f.execute(f.quote(t, SideBuy, "0.1", "60000", "12"), "sealed")
	require.NoError(t, err)
	_, err = f.DB.Exec(`UPDATE otc_trades SET receive_amount = 10 WHERE id = $1`, out.Trade.ID)
	require.NoError(t, err)
	_, err = f.execute(Quote{ID: out.Trade.QuoteID}, "sealed")
	require.ErrorIs(t, err, rowseal.ErrUnverified, "a trade changed in SQL is not returned")

	_, err = f.DB.Exec(`UPDATE otc_house_positions SET position = 0 WHERE currency_hk = $1`, f.market.BaseHK)
	require.NoError(t, err)
	_, err = f.execute(f.quote(t, SideBuy, "0.1", "60000", "12"), "after-tamper")
	require.ErrorIs(t, err, rowseal.ErrUnverified, "a position lowered in SQL lets no trade through")
	assert.Equal(t, [2]string{"0.1", "0"}, f.balance(t, f.market.BaseHK), "only the first trade")

	// A balance changed in SQL is never traded (#404).
	_, err = f.DB.Exec(`UPDATE balances SET available = 1000000 WHERE user_id = $1 AND currency_hk = $2`, f.user, f.market.QuoteHK)
	require.NoError(t, err)
	_, err = f.execute(f.quote(t, SideSell, "0.05", "60000", "6"), "balance-tamper")
	require.ErrorIs(t, err, rowseal.ErrUnverified)
}

// precisionChecks are fakeChecks with the precisions of the currencies.
type precisionChecks struct {
	*fakeChecks
	places map[string]int32
}

func (p precisionChecks) Precision(_ context.Context, currencyHK string) (int32, error) {
	return p.places[currencyHK], nil
}

// TestService_RoundsToThePrecisionOfTheCurrencies covers #422 in the
// quotes: the total is rounded against the user to the quote currency's
// precision, and the fee with it, so that the execution can settle it.
func TestService_RoundsToThePrecisionOfTheCurrencies(t *testing.T) {
	ctx := context.Background()
	m := btcUSDT()
	m.TickSize = "0.001"
	store, err := NewStore(testRedis(t), nil)
	require.NoError(t, err)
	checks := precisionChecks{fakeChecks: &fakeChecks{}, places: map[string]int32{m.BaseHK: 8, m.QuoteHK: 2}}
	s, err := New(testSettings(), fakePrices{"BTC/USDT": live("65000.123")}, fakeMarkets{m}, checks, store, nil)
	require.NoError(t, err)
	alice := Account{UserHK: shared.NewUUID().String()}
	buy, err := s.Quote(ctx, Request{Account: alice, Market: "BTC/USDT", Side: SideBuy, Quantity: ptr("0.123")})
	require.NoError(t, err)
	exact := buy.Quantity.Decimal().Mul(buy.Price.Decimal())
	assert.True(t, buy.Total.Decimal().GreaterThanOrEqual(exact), "a buy's total is rounded up")
	assert.Equal(t, buy.Total.Decimal().String(), buy.Total.Decimal().Truncate(2).String(), "to 2 places")
	assert.Equal(t, buy.Fee.Decimal().String(), buy.Fee.Decimal().Truncate(2).String(), "and the fee too")
	sell, err := s.Quote(ctx, Request{Account: alice, Market: "BTC/USDT", Side: SideSell, Quantity: ptr("0.123")})
	require.NoError(t, err)
	assert.True(t, sell.Total.Decimal().LessThanOrEqual(sell.Quantity.Decimal().Mul(sell.Price.Decimal())), "a sell's is rounded down")
	assert.Equal(t, sell.Total.Decimal().String(), sell.Total.Decimal().Truncate(2).String())
}
