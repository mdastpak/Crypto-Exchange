package routed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/otc/routing"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/shopspring/decimal"
)

// StatusResolved ends an order that a review resolved (#513): an
// administrator hedged it by hand, or routed its remainder again.
const StatusResolved = "resolved"

// The resolutions of an order left to a review.
const (
	// ResolveHandled records that a person took care of the order, such
	// as a hedge placed by hand or a remainder left to the inventory on
	// purpose. Nothing is placed.
	ResolveHandled = "handled"
	// ResolveRetry routes the remainder of the order again, at any venue,
	// as orders of new attempts. Only an order whose venue order is over
	// can be retried: one left to a review for no_route or
	// attempts_exhausted.
	ResolveRetry = "retry"
)

// MaxReviewedAttempts bounds the orders of one leg with the retries of
// the reviews: MaxAttempts, and then the attempts that administrators
// asked for.
const MaxReviewedAttempts = 10

// The refusals of a review.
var (
	ErrNotFound = errors.New("routed: the order is not found")
	// ErrNotInReview is a resolution of an order that is not in
	// manual_review, such as one resolved meanwhile.
	ErrNotInReview = errors.New("routed: the order is not left to a review")
	// ErrNotRetryable is a retry of an order whose venue order may still
	// be open or whose fills do not add up: a person checks the venue and
	// resolves it as handled.
	ErrNotRetryable = errors.New("routed: the order cannot be retried")
	// ErrRetryUnavailable is a retry on an instance that does not route.
	ErrRetryUnavailable = errors.New("routed: retries need otc.routing.enabled")
	// ErrNoRetryRoute is a retry that no venue can take now; nothing
	// changes.
	ErrNoRetryRoute = errors.New("routed: no venue takes the remainder now")
	// ErrRetriesExhausted is a retry of a leg that has
	// MaxReviewedAttempts orders.
	ErrRetriesExhausted = errors.New("routed: the leg has no attempts left")
)

// retryable are the reasons of a review after which the venue order is
// over: it was cancelled or refused, and only its remainder found no
// route.
var retryable = map[string]bool{ReasonNoRoute: true, ReasonAttemptsExhausted: true}

// Review lists the routed orders for administrators and resolves those
// left to a review (#513). Every node may serve it: a resolution locks the
// order's row, checks its seal, and writes the order, its event, its
// outbox message, and its audit event in one transaction.
type Review struct {
	DB *sql.DB
	// Planner and Reroute plan a retry. Without them, a retry answers
	// ErrRetryUnavailable.
	Planner *Planner
	Reroute Reroute
}

// Filter selects the orders of a list.
type Filter struct {
	// Status, Venue, and TradeID filter when set.
	Status, Venue, TradeID string
	// Limit bounds the orders; DefaultListLimit when zero.
	Limit int
}

// The bounds of a list.
const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// Statuses are the statuses that an order may have.
var Statuses = []string{StatusPlanned, StatusPlaced, StatusPartiallyFilled, StatusFilled, StatusFailed, StatusCancelled,
	StatusManualReview, StatusResolved}

