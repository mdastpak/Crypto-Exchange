// Package routed places the venue legs of the OTC trades' routes (#387,
// epic #383) as orders at the venues, and drives each order to its end:
// placed, filled, and posted to the ledger, or cancelled and its remainder
// routed again.
//
// An order is planned in the transaction of its trade (Planner.Create), so
// that a trade never commits without the orders that cover it, and an
// order never exists without its trade. A Driver then places and follows
// the orders of one venue, as a job of cmd/market-worker. Every node may
// run it: each order is driven by one node at a time, under a lease of its
// row whose token every write carries, and each call to the venue carries
// the order's own client id, which is looked up before anything is placed
// again. See docs/otc-routing.md#routed-orders.
package routed

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/otc/routing"
	"github.com/mdastpak/crypto-exchange/internal/outbox"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/venues"
	"github.com/mdastpak/crypto-exchange/internal/venues/state"

	"github.com/shopspring/decimal"
)

// RefType is the reference type of what a routed order writes: its
// reservation of the venue balance and its fills in the ledger.
const RefType = "otc_routed_order"

// TopicChanged carries an Event of each transition of a routed order. Its
// event_id is unique per transition, by which consumers drop a message
// delivered twice.
const TopicChanged = "otc.routed_order_changed"

// The states of an order. The withdrawal states of the migration are
// those of #388.
const (
	StatusPlanned         = "planned"
	StatusPlaced          = "placed"
	StatusPartiallyFilled = "partially_filled"
	StatusFilled          = "filled"
	StatusFailed          = "failed"
	StatusCancelled       = "cancelled"
	StatusManualReview    = "manual_review"
)

// The reasons that an order records, as codes: never the venue's text.
const (
	ReasonNotRoutable       = "not_routable"
	ReasonRejected          = "venue_rejected"
	ReasonTimedOut          = "timed_out"
	ReasonNoRoute           = "no_route"
	ReasonAttemptsExhausted = "attempts_exhausted"
	ReasonDuplicate         = "duplicate_unresolved"
	ReasonMissing           = "order_missing"
	ReasonFillInvalid       = "fill_invalid"
	ReasonBeyondLimit       = "price_beyond_limit"
)

// MaxAttempts bounds the orders of one leg of a trade: the first and the
// orders that route its remainder again. Then the remainder is left to a
// review.
const MaxAttempts = 3

// Trade is what a trade gives its routed orders.
type Trade struct {
	ID string
	// Side is the user's side, which is the side of the orders: the
	// inventory gave the user what the venue now buys, or took what it
	// now sells.
	Side string
	// Base and Quote are our currencies of the market, with their keys.
	Base, Quote, BaseHK, QuoteHK string
	// Price is the trade's price to the user, and Reference the reference
	// price of its quote.
	Price, Reference decimal.Decimal
	Route            routing.Route
}

// Settings are what the orders read of the configuration.
type Settings struct {
	// MaxSlippageBPS bounds a limit around the reference price.
	MaxSlippageBPS int
	// OrderTimeout is how long an order stays open before it is cancelled.
	OrderTimeout time.Duration
	// UnknownGrace is how long an order whose answer was lost is looked up
	// before it is placed again.
	UnknownGrace time.Duration
	// Venues are the fee and the asset mapping of each enabled venue.
	Venues map[string]VenueSettings
}

// VenueSettings are the settings of a venue.
type VenueSettings struct {
	FeeBPS  int
	Mapping venues.Mapping
}

// SettingsOf are the settings of c.
func SettingsOf(c *config.Configuration) Settings {
	r := c.OTC.Routing
	s := Settings{
		MaxSlippageBPS: r.MaxSlippageBPS,
		OrderTimeout:   time.Duration(r.OrderTimeoutSeconds) * time.Second,
		UnknownGrace:   time.Duration(r.UnknownGraceSeconds) * time.Second,
		Venues:         map[string]VenueSettings{},
	}
	for name, m := range state.MappingsOf(c.Venues) {
		s.Venues[name] = VenueSettings{FeeBPS: c.Venues.Exchanges[name].FeeBPS, Mapping: m}
	}
	return s
}

// Order is a routed order, as its row has it.
type Order struct {
	ID, TradeID              string
	Leg, Attempt             int
	ParentID                 *string
	Venue, Symbol            string
	BaseAsset, QuoteAsset    string
	BaseHK, QuoteHK          string
	Side                     string
	Quantity, Limit          decimal.Decimal
	TradePrice, Reference    decimal.Decimal
	ClientID                 string
	VenueOrderID             *string
	Status                   string
	Filled, FilledQuote      decimal.Decimal
	ReservationID            *string
	Reason                   *string
	CreatedAt                time.Time
	UncertainSince, PlacedAt *time.Time
}

// Remaining is what the order has not filled.
func (o Order) Remaining() decimal.Decimal { return o.Quantity.Sub(o.Filled) }

