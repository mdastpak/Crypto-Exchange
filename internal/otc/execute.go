package otc

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/agreements"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/domain/market"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/outbox"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/verification"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
)

// RefTrade is the type of the reference of the ledger entries of an OTC
// trade: the lock of what the user gives, and the settlement that ends it.
const RefTrade = "otc_trade"

// The outbox topics of the trades (#62). A message carries an event_id,
// unique per event, by which consumers drop a message delivered twice:
// the relay delivers at least once.
const (
	// TopicTradeExecuted carries a TradeExecuted of each trade.
	TopicTradeExecuted = "otc.trade_executed"
	// TopicBalanceChanged carries a BalanceChanged of each balance that a
	// trade changed.
	TopicBalanceChanged = "balances.changed"
)

// idempotencyKey is the form of the key of an execution, as that of a
// withdrawal: what an HTTP header carries well.
var idempotencyKey = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,100}$`)

// The codes of the refusals of an execution, beside those of the quotes.
const (
	CodeQuoteNotFound          = "quote_not_found"
	CodeQuoteExpired           = "quote_expired"
	CodeQuoteInvalid           = "quote_invalid"
	CodeQuoteNotSettleable     = "quote_not_settleable"
	CodeInventoryLimit         = "otc_inventory_limit"
	CodeIdempotencyKeyReused   = "idempotency_key_reused"
	CodeAgreementsPending      = "terms_update_required"
	codeInvalidExecutionFields = "quote_id: a UUID; idempotency_key: 1 to 100 letters, digits, dots, colons, underscores, or hyphens"
)

// ErrAgreementsPending refuses the trade of a user who has not accepted
// the current version of a required document.
var ErrAgreementsPending = &Refusal{Code: CodeAgreementsPending}

// ExecuteRequest takes the quote QuoteID of the account. A retry with the
// same IdempotencyKey returns the trade of the first.
type ExecuteRequest struct {
	Account        Account
	QuoteID        string
	IdempotencyKey string
	// IP is the client address, which the platform controls may name.
	IP string
}

func (r ExecuteRequest) validate() error {
	if _, err := uuid.Parse(r.QuoteID); err != nil || !idempotencyKey.MatchString(r.IdempotencyKey) {
		return &Refusal{Code: CodeInvalidQuoteRequest, Limit: codeInvalidExecutionFields}
	}
	if r.Account.SubAccountID != nil {
		if _, err := uuid.Parse(*r.Account.SubAccountID); err != nil {
			return &Refusal{Code: CodeInvalidQuoteRequest, Limit: "sub_account_id"}
		}
	}
	return nil
}

// digest is what a retry of the key must ask for again: the quote, and
// the account it is taken by.
func (r ExecuteRequest) digest() []byte {
	h := sha256.New()
	writeField(h, strings.ToLower(r.QuoteID))
	sub := ""
	if r.Account.SubAccountID != nil {
		sub = strings.ToLower(*r.Account.SubAccountID)
	}
	writeField(h, sub)
	return h.Sum(nil)
}

// Leg is an amount of a currency that a trade moved.
type Leg struct {
	Currency   string         `json:"currency" example:"USDT"`
	CurrencyHK string         `json:"currency_hk" format:"uuid"`
	Amount     shared.Decimal `json:"amount" swaggertype:"string" example:"32565.06"`
}

// Trade is an executed OTC trade, as its user sees it.
type Trade struct {
	ID           string         `json:"id" format:"uuid"`
	QuoteID      string         `json:"quote_id" format:"uuid"`
	SubAccountID *string        `json:"sub_account_id,omitempty" format:"uuid"`
	Market       string         `json:"market" example:"BTC/USDT"`
	Side         Side           `json:"side" enums:"buy,sell" example:"buy"`
	Quantity     shared.Decimal `json:"quantity" swaggertype:"string" example:"0.5"`
	Price        shared.Decimal `json:"price" swaggertype:"string" example:"65000.12"`
	Total        shared.Decimal `json:"total" swaggertype:"string" example:"32500.06"`
	Fee          shared.Decimal `json:"fee" swaggertype:"string" example:"65"`
	FeeBPS       int            `json:"fee_bps" example:"20"`
	FeeCurrency  string         `json:"fee_currency" example:"USDT"`
	// Give is what the user gave, and Receive what the user was credited:
	// a buy gives total + fee and receives the quantity; a sell gives the
	// quantity and receives total − fee.
	Give       Leg       `json:"give"`
	Receive    Leg       `json:"receive"`
	ExecutedAt time.Time `json:"executed_at"`
}

// Execution is the outcome of an execution: the trade, and whether it is
// the trade of an earlier request with the same key.
type Execution struct {
	Trade    Trade
	Replayed bool
}

// InventoryLimit bounds the net position of the exchange's inventory in a
// currency: MaxShort is how much it may owe, what it gave users beyond
// what they gave it, and MaxLong how much it may hold. Nil is no bound.
type InventoryLimit struct {
	MaxShort, MaxLong *decimal.Decimal
}

// ParseInventoryLimits parses otc.inventory_limits, by currency symbol.
func ParseInventoryLimits(raw map[string]config.OTCInventoryLimit) (map[string]InventoryLimit, error) {
	limits := make(map[string]InventoryLimit, len(raw))
	for symbol, l := range raw {
		var parsed InventoryLimit
		for _, f := range []struct {
			name, s string
			into    **decimal.Decimal
		}{{"max_short", l.MaxShort, &parsed.MaxShort}, {"max_long", l.MaxLong, &parsed.MaxLong}} {
			if f.s == "" {
				continue
			}
			d, err := decimal.NewFromString(f.s)
			if err != nil || d.IsNegative() {
				return nil, fmt.Errorf("otc.inventory_limits.%s.%s: %q is not an amount of zero or more", symbol, f.name, f.s)
			}
			*f.into = &d
		}
		limits[strings.ToUpper(symbol)] = parsed
	}
	return limits, nil
}

// Executor executes the quotes (#62): each in one transaction of the
// database, which takes the quote, checks every control again under its
// locks, settles the trade in the ledger, records it, moves the
// inventory's positions, and writes its events to the outbox. Nothing of a
// trade commits without the rest.
type Executor struct {
	DB           *sql.DB
	Store        *Store
	Ledger       ledger.Ledger
	Verification verification.Store
	// Legal returns the documents that a user must have accepted; nil
	// requires none.
	Legal func() config.LegalConfig
	// Limits are the inventory limits by currency symbol. A currency
	// without one is not bounded, unless RequireLimits.
	Limits map[string]InventoryLimit
	// RequireLimits refuses the trades of a currency without a limit with
	// both bounds, as a release build does: a currency renamed after the
	// configuration was written (#215) is not traded unbounded.
	RequireLimits bool
	// Committed runs after a trade commits, such as a nudge of the outbox
	// relay. It may be nil.
	Committed func()
	// Now is the clock of the quotes' expiry; nil is time.Now.
	Now func() time.Time

	// fail injects a failure after a step, for the tests.
	fail func(step string) error
}

// The steps after which the tests inject a failure.
const (
	stepLocked    = "locked"
	stepSettled   = "settled"
	stepRecorded  = "recorded"
	stepPositions = "positions"
	stepEvents    = "events"
)

func (e *Executor) step(name string) error {
	if e.fail == nil {
		return nil
	}
	return e.fail(name)
}

func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Execute takes the quote of the request and settles it, once: a retry of
// the key returns the first trade, and a quote is traded at most once. It
// refuses with a *Refusal, or with the refusal of the control that
// refuses: verification, restrictions, currencies, or the platform.
func (e *Executor) Execute(ctx context.Context, r ExecuteRequest, origin audit.Event) (Execution, error) {
	if err := r.validate(); err != nil {
		return Execution{}, err
	}
	digest := r.digest()
	if t, err := replay(ctx, e.DB, r.Account.UserHK, r.IdempotencyKey, digest); t != nil || err != nil {
		return Execution{Trade: deref(t), Replayed: t != nil}, err
	}
	out, err := e.run(ctx, r, digest, origin)
	if isUnique(err, "otc_trades_user_key") {
		// Another request of the key committed first, past the lock of
		// the key: a retry of it.
		if t, again := replay(ctx, e.DB, r.Account.UserHK, r.IdempotencyKey, digest); t != nil && again == nil {
			return Execution{Trade: *t, Replayed: true}, nil
		}
	}
	if err == nil && e.Committed != nil {
		e.Committed()
	}
	return out, err
}

func deref(t *Trade) Trade {
	if t == nil {
		return Trade{}
	}
	return *t
}

// run executes the request in one transaction. The locks are taken in the
// order of the repository: the user, the key, the platform state, the
// currencies and the market, the balances, then the positions of the
// inventory, each set in a fixed order.
func (e *Executor) run(ctx context.Context, r ExecuteRequest, digest []byte, origin audit.Event) (Execution, error) {
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return Execution{}, err
	}
	defer tx.Rollback()
	userHK := r.Account.UserHK
	if err := restrictions.Lock(ctx, tx, userHK); err != nil {
		return Execution{}, err
	}
	// The lock of the user is shared: two requests of one key wait here
	// for each other, so that the second finds the trade of the first
	// rather than a quote already taken.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('otc-execute'), hashtext($1 || ':' || $2))`,
		userHK, r.IdempotencyKey); err != nil {
		return Execution{}, fmt.Errorf("otc: the lock of the key: %w", err)
	}
	if t, err := replay(ctx, tx, userHK, r.IdempotencyKey, digest); t != nil || err != nil {
		return Execution{Trade: deref(t), Replayed: t != nil}, err
	}
	// The quote is taken first: once taken, no other request can trade it,
	// even if this one then fails, which leaves it untraded. A user asks
	// for a new one.
	q, err := e.Store.Take(ctx, r.QuoteID, userHK, r.Account.SubAccountID, e.now())
	switch {
	case errors.Is(err, ErrQuoteNotFound):
		return Execution{}, &Refusal{Code: CodeQuoteNotFound}
	case errors.Is(err, ErrQuoteExpired):
		return Execution{}, &Refusal{Code: CodeQuoteExpired}
	case errors.Is(err, ErrQuoteTampered):
		log.Printf("SECURITY: OTC quote %s of user %s fails its signature: %v; it is not executed", r.QuoteID, userHK, err)
		return Execution{}, &Refusal{Code: CodeQuoteInvalid}
	case err != nil:
		return Execution{}, err
	}
	m, err := e.check(ctx, tx, r, q)
	if err != nil {
		return Execution{}, err
	}
	s, err := settlementOf(q, m)
	if err != nil {
		return Execution{}, err
	}
	if err := e.settleable(ctx, tx, s); err != nil {
		return Execution{}, err
	}

	id := shared.NewUUID().String()
	account, err := ledgerAccount(r.Account)
	if err != nil {
		return Execution{}, err
	}
	ref := ledger.Ref{Type: RefTrade, ID: id}
	books := e.Ledger.In(tx)
	if _, err := books.Lock(ctx, "otc:"+id+":lock", account, ledger.Amount{Currency: s.give.currency, Value: s.lock()}, ref); err != nil {
		return Execution{}, refusalOf(err)
	}
	if err := e.step(stepLocked); err != nil {
		return Execution{}, err
	}
	res, err := books.SettleTrade(ctx, "otc:"+id+":settle", account, s.trade(), ref)
	if err != nil {
		return Execution{}, refusalOf(err)
	}
	if err := e.step(stepSettled); err != nil {
		return Execution{}, err
	}
	if err := record(ctx, tx, id, r, digest, q, m, s, res.EntryID); err != nil {
		return Execution{}, err
	}
	if err := e.step(stepRecorded); err != nil {
		return Execution{}, err
	}
	if err := e.move(ctx, tx, s); err != nil {
		return Execution{}, err
	}
	if err := e.step(stepPositions); err != nil {
		return Execution{}, err
	}
	t, err := readTrade(ctx, tx, `t.id = $1`, id)
	if err != nil {
		return Execution{}, err
	}
	if err := enqueue(ctx, tx, books, account, t, userHK, res.EntryID); err != nil {
		return Execution{}, err
	}
	if err := e.step(stepEvents); err != nil {
		return Execution{}, err
	}
	origin.ActorType, origin.ActorID = audit.ActorUser, userHK
	ev := audit.Of(origin, audit.ActionUserOTCTrade, audit.TargetOTCTrade, id)
	ev.Changes = &audit.Changes{After: map[string]any{
		"market": t.Market, "side": string(t.Side), "quote_id": t.QuoteID,
		"give": t.Give.Amount.String() + " " + t.Give.Currency, "receive": t.Receive.Amount.String() + " " + t.Receive.Currency,
		"fee": t.Fee.String() + " " + t.FeeCurrency,
	}}
	if err := audit.Record(ctx, tx, ev); err != nil {
		return Execution{}, err
	}
	if err := tx.Commit(); err != nil {
		return Execution{}, fmt.Errorf("otc: commit the trade: %w", err)
	}
	return Execution{Trade: t}, nil
}