// Item is an order as a review shows it.
type Item struct {
	ID           string          `json:"id"`
	TradeID      string          `json:"trade_id"`
	Leg          int             `json:"leg"`
	Attempt      int             `json:"attempt"`
	ParentID     *string         `json:"parent_id,omitempty"`
	Venue        string          `json:"venue"`
	Symbol       string          `json:"symbol"`
	Side         string          `json:"side"`
	Quantity     decimal.Decimal `json:"quantity"`
	Limit        decimal.Decimal `json:"limit_price"`
	Reference    decimal.Decimal `json:"reference_price"`
	Filled       decimal.Decimal `json:"filled"`
	FilledQuote  decimal.Decimal `json:"filled_quote"`
	Remaining    decimal.Decimal `json:"remaining"`
	Status       string          `json:"status"`
	Reason       *string         `json:"reason,omitempty"`
	ClientID     string          `json:"client_id"`
	VenueOrderID *string         `json:"venue_order_id,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	// Sealed is false for an order changed outside the backend: its
	// values are shown as the row has them, a node does not drive it, and
	// it cannot be resolved until its integrity finding, FindingID, is
	// re-sealed after a review.
	Sealed    bool   `json:"sealed"`
	FindingID *int64 `json:"finding_id,omitempty"`
	// Unsealed are the columns that the seal does not cover.
	Unsealed Unsealed    `json:"unsealed"`
	Events   []EventItem `json:"events"`
}

// Unsealed are the columns of an order that its seal does not cover: the
// lease and the deadlines. A change of them in SQL can only delay a
// deadline, but a review shows them.
type Unsealed struct {
	PlacedAt       *time.Time `json:"placed_at,omitempty"`
	NextAttemptAt  time.Time  `json:"next_attempt_at"`
	UncertainSince *time.Time `json:"uncertain_since,omitempty"`
	LeaseHolder    *string    `json:"lease_holder,omitempty"`
	LeaseUntil     *time.Time `json:"lease_until,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// EventItem is a transition of an order.
type EventItem struct {
	ID     int64     `json:"id"`
	From   *string   `json:"from_status,omitempty"`
	To     string    `json:"to_status"`
	Node   string    `json:"node"`
	Reason *string   `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// Resolution is a resolved order and the orders that a retry planned.
type Resolution struct {
	Order    Item   `json:"order"`
	Children []Item `json:"children,omitempty"`
	Action   string `json:"action"`
	Note     string `json:"note"`
}

// reviewColumns are the columns of an item, before those of the seal.
const reviewColumns = columns + `, o.lease_token, o.lease_holder, NULLIF(o.lease_until, '-infinity'), o.next_attempt_at, o.updated_at`

type scanned struct {
	Order
	token       int64
	holder      *string
	leaseUntil  *time.Time
	nextAttempt time.Time
	updatedAt   time.Time
}

func scanItem(sc interface{ Scan(...any) error }, sealed *rowseal.Row) (scanned, error) {
	var s scanned
	o := &s.Order
	err := sc.Scan(append([]any{
		&o.ID, &o.TradeID, &o.Leg, &o.Attempt, &o.ParentID, &o.Venue, &o.Symbol, &o.BaseAsset, &o.QuoteAsset, &o.BaseHK,
		&o.QuoteHK, &o.Side, &o.Quantity, &o.Limit, &o.TradePrice, &o.Reference, &o.ClientID, &o.VenueOrderID, &o.Status,
		&o.Filled, &o.FilledQuote, &o.ReservationID, &o.Reason, &o.CreatedAt, &o.UncertainSince, &o.PlacedAt,
		&s.token, &s.holder, &s.leaseUntil, &s.nextAttempt, &s.updatedAt,
	}, sealed.Dest()...)...)
	return s, err
}

func (s scanned) item(sealed bool) Item {
	it := Item{
		ID: s.ID, TradeID: s.TradeID, Leg: s.Leg, Attempt: s.Attempt, ParentID: s.ParentID, Venue: s.Venue, Symbol: s.Symbol,
		Side: s.Side, Quantity: s.Quantity, Limit: s.Limit, Reference: s.Reference, Filled: s.Filled, FilledQuote: s.FilledQuote,
		Remaining: s.Remaining(), Status: s.Status, Reason: s.Reason, ClientID: s.ClientID, VenueOrderID: s.VenueOrderID,
		CreatedAt: s.CreatedAt, Sealed: sealed, Events: []EventItem{},
		Unsealed: Unsealed{PlacedAt: s.PlacedAt, NextAttemptAt: s.nextAttempt, UncertainSince: s.UncertainSince,
			LeaseHolder: s.holder, UpdatedAt: s.updatedAt},
	}
	if s.holder != nil {
		it.Unsealed.LeaseUntil = s.leaseUntil
	}
	return it
}

// List returns the orders of f, the newest first, each with its events,
// whether it passes its seal, and its open integrity finding. An order
// that fails its seal is listed, flagged, rather than failing the list:
// a review must see it.
func (r *Review) List(ctx context.Context, f Filter) ([]Item, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	limit = min(limit, MaxListLimit)
	where, args := []string{"TRUE"}, []any{}
	add := func(cond, v string) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.Status != "" {
		add("o.status = $%d", f.Status)
	}
	if f.Venue != "" {
		add("o.venue = $%d", f.Venue)
	}
	if f.TradeID != "" {
		add("o.trade_id = $%d::uuid", f.TradeID)
	}
	args = append(args, limit)
	t := rowseal.OTCRoutedOrders
	//nolint:gosec // G202: the conditions are constant fragments with placeholders, and the fields the sealed columns of the table; every value is an argument.
	rows, err := r.DB.QueryContext(ctx, `SELECT `+reviewColumns+`, `+t.Fields("o")+` FROM otc_routed_orders o
		WHERE `+strings.Join(where, " AND ")+` ORDER BY o.created_at DESC, o.id LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("routed: list the orders: %w", err)
	}
	type row struct {
		s   scanned
		row *rowseal.Row
	}
	var got []row
	for rows.Next() {
		sealed := t.NewRow()
		s, err := scanItem(rows, sealed)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("routed: list the orders: %w", err)
		}
		got = append(got, row{s, sealed})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(got))
	byID := map[string]int{}
	ids := make([]string, 0, len(got))
	for _, g := range got {
		err := rowseal.Require(ctx, t, g.row)
		if err != nil && !errors.Is(err, rowseal.ErrUnverified) {
			return nil, err
		}
		it := g.s.item(err == nil)
		if !it.Sealed {
			if it.FindingID, err = openFinding(ctx, r.DB, it.ID); err != nil {
				return nil, err
			}
		}
		byID[it.ID] = len(out)
		ids = append(ids, it.ID)
		out = append(out, it)
	}
	if len(ids) == 0 {
		return out, nil
	}
	evs, err := r.DB.QueryContext(ctx, `SELECT order_id::text, id, from_status, to_status, node, reason, at
		FROM otc_routed_order_events WHERE order_id = ANY($1::uuid[]) ORDER BY order_id, id`, "{"+strings.Join(ids, ",")+"}")
	if err != nil {
		return nil, fmt.Errorf("routed: list the events of the orders: %w", err)
	}
	defer evs.Close()
	for evs.Next() {
		var id string
		var e EventItem
		if err := evs.Scan(&id, &e.ID, &e.From, &e.To, &e.Node, &e.Reason, &e.At); err != nil {
			return nil, err
		}
		i := byID[id]
		out[i].Events = append(out[i].Events, e)
	}
	return out, evs.Err()
}

