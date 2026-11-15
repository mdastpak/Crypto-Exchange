package delisting_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/delisting"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/price"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sym returns a unique symbol, in upper case as the database requires.
func sym(prefix string) string { return prefix + strings.ToUpper(uuid.NewString()[:5]) }

type books struct {
	t  *testing.T
	db *sql.DB
}

func (b books) user() ledger.Account {
	b.t.Helper()
	var hk shared.UUID
	require.NoError(b.t, b.db.QueryRow(`INSERT INTO users (status) VALUES ('ACTIVE') RETURNING hk`).Scan(&hk))
	return ledger.Account{UserHK: hk}
}

func (b books) amount(currency, value string) ledger.Amount {
	b.t.Helper()
	hk, err := shared.UUIDFromString(currency)
	require.NoError(b.t, err)
	return ledger.Amount{Currency: hk, Value: shared.MustNewDecimalFromString(value)}
}

func (b books) fund(a ledger.Account, currency, value string) {
	b.t.Helper()
	key := "fund:" + uuid.NewString()
	l := ledger.Ledger{DB: b.db}
	_, err := l.DetectDeposit(ctx, key+":detect", a, b.amount(currency, value), ledger.Ref{Type: "deposit", ID: key})
	require.NoError(b.t, err)
	_, err = l.CreditDeposit(ctx, key+":credit", a, b.amount(currency, value), ledger.Ref{Type: "deposit", ID: key})
	require.NoError(b.t, err)
}

func (b books) balance(a ledger.Account, currency string) [3]string {
	b.t.Helper()
	bal, err := ledger.Ledger{DB: b.db}.Balance(ctx, a, b.amount(currency, "0").Currency)
	require.NoError(b.t, err)
	return [3]string{bal.Available.String(), bal.Locked.String(), bal.PendingDeposit.String()}
}

func (b books) delisting(currency string) delisting.Delisting {
	b.t.Helper()
	list, err := delisting.List(ctx, b.db, currency)
	require.NoError(b.t, err)
	require.NotEmpty(b.t, list)
	return list[0]
}

// convertPlan is a standard delisting that converts into usdt at fixed, or
// at the reference TWAP without it.
func convertPlan(start time.Time, usdt, fixed string) delisting.Plan {
	p := plan(start, fullDelisting...)
	p.Settlement, p.ClaimDays, p.SettlementCurrencyHK = delisting.SettlementConvert, 0, usdt
	p.PriceSource, p.FixedPrice = delisting.PriceTWAP, ""
	if fixed != "" {
		p.PriceSource, p.FixedPrice = delisting.PriceFixed, fixed
	}
	return p
}

type fakePrices map[string]price.Price

func (f fakePrices) Get(_ context.Context, market string) (price.Price, error) {
	p, ok := f[market]
	if !ok {
		return price.Price{}, price.ErrNoPrice
	}
	return p, nil
}