// tradedMarket is the market of a trade, read under a share lock in the
// execution.
type tradedMarket struct {
	id, base, quote, baseHK, quoteHK string
}

// check checks the trade under the locks of its transaction: the platform
// feature, the agreements, the user's level, the restrictions and the
// security hold of both sides, the capabilities of both currencies, and
// the market, which must still trade.
func (e *Executor) check(ctx context.Context, tx *sql.Tx, r ExecuteRequest, q Quote) (tradedMarket, error) {
	userHK := r.Account.UserHK
	if err := platform.Check(ctx, tx, platform.FeatureTrading, platform.Subject{UserHK: userHK, IP: r.IP}); err != nil {
		return tradedMarket{}, err
	}
	if e.Legal != nil {
		missing, err := agreements.Missing(ctx, tx, e.Legal(), userHK)
		if err != nil {
			return tradedMarket{}, err
		}
		if len(missing) > 0 {
			return tradedMarket{}, ErrAgreementsPending
		}
	}
	if err := e.Verification.Check(ctx, tx, userHK, verification.TradeOTC); err != nil {
		return tradedMarket{}, err
	}
	m, err := lockMarket(ctx, tx, q.Market)
	if err != nil {
		return tradedMarket{}, err
	}
	baseOp, quoteOp := restrictions.OpBuy, restrictions.OpSell
	baseCap, quoteCap := currencies.CapabilityBuy, currencies.CapabilitySell
	if q.Side == SideSell {
		baseOp, quoteOp = quoteOp, baseOp
		baseCap, quoteCap = quoteCap, baseCap
	}
	for _, check := range []func() error{
		func() error { return restrictions.Check(ctx, tx, userHK, baseOp, m.baseHK) },
		func() error { return restrictions.Check(ctx, tx, userHK, quoteOp, m.quoteHK) },
		// Under a share lock: a change of a currency's lifecycle waits for
		// the trade, or the trade for it.
		func() error { return currencies.Check(ctx, tx, m.baseHK, baseCap, true) },
		func() error { return currencies.Check(ctx, tx, m.quoteHK, quoteCap, true) },
	} {
		if err := check(); err != nil {
			return tradedMarket{}, err
		}
	}
	return m, nil
}