// openFinding is the id of the open integrity finding of an order, if any.
func openFinding(ctx context.Context, db *sql.DB, id string) (*int64, error) {
	var fid int64
	err := db.QueryRowContext(ctx, `SELECT id FROM integrity_findings WHERE table_name = $1 AND row_key = $2 AND resolved_at IS NULL
		ORDER BY id DESC LIMIT 1`, rowseal.OTCRoutedOrders.Name, id).Scan(&fid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("routed: read the finding of %s: %w", id, err)
	}
	return &fid, nil
}

// Resolve resolves the order id, left to a review, by action, with the
// administrator's note: in one transaction, it locks the order, requires
// its seal, plans the orders of a retry, marks it resolved and seals it,
// and records its event, its outbox message, and an audit event. An order
// that fails its seal is rowseal.ErrUnverified: re-seal it after a review
// first. A lockdown of otc_routing refuses a retry with *platform.Locked.
func (r *Review) Resolve(ctx context.Context, id, action, note string, origin audit.Event) (Resolution, error) {
	if action != ResolveHandled && action != ResolveRetry {
		return Resolution{}, fmt.Errorf("routed: unknown resolution %q", action)
	}
	if action == ResolveRetry && (r.Planner == nil || r.Reroute == nil) {
		return Resolution{}, ErrRetryUnavailable
	}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return Resolution{}, err
	}
	defer func() { _ = tx.Rollback() }()
	t := rowseal.OTCRoutedOrders
	sealed := t.NewRow()
	s, err := scanItem(tx.QueryRowContext(ctx, `SELECT `+reviewColumns+`, `+t.Fields("o")+`
		FROM otc_routed_orders o WHERE o.id = $1::uuid FOR UPDATE`, id), sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return Resolution{}, ErrNotFound
	}
	if err != nil {
		return Resolution{}, fmt.Errorf("routed: read the order %s: %w", id, err)
	}
	if err := rowseal.Require(ctx, t, sealed); err != nil {
		return Resolution{}, err
	}
	if s.Status != StatusManualReview {
		return Resolution{}, ErrNotInReview
	}
	reason := ""
	if s.Reason != nil {
		reason = *s.Reason
	}
	var children []Order
	if action == ResolveRetry {
		if !retryable[reason] {
			return Resolution{}, ErrNotRetryable
		}
		if err := platform.Check(ctx, tx, platform.FeatureOTCRouting, platform.Subject{}); err != nil {
			return Resolution{}, err
		}
		children, err = rerouted(ctx, tx, r.Planner, r.Reroute, held{s.Order, s.token}, MaxReviewedAttempts, false)
		var none *routing.NoRoute
		switch {
		case errors.As(err, &none), errors.Is(err, ErrNotRoutable):
			return Resolution{}, ErrNoRetryRoute
		case errors.Is(err, errAttempts):
			return Resolution{}, ErrRetriesExhausted
		case err != nil:
			return Resolution{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE otc_routed_orders SET status = $2, updated_at = clock_timestamp() WHERE id = $1::uuid`,
		id, StatusResolved); err != nil {
		return Resolution{}, fmt.Errorf("routed: resolve the order %s: %w", id, err)
	}
	if err := rowseal.Reseal(ctx, tx, t, "id = $1::uuid", id); err != nil {
		return Resolution{}, err
	}
	node := "admin"
	if origin.ActorID != "" {
		node = "admin:" + origin.ActorID
	}
	if err := transition(ctx, tx, s.Order, StatusManualReview, StatusResolved, node, s.token, action); err != nil {
		return Resolution{}, err
	}
	childIDs := make([]string, 0, len(children))
	for _, c := range children {
		childIDs = append(childIDs, c.ID)
	}
	ev := audit.Of(origin, audit.ActionAdminOTCRoutedOrderResolve, audit.TargetOTCRoutedOrder, id)
	ev.Changes = &audit.Changes{
		Before: map[string]interface{}{"status": StatusManualReview, "reason": reason},
		After: map[string]interface{}{"status": StatusResolved, "action": action, "note": note, "trade_id": s.TradeID,
			"venue": s.Venue, "remaining": s.Remaining().String(), "children": childIDs},
	}
	if err := audit.Record(ctx, tx, ev); err != nil {
		return Resolution{}, err
	}
	if err := tx.Commit(); err != nil {
		return Resolution{}, fmt.Errorf("routed: resolve the order %s: %w", id, err)
	}
	items, err := r.List(ctx, Filter{TradeID: s.TradeID, Limit: MaxListLimit})
	if err != nil {
		return Resolution{}, err
	}
	res := Resolution{Action: action, Note: note}
	wanted := map[string]bool{}
	for _, c := range childIDs {
		wanted[c] = true
	}
	for _, it := range items {
		switch {
		case it.ID == id:
			res.Order = it
		case wanted[it.ID]:
			res.Children = append(res.Children, it)
		}
	}
	return res, nil
}