// Event is the message of a transition of an order, as TopicChanged
// carries it.
type Event struct {
	EventID     string          `json:"event_id"`
	OrderID     string          `json:"order_id"`
	TradeID     string          `json:"trade_id"`
	Venue       string          `json:"venue"`
	Symbol      string          `json:"symbol"`
	Side        string          `json:"side"`
	Quantity    decimal.Decimal `json:"quantity"`
	Limit       decimal.Decimal `json:"limit_price"`
	From        string          `json:"from_status,omitempty"`
	To          string          `json:"to_status"`
	Filled      decimal.Decimal `json:"filled"`
	FilledQuote decimal.Decimal `json:"filled_quote"`
	Reason      string          `json:"reason,omitempty"`
}

// ErrNotRoutable refuses a trade whose venue leg cannot be planned: its
// venue, market, or balance is gone, or the balance no longer covers it.
var ErrNotRoutable = errors.New("routed: the venue leg cannot be planned")

// Planner plans the routed orders.
type Planner struct {
	State *state.Store
	// Settings returns the settings of now.
	Settings func() Settings
	// Node names the node in the events of the orders.
	Node string
	// Now is the clock of the daily usage of the venues; nil is time.Now.
	Now func() time.Time
}

func (p *Planner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Create plans an order for each venue leg of the trade's route, in tx,
// the transaction of the trade: it reserves the venue balance that the
// order spends, adds its notional to the venue's daily usage (#386), and
// writes its event to the outbox. A lockdown of otc_routing refuses it
// with *platform.Locked, and a leg that can no longer be planned with
// ErrNotRoutable.
func (p *Planner) Create(ctx context.Context, tx *sql.Tx, t Trade) ([]Order, error) {
	var out []Order
	for i, leg := range t.Route.Legs {
		if leg.Source != routing.SourceVenue {
			continue
		}
		if len(out) == 0 {
			if err := platform.Check(ctx, tx, platform.FeatureOTCRouting, platform.Subject{}); err != nil {
				return nil, err
			}
		}
		o, err := p.plan(ctx, tx, t, i, 0, nil, leg)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// plan writes the order of a leg, its reservation, its usage, and its
// first event.
func (p *Planner) plan(ctx context.Context, tx *sql.Tx, t Trade, leg, attempt int, parent *string, l routing.Leg) (Order, error) {
	s := p.Settings()
	v, ok := s.Venues[l.Venue]
	if !ok {
		return Order{}, fmt.Errorf("%w: %s is not enabled", ErrNotRoutable, l.Venue)
	}
	baseAsset, _, okB := v.Mapping.Venue(t.Base, "")
	quoteAsset, _, okQ := v.Mapping.Venue(t.Quote, "")
	if !okB || !okQ {
		return Order{}, fmt.Errorf("%w: %s does not map %s/%s", ErrNotRoutable, l.Venue, t.Base, t.Quote)
	}
	in, listed, err := p.State.InstrumentOf(ctx, l.Venue, baseAsset, quoteAsset)
	if err != nil {
		return Order{}, err
	}
	if !listed || in.Symbol != l.Symbol {
		return Order{}, fmt.Errorf("%w: %s no longer lists %s", ErrNotRoutable, l.Venue, l.Symbol)
	}
	limit := Limit(t.Side, t.Price, t.Reference, v.FeeBPS, s.MaxSlippageBPS, in.Tick.Decimal())
	if !limit.IsPositive() {
		return Order{}, fmt.Errorf("%w: no limit for %s at %s", ErrNotRoutable, l.Symbol, l.Venue)
	}
	o := Order{
		TradeID: t.ID, Leg: leg, Attempt: attempt, ParentID: parent, Venue: l.Venue, Symbol: l.Symbol,
		BaseAsset: baseAsset, QuoteAsset: quoteAsset, BaseHK: t.BaseHK, QuoteHK: t.QuoteHK, Side: t.Side,
		Quantity: l.Quantity, Limit: limit, TradePrice: t.Price, Reference: t.Reference,
		ClientID: ClientID(t.ID, leg, attempt), Status: StatusPlanned, Filled: decimal.Zero, FilledQuote: decimal.Zero,
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO otc_routed_orders (trade_id, leg, attempt, parent_id, venue, symbol,
			base_asset, quote_asset, base_currency_hk, quote_currency_hk, side, quantity, limit_price, trade_price,
			reference_price, client_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::numeric, $13::numeric, $14::numeric, $15::numeric, $16)
		RETURNING id::text, created_at`,
		o.TradeID, o.Leg, o.Attempt, o.ParentID, o.Venue, o.Symbol, o.BaseAsset, o.QuoteAsset, o.BaseHK, o.QuoteHK, o.Side,
		o.Quantity.String(), o.Limit.String(), o.TradePrice.String(), o.Reference.String(), o.ClientID).Scan(&o.ID, &o.CreatedAt)
	if err != nil {
		return Order{}, fmt.Errorf("routed: plan the order of leg %d of trade %s: %w", leg, t.ID, err)
	}
	asset, amount := o.Spends(v.FeeBPS)
	r, err := p.State.ReserveIn(ctx, tx, state.Reservation{Venue: o.Venue, Asset: asset, Amount: shared.Decimal(amount),
		ReferenceType: RefType, ReferenceID: o.ID})
	switch {
	case errors.Is(err, state.ErrInsufficient), errors.Is(err, state.ErrNotRoutable):
		return Order{}, fmt.Errorf("%w: %w", ErrNotRoutable, err)
	case err != nil:
		return Order{}, err
	}
	o.ReservationID = &r.ID
	if _, err := tx.ExecContext(ctx, `UPDATE otc_routed_orders SET reservation_id = $2::uuid WHERE id = $1::uuid`, o.ID, r.ID); err != nil {
		return Order{}, fmt.Errorf("routed: reserve for the order %s: %w", o.ID, err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.OTCRoutedOrders, "id = $1::uuid", o.ID); err != nil {
		return Order{}, err
	}
	if err := routing.AddUsage(ctx, tx, o.Venue, o.QuoteHK, p.now(), o.Quantity.Mul(o.Reference)); err != nil {
		return Order{}, err
	}
	if err := transition(ctx, tx, o, "", StatusPlanned, p.Node, 0, ""); err != nil {
		return Order{}, err
	}
	return o, nil
}

// Spends returns the venue asset that the order spends and how much of it
// to reserve: a buy spends the quote asset, up to its quantity at its limit
// with the venue's fee, rounded up; a sell spends its quantity of the base
// asset.
func (o Order) Spends(feeBPS int) (string, decimal.Decimal) {
	if o.Side == string(venues.SideSell) {
		return o.BaseAsset, o.Quantity
	}
	fee := decimal.NewFromInt(int64(feeBPS)).Div(decimal.NewFromInt(10000))
	return o.QuoteAsset, o.Quantity.Mul(o.Limit).Mul(decimal.NewFromInt(1).Add(fee)).RoundCeil(8)
}

// Limit is the slippage guard of an order: the worst price at which it may
// fill. A buy pays at most what keeps the trade whole, price / (1 + fee),
// and at most reference × (1 + slippage), rounded down to the tick. A sell
// takes at least price / (1 − fee), and at least reference × (1 −
// slippage), rounded up to the tick. A zero tick does not round.
func Limit(side string, price, reference decimal.Decimal, feeBPS, slippageBPS int, tick decimal.Decimal) decimal.Decimal {
	one := decimal.NewFromInt(1)
	bps := func(n int) decimal.Decimal { return decimal.NewFromInt(int64(n)).Div(decimal.NewFromInt(10000)) }
	fee, slip := bps(feeBPS), bps(slippageBPS)
	const places = 16
	if side == string(venues.SideSell) {
		if !one.GreaterThan(fee) {
			return decimal.Zero
		}
		l := decimal.Max(price.DivRound(one.Sub(fee), places), reference.Mul(one.Sub(slip)))
		if tick.IsPositive() {
			return l.Div(tick).Ceil().Mul(tick)
		}
		return l
	}
	l := decimal.Min(price.DivRound(one.Add(fee), places), reference.Mul(one.Add(slip)))
	if tick.IsPositive() {
		return l.Div(tick).Floor().Mul(tick)
	}
	return l
}

// ClientID is our id of the order of a leg and attempt of a trade at its
// venue: derived, so that a node that lost the answer of a placement finds
// the order by the same id. It fits the 36 characters of Binance.
func ClientID(tradeID string, leg, attempt int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s:%d:%d", strings.ToLower(tradeID), leg, attempt))
	return "r" + hex.EncodeToString(sum[:])[:31]
}

// transition records a transition of the order in its events and the
// outbox, in tx.
func transition(ctx context.Context, tx *sql.Tx, o Order, from, to, node string, token int64, reason string) error {
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO otc_routed_order_events (order_id, from_status, to_status, node, lease_token, reason)
		VALUES ($1::uuid, NULLIF($2, ''), $3, $4, $5, NULLIF($6, '')) RETURNING id`, o.ID, from, to, nodeName(node), token, reason).Scan(&id)
	if err != nil {
		return fmt.Errorf("routed: record the transition of %s to %s: %w", o.ID, to, err)
	}
	payload, err := json.Marshal(Event{
		EventID: fmt.Sprintf("otc_routed_order:%s:%d", o.ID, id), OrderID: o.ID, TradeID: o.TradeID, Venue: o.Venue,
		Symbol: o.Symbol, Side: o.Side, Quantity: o.Quantity, Limit: o.Limit, From: from, To: to,
		Filled: o.Filled, FilledQuote: o.FilledQuote, Reason: reason,
	})
	if err != nil {
		return err
	}
	if err := outbox.Enqueue(ctx, tx, TopicChanged, payload); err != nil {
		return fmt.Errorf("routed: enqueue the transition of %s: %w", o.ID, err)
	}
	return nil
}

func nodeName(node string) string {
	if node == "" {
		return "unnamed"
	}
	if len(node) > 200 {
		return node[:200]
	}
	return node
}