// lockMarket reads the market symbol under a share lock, so that it does
// not stop trading before the trade commits, and checks its seal: a market
// changed in SQL does not trade in enforce mode (#139).
func lockMarket(ctx context.Context, tx *sql.Tx, symbol string) (tradedMarket, error) {
	base, quote, ok := strings.Cut(symbol, "/")
	if !ok {
		return tradedMarket{}, &Refusal{Code: CodeMarketNotQuoted}
	}
	var m tradedMarket
	var status string
	sealed := rowseal.Markets.NewRow()
	err := tx.QueryRowContext(ctx, `SELECT m.id, b.info->>'symbol', q.info->>'symbol', b.hk, q.hk, m.status::text, `+
		rowseal.Markets.Fields("m")+`
		FROM markets m JOIN currencies b ON b.hk = m.base_currency_hk JOIN currencies q ON q.hk = m.quote_currency_hk
		WHERE m.deleted_at IS NULL AND b.info->>'symbol' = $1 AND q.info->>'symbol' = $2
			AND b.status = 'approved' AND b.deleted_at IS NULL AND q.status = 'approved' AND q.deleted_at IS NULL
		FOR SHARE OF m`, base, quote).Scan(append([]any{&m.id, &m.base, &m.quote, &m.baseHK, &m.quoteHK, &status}, sealed.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return m, &Refusal{Code: CodeMarketNotQuoted}
	}
	if err != nil {
		return m, fmt.Errorf("otc: read the market %s: %w", symbol, err)
	}
	if err := rowseal.Check(ctx, rowseal.Markets, sealed); err != nil {
		return m, err
	}
	if status != string(market.MarketStatusActive) {
		return m, &Refusal{Code: CodeMarketInactive}
	}
	return m, nil
}

// side is one currency of a settlement.
type side struct {
	currency shared.UUID
	symbol   string
	amount   shared.Decimal
}

// settlement is what a trade moves: what the user gives, what the
// inventory gives the user, before a netted fee, and the fee.
type settlement struct {
	give, receive side
	fee           shared.Decimal
	// netted is a fee in the received currency, netted from the credit: a
	// sell. Else the fee is in the given currency, with it: a buy.
	netted bool
}

// lock is what the trade locks of the given currency: what it gives, and
// a fee with it.
func (s settlement) lock() shared.Decimal {
	if s.netted {
		return s.give.amount
	}
	return s.give.amount.Add(s.fee)
}

func (s settlement) trade() ledger.Trade {
	t := ledger.Trade{
		Give:    ledger.Amount{Currency: s.give.currency, Value: s.give.amount},
		Receive: ledger.Amount{Currency: s.receive.currency, Value: s.receive.amount},
	}
	if !s.fee.IsZero() {
		t.Fee = ledger.Amount{Currency: s.give.currency, Value: s.fee}
		if s.netted {
			t.Fee.Currency = s.receive.currency
		}
	}
	return t
}

// credited is what the user receives: the received amount less a netted
// fee.
func (s settlement) credited() shared.Decimal {
	if s.netted {
		return s.receive.amount.Sub(s.fee)
	}
	return s.receive.amount
}

// given is what the user gives in all: the amount and a fee with it.
func (s settlement) given() shared.Decimal { return s.lock() }

// settlementOf returns what the quote q of the market m settles. A buy
// gives total + fee of the quote currency and receives the quantity; a sell
// gives the quantity and receives total − fee.
func settlementOf(q Quote, m tradedMarket) (settlement, error) {
	base, err := shared.UUIDFromString(m.baseHK)
	if err != nil {
		return settlement{}, err
	}
	quote, err := shared.UUIDFromString(m.quoteHK)
	if err != nil {
		return settlement{}, err
	}
	if q.FeeCurrency != m.quote || !q.Quantity.IsPositive() || !q.Total.IsPositive() || q.Fee.IsNegative() {
		return settlement{}, &Refusal{Code: CodeQuoteNotSettleable}
	}
	b := side{currency: base, symbol: m.base, amount: q.Quantity}
	c := side{currency: quote, symbol: m.quote, amount: q.Total}
	if q.Side == SideBuy {
		return settlement{give: c, receive: b, fee: q.Fee}, nil
	}
	if !q.Total.Decimal().GreaterThan(q.Fee.Decimal()) {
		return settlement{}, &Refusal{Code: CodeQuoteNotSettleable}
	}
	return settlement{give: b, receive: c, fee: q.Fee, netted: true}, nil
}

// settleable refuses a settlement finer than the precision of a currency
// (#422), which the ledger would refuse: a quote of a market whose lot or
// tick is finer than its currencies.
func (e *Executor) settleable(ctx context.Context, tx *sql.Tx, s settlement) error {
	for _, c := range []struct {
		side    side
		amounts []shared.Decimal
	}{{s.give, []shared.Decimal{s.given()}}, {s.receive, []shared.Decimal{s.credited(), s.receive.amount}}} {
		p, err := ledger.Precision(ctx, tx, c.side.currency.String())
		if err != nil {
			return err
		}
		for _, a := range c.amounts {
			if d := a.Decimal(); !d.Equal(d.Truncate(p)) {
				return &Refusal{Code: CodeQuoteNotSettleable, Limit: fmt.Sprintf("%s has %d decimal places", c.side.symbol, p)}
			}
		}
	}
	return nil
}

// refusalOf turns the refusals of the ledger into those of the API.
func refusalOf(err error) error {
	switch {
	case errors.Is(err, ledger.ErrInsufficientBalance), errors.Is(err, ledger.ErrInsufficientLocked):
		return ErrInsufficientBalance
	case errors.Is(err, ledger.ErrInvalidRequest):
		return fmt.Errorf("%w: %w", &Refusal{Code: CodeQuoteNotSettleable}, err)
	}
	return err
}

func ledgerAccount(a Account) (ledger.Account, error) {
	user, err := shared.UUIDFromString(a.UserHK)
	if err != nil {
		return ledger.Account{}, fmt.Errorf("otc: the user %q: %w", a.UserHK, err)
	}
	account := ledger.Account{UserHK: user}
	if a.SubAccountID != nil {
		sub, err := shared.UUIDFromString(*a.SubAccountID)
		if err != nil {
			return ledger.Account{}, &Refusal{Code: CodeInvalidQuoteRequest, Limit: "sub_account_id"}
		}
		account.SubAccountID = &sub
	}
	return account, nil
}

// record writes the row of the trade, and seals it.
func record(ctx context.Context, tx *sql.Tx, id string, r ExecuteRequest, digest []byte, q Quote, m tradedMarket,
	s settlement, entryID string) error {
	components, err := json.Marshal(q.Components)
	if err != nil {
		return fmt.Errorf("otc: encode the components: %w", err)
	}
	var override any
	if q.Components.OverrideID != "" {
		override = q.Components.OverrideID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO otc_trades (id, user_id, sub_account_id, quote_id, idempotency_key, request_digest,
			market, base_currency_hk, quote_currency_hk, side, quantity, price, total, fee, fee_bps, fee_currency_hk,
			give_amount, receive_amount, components, override_id, entry_id, quoted_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::numeric, $12::numeric, $13::numeric, $14::numeric, $15, $16,
			$17::numeric, $18::numeric, $19, $20, $21, $22, $23)`,
		id, r.Account.UserHK, r.Account.SubAccountID, q.ID, r.IdempotencyKey, digest,
		q.Market, m.baseHK, m.quoteHK, string(q.Side), q.Quantity.String(), q.Price.String(), q.Total.String(), q.Fee.String(),
		q.FeeBPS, m.quoteHK, s.given().String(), s.credited().String(), components, override, entryID, q.CreatedAt, q.ExpiresAt)
	if err != nil {
		if isUnique(err, "otc_trades_quote_key") {
			return &Refusal{Code: CodeQuoteNotFound}
		}
		return fmt.Errorf("otc: record the trade: %w", err)
	}
	return rowseal.Reseal(ctx, tx, rowseal.OTCTrades, "id = $1", id)
}

// move changes the positions of the inventory by what the trade moved
// through it, under the locks of their rows, in the order of the
// currencies, and refuses a trade that takes a position beyond its limit.
// A trade that brings a position back toward zero always passes.
func (e *Executor) move(ctx context.Context, tx *sql.Tx, s settlement) error {
	deltas := []side{
		{currency: s.give.currency, symbol: s.give.symbol, amount: s.give.amount},
		{currency: s.receive.currency, symbol: s.receive.symbol, amount: s.receive.amount.Neg()},
	}
	sort.Slice(deltas, func(i, j int) bool { return deltas[i].currency.String() < deltas[j].currency.String() })
	for _, d := range deltas {
		var created string
		err := tx.QueryRowContext(ctx, `INSERT INTO otc_house_positions (currency_hk) VALUES ($1)
			ON CONFLICT (currency_hk) DO NOTHING RETURNING currency_hk`, d.currency).Scan(&created)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("otc: create the position of %s: %w", d.symbol, err)
		default:
			if err := rowseal.Reseal(ctx, tx, rowseal.OTCHousePositions, "currency_hk = $1", d.currency); err != nil {
				return err
			}
		}
		var position shared.Decimal
		sealed := rowseal.OTCHousePositions.NewRow()
		if err := tx.QueryRowContext(ctx, `SELECT position, `+rowseal.OTCHousePositions.Fields("p")+`
			FROM otc_house_positions p WHERE currency_hk = $1 FOR UPDATE`, d.currency).
			Scan(append([]any{&position}, sealed.Dest()...)...); err != nil {
			return fmt.Errorf("otc: read the position of %s: %w", d.symbol, err)
		}
		if err := rowseal.Check(ctx, rowseal.OTCHousePositions, sealed); err != nil {
			return err
		}
		next := position.Add(d.amount)
		l, ok := e.Limits[strings.ToUpper(d.symbol)]
		if e.RequireLimits && (!ok || l.MaxShort == nil || l.MaxLong == nil) {
			log.Printf("OTC: %s has no inventory limit in otc.inventory_limits; its trades are refused", d.symbol)
			return &Refusal{Code: CodeInventoryLimit}
		}
		if ok {
			n := next.Decimal()
			if d.amount.IsNegative() && l.MaxShort != nil && n.Neg().GreaterThan(*l.MaxShort) ||
				d.amount.IsPositive() && l.MaxLong != nil && n.GreaterThan(*l.MaxLong) {
				log.Printf("OTC: a trade would take the inventory of %s to %s, beyond its limit; refused", d.symbol, next)
				return &Refusal{Code: CodeInventoryLimit}
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE otc_house_positions SET position = $2::numeric, updated_at = now()
			WHERE currency_hk = $1`, d.currency, next.String()); err != nil {
			return fmt.Errorf("otc: write the position of %s: %w", d.symbol, err)
		}
		if err := rowseal.Reseal(ctx, tx, rowseal.OTCHousePositions, "currency_hk = $1", d.currency); err != nil {
			return err
		}
	}
	return nil
}