// TestSettlement_ConvertsTheBalancesAtTheRecordedPrice runs a delisting
// that converts at a fixed price: frozen records the price once, each
// available balance converts into the settlement currency with the dust
// kept, a locked balance holds delisted until it is released and
// converted, and the books balance.
func TestSettlement_ConvertsTheBalancesAtTheRecordedPrice(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	b := books{t: t, db: db}
	xyz, usdt := insertCurrency(t, db, "XYZ", "approved"), insertCurrency(t, db, "USDT", "approved")
	_, err := db.Exec(`INSERT INTO currency_networks (currency_hk, network, decimals, custody_mode) VALUES ($1, 'tron', 6, 'ON_CHAIN')`, usdt)
	require.NoError(t, err)
	alice, bob, carol := b.user(), b.user(), b.user()
	b.fund(alice, xyz, "1000")
	b.fund(bob, xyz, "0.00001")
	b.fund(carol, xyz, "40")
	l := ledger.Ledger{DB: db}
	_, err = l.Lock(ctx, "hold:carol", carol, b.amount(xyz, "15"), ledger.Ref{Type: "withdrawal", ID: "w1"})
	require.NoError(t, err)

	start := time.Now()
	d, _, err := delisting.Schedule(ctx, db, xyz, convertPlan(start, usdt, "0.0125"), origin())
	require.NoError(t, err)
	preview, err := delisting.Report(ctx, db, xyz, d.ID)
	require.NoError(t, err)
	assert.Equal(t, 3, preview.Holders)
	assert.Equal(t, "1040.00001", preview.Remaining)
	assert.False(t, preview.PriceRecorded)
	require.NotNil(t, preview.Estimated)
	assert.Equal(t, "13.000000125", *preview.Estimated, "the preview at the fixed price")

	settler := delisting.Settler{}
	_, err = settler.Advance(ctx, db, start.Add(24*time.Hour), 100)
	require.NoError(t, err)
	assert.Equal(t, "frozen", lifecycle(t, db, xyz).State, "delisted waits for the locked balance")
	assert.Equal(t, [3]string{"0", "0", "0"}, b.balance(alice, xyz))
	assert.Equal(t, [3]string{"12.5", "0", "0"}, b.balance(alice, usdt))
	assert.Equal(t, [3]string{"0", "0", "0"}, b.balance(bob, xyz), "all dust")
	assert.Equal(t, [3]string{"0", "0", "0"}, b.balance(bob, usdt))
	assert.Equal(t, [3]string{"0", "15", "0"}, b.balance(carol, xyz))
	assert.Equal(t, [3]string{"0.3125", "0", "0"}, b.balance(carol, usdt))
	got := b.delisting(xyz)
	require.NotNil(t, got.SettlementPrice)
	assert.Equal(t, "0.0125", *got.SettlementPrice)
	assert.Equal(t, "fixed", got.SettlementPriceDetail)
	assert.Equal(t, 1, count(t, db, `SELECT count(*) FROM audit_events WHERE action = 'currency.delisting_price' AND target_id = $1`, xyz))

	_, err = l.Release(ctx, "hold:carol:release", carol, b.amount(xyz, "15"), ledger.Ref{Type: "withdrawal", ID: "w1"})
	require.NoError(t, err)
	_, err = settler.Advance(ctx, db, start.Add(24*time.Hour), 100)
	require.NoError(t, err)
	assert.Equal(t, [3]string{"0.5", "0", "0"}, b.balance(carol, usdt))
	assert.Equal(t, "delisted", lifecycle(t, db, xyz).State, "the last conversion lets delisted apply in the same run")
	assert.Equal(t, delisting.StatusCompleted, b.delisting(xyz).Status)

	report, err := delisting.Report(ctx, db, xyz, d.ID)
	require.NoError(t, err)
	assert.Equal(t, delisting.SettlementReport{Settlement: delisting.SettlementConvert, Remaining: "0", Available: "0",
		Price: got.SettlementPrice, PriceDetail: "fixed", PricedAt: report.PricedAt, PriceRecorded: true, Estimated: report.Estimated,
		Conversions: 4, Converted: "1040.00001", Credited: "13", Dust: "0.000000125"}, report)
	books, err := l.Reconcile(ctx, nil)
	require.NoError(t, err)
	assert.True(t, books.OK(), "%+v", books)
	assert.Equal(t, 1, count(t, db, `SELECT count(*) FROM audit_events WHERE action = 'currency.delisting_price' AND target_id = $1`, xyz),
		"the price is recorded once")
}

// TestSettlement_WaitsForALiveReferencePrice records the TWAP of the
// longest window, once the reference is live.
func TestSettlement_WaitsForALiveReferencePrice(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	b := books{t: t, db: db}
	symbol := sym("D")
	coin, usdt := insertCurrency(t, db, symbol, "approved"), insertCurrency(t, db, sym("U"), "approved")
	alice := b.user()
	b.fund(alice, coin, "10")
	start := time.Now()
	_, _, err := delisting.Schedule(ctx, db, coin, convertPlan(start, usdt, ""), origin())
	require.NoError(t, err)

	prices := fakePrices{}
	settler := delisting.Settler{Prices: prices}
	_, err = settler.Advance(ctx, db, start.Add(24*time.Hour), 100)
	require.NoError(t, err)
	assert.Equal(t, [3]string{"10", "0", "0"}, b.balance(alice, coin), "no price, no conversion")
	assert.Nil(t, b.delisting(coin).SettlementPrice)
	assert.Equal(t, "frozen", lifecycle(t, db, coin).State)

	var target string
	require.NoError(t, db.QueryRow(`SELECT info->>'symbol' FROM currencies WHERE hk = $1`, usdt).Scan(&target))
	prices[symbol+"/"+target] = price.Price{Status: price.StatusLive, Mid: shared.MustNewDecimalFromString("2.1"), At: time.Now(),
		TWAP: map[string]shared.Decimal{"60s": shared.MustNewDecimalFromString("2.05"), "300s": shared.MustNewDecimalFromString("2")}}
	_, err = settler.Advance(ctx, db, start.Add(24*time.Hour), 100)
	require.NoError(t, err)
	got := b.delisting(coin)
	require.NotNil(t, got.SettlementPrice)
	assert.Equal(t, "2", *got.SettlementPrice, "the longest window")
	assert.Contains(t, got.SettlementPriceDetail, "twap 300s of "+symbol+"/"+target)
	assert.Equal(t, [3]string{"20", "0", "0"}, b.balance(alice, usdt))
	assert.Equal(t, "delisted", lifecycle(t, db, coin).State)
}