// TradeExecuted is the event of a trade, as TopicTradeExecuted carries it.
type TradeExecuted struct {
	EventID string `json:"event_id"`
	UserHK  string `json:"user_hk"`
	EntryID string `json:"entry_id"`
	Trade
}

// BalanceChanged is the event of a balance that a trade changed, as
// TopicBalanceChanged carries it: the balance after the trade.
type BalanceChanged struct {
	EventID      string         `json:"event_id"`
	UserHK       string         `json:"user_hk"`
	SubAccountID *string        `json:"sub_account_id,omitempty"`
	CurrencyHK   string         `json:"currency_hk"`
	Currency     string         `json:"currency"`
	Available    shared.Decimal `json:"available"`
	Locked       shared.Decimal `json:"locked"`
	Version      int            `json:"version"`
	EntryID      string         `json:"entry_id"`
	Cause        string         `json:"cause"`
	CauseID      string         `json:"cause_id"`
}

// enqueue writes the events of the trade t to the outbox, in its
// transaction: they are published once it commits, and never if it rolls
// back.
func enqueue(ctx context.Context, tx *sql.Tx, books ledger.Tx, a ledger.Account, t Trade, userHK, entryID string) error {
	payload, err := json.Marshal(TradeExecuted{EventID: "otc_trade:" + t.ID, UserHK: userHK, EntryID: entryID, Trade: t})
	if err != nil {
		return err
	}
	if err := outbox.Enqueue(ctx, tx, TopicTradeExecuted, payload); err != nil {
		return fmt.Errorf("otc: enqueue the trade: %w", err)
	}
	for _, leg := range []Leg{t.Give, t.Receive} {
		currency, err := shared.UUIDFromString(leg.CurrencyHK)
		if err != nil {
			return err
		}
		b, err := books.Balance(ctx, a, currency)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(BalanceChanged{
			EventID: "ledger:" + entryID + ":" + leg.CurrencyHK, UserHK: userHK, SubAccountID: t.SubAccountID,
			CurrencyHK: leg.CurrencyHK, Currency: leg.Currency, Available: b.Available, Locked: b.Locked,
			Version: b.Version, EntryID: entryID, Cause: RefTrade, CauseID: t.ID,
		})
		if err != nil {
			return err
		}
		if err := outbox.Enqueue(ctx, tx, TopicBalanceChanged, payload); err != nil {
			return fmt.Errorf("otc: enqueue the balance: %w", err)
		}
	}
	return nil
}

// querier reads in a transaction or out of one.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// replay returns the trade that a request with the key made, if any:
// ErrIdempotencyConflict when it asked for another.
func replay(ctx context.Context, q querier, userHK, key string, digest []byte) (*Trade, error) {
	var id string
	var stored []byte
	err := q.QueryRowContext(ctx, `SELECT id, request_digest FROM otc_trades WHERE user_id = $1 AND idempotency_key = $2`,
		userHK, key).Scan(&id, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(stored, digest) != 1 {
		return nil, &Refusal{Code: CodeIdempotencyKeyReused}
	}
	t, err := readTrade(ctx, q, `t.id = $1`, id)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// readTrade reads the trade where selects, and checks its seal: a trade
// changed in SQL is refused with rowseal.ErrUnverified.
func readTrade(ctx context.Context, q querier, where string, args ...any) (Trade, error) {
	var t Trade
	var side, baseHK, quoteHK string
	var give, receive shared.Decimal
	sealed := rowseal.OTCTrades.NewRow()
	err := q.QueryRowContext(ctx, `SELECT t.id, t.quote_id, t.sub_account_id, t.market, t.side, t.quantity, t.price, t.total,
			t.fee, t.fee_bps, t.base_currency_hk, t.quote_currency_hk, t.give_amount, t.receive_amount, t.executed_at, `+
		rowseal.OTCTrades.Fields("t")+` FROM otc_trades t WHERE `+where, args...).
		Scan(append([]any{&t.ID, &t.QuoteID, &t.SubAccountID, &t.Market, &side, &t.Quantity, &t.Price, &t.Total, &t.Fee,
			&t.FeeBPS, &baseHK, &quoteHK, &give, &receive, &t.ExecutedAt}, sealed.Dest()...)...)
	if err != nil {
		return t, fmt.Errorf("otc: read the trade: %w", err)
	}
	if err := rowseal.Check(ctx, rowseal.OTCTrades, sealed); err != nil {
		return Trade{}, err
	}
	t.Side = Side(side)
	t.ExecutedAt = t.ExecutedAt.UTC()
	base, quote, _ := strings.Cut(t.Market, "/")
	t.FeeCurrency = quote
	b, c := Leg{Currency: base, CurrencyHK: baseHK}, Leg{Currency: quote, CurrencyHK: quoteHK}
	if t.Side == SideBuy {
		t.Give, t.Receive = c, b
	} else {
		t.Give, t.Receive = b, c
	}
	t.Give.Amount, t.Receive.Amount = give, receive
	return t, nil
}

func isUnique(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