// TestSettlement_AClaimHoldsTheBalances keeps the balances of a delisting
// for a claim, and delisted waits for them.
func TestSettlement_AClaimHoldsTheBalances(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	b := books{t: t, db: db}
	coin := insertCurrency(t, db, sym("C"), "approved")
	alice := b.user()
	b.fund(alice, coin, "3")
	start := time.Now()
	d, _, err := delisting.Schedule(ctx, db, coin, plan(start, fullDelisting...), origin())
	require.NoError(t, err)
	_, err = delisting.Settler{}.Advance(ctx, db, start.Add(24*time.Hour), 100)
	require.NoError(t, err)
	assert.Equal(t, [3]string{"3", "0", "0"}, b.balance(alice, coin))
	assert.Equal(t, "frozen", lifecycle(t, db, coin).State, "delisted waits while a balance is held")
	report, err := delisting.Report(ctx, db, coin, d.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Holders)
	assert.Nil(t, report.Price)
	_, err = delisting.Report(ctx, db, uuid.NewString(), d.ID)
	assert.ErrorIs(t, err, delisting.ErrNotFound, "through its currency only")
}

// TestSettlement_InstancesShareTheBalances runs eight settlers over many
// holders: each balance converts once.
func TestSettlement_InstancesShareTheBalances(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	db.SetMaxOpenConns(20)
	b := books{t: t, db: db}
	coin, usdt := insertCurrency(t, db, sym("M"), "approved"), insertCurrency(t, db, sym("N"), "approved")
	var users []ledger.Account
	for i := range 40 {
		u := b.user()
		b.fund(u, coin, fmt.Sprint(i+1))
		users = append(users, u)
	}
	start := time.Now()
	d, _, err := delisting.Schedule(ctx, db, coin, convertPlan(start, usdt, "3"), origin())
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 10 {
				if _, err := (delisting.Settler{}).Advance(ctx, db, start.Add(24*time.Hour), 3); err != nil {
					t.Errorf("advance: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
	_, err = delisting.Settler{}.Advance(ctx, db, start.Add(24*time.Hour), 100)
	require.NoError(t, err)
	for i, u := range users {
		assert.Equal(t, [3]string{fmt.Sprint((i + 1) * 3), "0", "0"}, b.balance(u, usdt))
	}
	assert.Equal(t, len(users), count(t, db, `SELECT count(*) FROM ledger_entries WHERE kind = 'conversion' AND reference_id = $1`, d.ID))
	assert.Equal(t, "delisted", lifecycle(t, db, coin).State)
}

func TestPlan_DelistedPassesThroughFrozen(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	start := time.Now()
	p := plan(start, currencies.LifecycleTradingHalted, currencies.LifecycleDelisted)
	p.Kind = delisting.KindEmergency
	assert.Contains(t, p.Validate(start), "steps")

	// A currency that a halt left at withdrawal_only cannot skip frozen
	// either.
	hk := insertCurrency(t, db, "SKIP", "approved")
	halt := plan(start, currencies.LifecycleWithdrawalOnly)
	halt.Kind = delisting.KindEmergency
	_, _, err := delisting.Schedule(ctx, db, hk, halt, origin())
	require.NoError(t, err)
	_, err = delisting.Advance(ctx, db, start, 10)
	require.NoError(t, err)
	last := plan(start.Add(time.Minute), currencies.LifecycleDelisted)
	last.Kind = delisting.KindEmergency
	assert.Empty(t, last.Validate(start))
	_, _, err = delisting.Schedule(ctx, db, hk, last, origin())
	assert.ErrorIs(t, err, delisting.ErrSkipsFrozen)
}
