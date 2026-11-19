package routed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/jobs"
	"github.com/mdastpak/crypto-exchange/internal/otc/routing"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/venues"
	"github.com/mdastpak/crypto-exchange/internal/venues/state"

	"github.com/shopspring/decimal"
)

// ErrLeaseLost is a write of an order whose lease this node no longer
// holds: another node drives it now, and the write is rolled back.
var ErrLeaseLost = errors.New("routed: the lease of the order is lost")

// Delta is a change of the inventory's position in a currency.
type Delta struct {
	CurrencyHK string
	Amount     decimal.Decimal
}

// Positions moves the positions of the inventory by deltas, in tx: a fill
// of a routed order gives back to the inventory what the trade took from
// it.
type Positions func(ctx context.Context, tx *sql.Tx, deltas []Delta) error

// Reroute plans the route of the remainder of an order again, without the
// venues of exclude: a *routing.NoRoute when there is none.
type Reroute func(ctx context.Context, o Order, remainder decimal.Decimal, exclude []string) (Trade, error)

// The waits of a driver.
const (
	// retryWait is the wait after a call that failed without effect.
	retryWait = 5 * time.Second
	// followWait is the wait between two lookups of an open order.
	followWait = 2 * time.Second
	// lockedWait is the wait of an order while otc_routing is locked.
	lockedWait = 15 * time.Second
	// reviewWait is the wait of an order whose row fails its seal: it is
	// tried again, and fails again until it is reviewed.
	reviewWait = time.Hour
	// DefaultLease is how long a node holds an order without a write.
	DefaultLease = 30 * time.Second
	// DefaultBatch bounds the orders of one run.
	DefaultBatch = 50
)

// Driver places and follows the routed orders of one venue. It is a job of
// cmd/market-worker (#264), which runs it on one node at a time; each
// order is also held under a lease of its own, so that a node whose job
// lease expired mid-run cannot write over the node that took over.
type Driver struct {
	DB      *sql.DB
	Venue   venues.Venue
	State   *state.Store
	Ledger  ledger.Ledger
	Planner *Planner
	// Reroute and Positions are of the OTC trades.
	Reroute   Reroute
	Positions Positions
	// Node names this node in the leases and the events.
	Node string
	// Lease is how long a node holds an order; DefaultLease when zero.
	Lease time.Duration
	// Batch bounds the orders of a run; DefaultBatch when zero.
	Batch int
	// Now is the clock of the deadlines; nil is time.Now. The leases are
	// on the database's clock, which every node shares.
	Now func() time.Time
}

// Name implements jobs.Job.
func (d *Driver) Name() string { return "routed_orders:" + d.Venue.Name() }

// Sources implements jobs.Job: none of the market data sources.
func (d *Driver) Sources() []string { return nil }

func (d *Driver) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Driver) lease() time.Duration {
	if d.Lease > 0 {
		return d.Lease
	}
	return DefaultLease
}

// Run implements jobs.Job: it drives the orders of the venue that are
// due. A dry run of the worker places nothing. An order that fails is
// logged and tried again later; the run fails only when it cannot read the
// orders or lost its job lease.
func (d *Driver) Run(ctx context.Context, run *jobs.Run) error {
	if run.DryRun() {
		return nil
	}
	batch := d.Batch
	if batch <= 0 {
		batch = DefaultBatch
	}
	rows, err := d.DB.QueryContext(ctx, `SELECT id::text FROM otc_routed_orders
		WHERE venue = $1 AND status IN ('planned', 'placed', 'partially_filled') AND next_attempt_at <= $2
			AND lease_until < clock_timestamp()
		ORDER BY next_attempt_at, id LIMIT $3`, d.Venue.Name(), d.now(), batch)
	if err != nil {
		return fmt.Errorf("routed: read the due orders of %s: %w", d.Venue.Name(), err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	run.AddRead(len(ids))
	for _, id := range ids {
		if err := run.Fence(ctx); err != nil {
			return err
		}
		if err := d.Drive(ctx, id); err != nil {
			log.Printf("routed: order %s at %s: %v", id, d.Venue.Name(), err)
			continue
		}
		run.AddWritten(1)
	}
	return nil
}

// held is an order under this node's lease.
type held struct {
	Order
	token int64
}

// Drive takes the lease of the order and moves it one step: a planned
// order is looked up, then placed; an open one is looked up, its fills
// posted, and past its deadline cancelled. It does nothing when another
// node holds the order.
func (d *Driver) Drive(ctx context.Context, id string) error {
	var token int64
	err := d.DB.QueryRowContext(ctx, `UPDATE otc_routed_orders
		SET lease_token = lease_token + 1, lease_holder = $2, lease_until = clock_timestamp() + $3::interval
		WHERE id = $1::uuid AND lease_until < clock_timestamp() AND status IN ('planned', 'placed', 'partially_filled')
		RETURNING lease_token`, id, nodeName(d.Node), fmt.Sprintf("%d milliseconds", d.lease().Milliseconds())).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("routed: take the lease of %s: %w", id, err)
	}
	o, err := read(ctx, d.DB, id)
	if errors.Is(err, rowseal.ErrUnverified) {
		log.Printf("SECURITY: the routed order %s fails its seal; it is not driven until it is reviewed", id)
		return d.wait(ctx, held{Order{ID: id}, token}, reviewWait)
	}
	if err != nil {
		_ = d.wait(ctx, held{Order{ID: id}, token}, retryWait)
		return err
	}
	h := held{o, token}
	if locked, err := d.locked(ctx); err != nil || locked {
		_ = d.wait(ctx, h, lockedWait)
		return err
	}
	switch o.Status {
	case StatusPlanned:
		err = d.place(ctx, h)
	default:
		err = d.follow(ctx, h)
	}
	if err != nil {
		if !errors.Is(err, ErrLeaseLost) {
			_ = d.wait(ctx, h, retryWait)
		}
		return err
	}
	return nil
}

// locked reports whether a lockdown of otc_routing holds the orders.
func (d *Driver) locked(ctx context.Context) (bool, error) {
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	err = platform.Check(ctx, tx, platform.FeatureOTCRouting, platform.Subject{})
	var l *platform.Locked
	if errors.As(err, &l) {
		return true, nil
	}
	return false, err
}

// wait releases the lease, with the order due again after delay.
func (d *Driver) wait(ctx context.Context, h held, delay time.Duration) error {
	return d.release(ctx, d.DB, h, delay, nil)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// release ends the lease of h, with the order due after delay; uncertain
// sets uncertain_since, or clears it when it points to the zero time.
func (d *Driver) release(ctx context.Context, q execer, h held, delay time.Duration, uncertain *time.Time) error {
	var since any
	keep := uncertain == nil
	if uncertain != nil && !uncertain.IsZero() {
		since = *uncertain
	}
	res, err := q.ExecContext(ctx, `UPDATE otc_routed_orders SET lease_until = '-infinity', lease_holder = NULL,
			next_attempt_at = $3, uncertain_since = CASE WHEN $4 THEN uncertain_since ELSE $5::timestamptz END,
			updated_at = clock_timestamp()
		WHERE id = $1::uuid AND lease_token = $2 AND lease_until > clock_timestamp()`,
		h.ID, h.token, d.now().Add(delay), keep, since)
	if err != nil {
		return fmt.Errorf("routed: release the lease of %s: %w", h.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLeaseLost
	}
	return nil
}

// place looks the order up at the venue, by its client id, before it
// places it: an order whose answer was lost, or that a node placed before
// it crashed, is found and followed rather than placed twice.
func (d *Driver) place(ctx context.Context, h held) error {
	found, err := d.Venue.GetOrder(ctx, h.Symbol, h.ClientID)
	switch {
	case err == nil:
		return d.apply(ctx, h, found)
	case !errors.Is(err, venues.ErrNotFound):
		return d.wait(ctx, h, retryWait)
	}
	now := d.now()
	if h.UncertainSince != nil && now.Sub(*h.UncertainSince) < d.Planner.Settings().UnknownGrace {
		// A venue may not show an order at once: not found is no proof
		// yet that the lost placement did not happen (#478).
		return d.wait(ctx, h, followWait)
	}
	if r, ok := d.Venue.(venues.BookReader); ok {
		// The pre-check of #515: an order whose limit the book already
		// passed would rest until its deadline. A book that cannot be read
		// does not hold the order: the limit still guards its price.
		if b, err := r.BestPrices(ctx, h.Symbol); err == nil && !marketable(h.Side, h.Limit, b) {
			log.Printf("routed: the book of %s at %s is past the limit of the order %s; it is routed elsewhere", h.Symbol, h.Venue, h.ID)
			return d.close(ctx, h, StatusFailed, ReasonNotMarketable, true)
		}
	}
	placed, err := d.Venue.PlaceOrder(ctx, venues.OrderRequest{
		ClientID: h.ClientID, Symbol: h.Symbol, Side: venues.Side(h.Side), Type: venues.OrderLimit,
		Quantity: shared.Decimal(h.Quantity), Price: shared.Decimal(h.Limit),
	})
	if err == nil {
		return d.apply(ctx, h, placed)
	}
	var ve *venues.Error
	errors.As(err, &ve)
	switch kind := venues.KindOf(err); {
	case kind == venues.KindUnknown, kind == venues.KindRejected && ve != nil && ve.Code == venues.CodeDuplicate:
		if h.UncertainSince != nil {
			// Looked up past its grace, and still the venue would not
			// take the id, or would not say: a person looks.
			return d.close(ctx, h, StatusManualReview, ReasonDuplicate, false)
		}
		return d.release(ctx, d.DB, h, followWait, &now)
	case kind == venues.KindRejected:
		code := ""
		if ve != nil {
			code = ve.Code
		}
		log.Printf("routed: %s refused the order %s (%s)", h.Venue, h.ID, safeCode(code))
		return d.close(ctx, h, StatusFailed, ReasonRejected, true)
	default:
		return d.wait(ctx, h, retryWait)
	}
}

// marketable reports whether an order at limit can fill against the book:
// a buy when the lowest ask is at most its limit, a sell when the highest
// bid is at least it. An empty side is not marketable.
func marketable(side string, limit decimal.Decimal, b venues.Book) bool {
	if side == string(venues.SideSell) {
		bid := b.Bid.Decimal()
		return bid.IsPositive() && !bid.LessThan(limit)
	}
	ask := b.Ask.Decimal()
	return ask.IsPositive() && !ask.GreaterThan(limit)
}

var venueCode = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

// safeCode is a venue's code as a log may hold it.
func safeCode(code string) string {
	if venueCode.MatchString(code) {
		return code
	}
	return "unknown"
}

// follow looks an open order up, posts what it filled, and cancels it past
// its deadline.
func (d *Driver) follow(ctx context.Context, h held) error {
	got, err := d.Venue.GetOrder(ctx, h.Symbol, h.ClientID)
	if errors.Is(err, venues.ErrNotFound) {
		if h.PlacedAt != nil && d.now().Sub(*h.PlacedAt) > d.Planner.Settings().UnknownGrace {
			return d.close(ctx, h, StatusManualReview, ReasonMissing, false)
		}
		return d.wait(ctx, h, followWait)
	}
	if err != nil {
		return d.wait(ctx, h, retryWait)
	}
	if !got.Status.Done() && h.PlacedAt != nil && d.now().Sub(*h.PlacedAt) > d.Planner.Settings().OrderTimeout {
		// Cancel, then look again: a cancel may answer as if the order
		// were unknown when it filled meanwhile, as Binance's -2011 does.
		if _, err := d.Venue.CancelOrder(ctx, h.Symbol, h.ClientID); err != nil && !errors.Is(err, venues.ErrNotFound) &&
			venues.KindOf(err) == venues.KindRetryable {
			return d.wait(ctx, h, retryWait)
		}
		again, err := d.Venue.GetOrder(ctx, h.Symbol, h.ClientID)
		if err != nil {
			return d.wait(ctx, h, retryWait)
		}
		got = again
	}
	return d.apply(ctx, h, got)
}

// apply writes what the venue says of the order, in one transaction: the
// fill since the last one in the ledger, and the inventory's positions,
// its new status, and at its end the release of its reservation and the
// route of its remainder.
func (d *Driver) apply(ctx context.Context, h held, v venues.Order) error {
	filled, filledQuote := v.Filled.Decimal(), v.FilledQuote.Decimal()
	base, quote := filled.Sub(h.Filled), filledQuote.Sub(h.FilledQuote)
	if base.IsNegative() || quote.IsNegative() || filled.GreaterThan(h.Quantity) || base.IsPositive() != quote.IsPositive() {
		log.Printf("SECURITY: %s reports a fill of the routed order %s that does not follow the last one; it is left to a review",
			h.Venue, h.ID)
		return d.close(ctx, h, StatusManualReview, ReasonFillInvalid, false)
	}
	if base.IsPositive() && beyond(h.Side, quote.Div(base), h.Limit) {
		log.Printf("SECURITY: %s filled the routed order %s beyond its limit; it is left to a review", h.Venue, h.ID)
		return d.close(ctx, h, StatusManualReview, ReasonBeyondLimit, false)
	}
	status := StatusPlaced
	switch {
	case v.Status == venues.OrderFilled:
		status = StatusFilled
	case v.Status.Done():
		status = StatusCancelled
	case filled.IsPositive():
		status = StatusPartiallyFilled
	}
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if base.IsPositive() {
		if err := d.post(ctx, tx, h, base, quote, filled); err != nil {
			return err
		}
	}
	h.Filled, h.FilledQuote = filled, filledQuote
	var venueID *string
	if v.VenueID != "" {
		venueID = &v.VenueID
	}
	placedAt := h.PlacedAt
	if placedAt == nil {
		now := d.now()
		placedAt = &now
	}
	if status == StatusCancelled {
		reason := ReasonTimedOut
		if v.Status == venues.OrderRejected {
			reason = ReasonRejected
		}
		return d.finish(ctx, tx, h, StatusCancelled, reason, true, venueID)
	}
	if status == StatusFilled {
		return d.finish(ctx, tx, h, StatusFilled, "", false, venueID)
	}
	if err := d.update(ctx, tx, h, status, nil, venueID, placedAt); err != nil {
		return err
	}
	if status != h.Status {
		if err := transition(ctx, tx, h.Order, h.Status, status, d.Node, h.token, ""); err != nil {
			return err
		}
	}
	if err := d.release(ctx, tx, h, followWait, &time.Time{}); err != nil {
		return err
	}
	return tx.Commit()
}

// beyond reports whether an average price is worse than the limit.
func beyond(side string, price, limit decimal.Decimal) bool {
	if side == string(venues.SideSell) {
		return price.LessThan(limit)
	}
	return price.GreaterThan(limit)
}

// post posts a fill of base for quote in the ledger, and moves the
// inventory's positions back by it. The key is of the cumulative fill, so
// that a fill posted by a node that then lost its lease is not posted
// again.
func (d *Driver) post(ctx context.Context, tx *sql.Tx, h held, base, quote, cumulative decimal.Decimal) error {
	baseHK, err := shared.UUIDFromString(h.BaseHK)
	if err != nil {
		return err
	}
	quoteHK, err := shared.UUIDFromString(h.QuoteHK)
	if err != nil {
		return err
	}
	b := ledger.Amount{Currency: baseHK, Value: shared.Decimal(base)}
	q := ledger.Amount{Currency: quoteHK, Value: shared.Decimal(quote)}
	got, gave := b, q
	deltas := []Delta{{CurrencyHK: h.BaseHK, Amount: base}, {CurrencyHK: h.QuoteHK, Amount: quote.Neg()}}
	if h.Side == string(venues.SideSell) {
		got, gave = q, b
		deltas = []Delta{{CurrencyHK: h.BaseHK, Amount: base.Neg()}, {CurrencyHK: h.QuoteHK, Amount: quote}}
	}
	if _, err := d.Ledger.In(tx).VenueFill(ctx, "otc_routed:"+h.ID+":fill:"+cumulative.String(), h.Venue, got, gave,
		ledger.Ref{Type: RefType, ID: h.ID}); err != nil {
		return fmt.Errorf("routed: post the fill of %s: %w", h.ID, err)
	}
	if d.Positions != nil {
		if err := d.Positions(ctx, tx, deltas); err != nil {
			return err
		}
	}
	return nil
}

// update writes the sealed state of the order under its lease.
func (d *Driver) update(ctx context.Context, tx *sql.Tx, h held, status string, reason *string, venueID *string, placedAt *time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE otc_routed_orders SET status = $3, filled = $4::numeric, filled_quote = $5::numeric,
			venue_order_id = COALESCE($6, venue_order_id), reason = COALESCE($7, reason), placed_at = COALESCE($8, placed_at),
			updated_at = clock_timestamp()
		WHERE id = $1::uuid AND lease_token = $2 AND lease_until > clock_timestamp()`,
		h.ID, h.token, status, h.Filled.String(), h.FilledQuote.String(), venueID, reason, placedAt)
	if err != nil {
		return fmt.Errorf("routed: write the order %s: %w", h.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLeaseLost
	}
	return rowseal.Reseal(ctx, tx, rowseal.OTCRoutedOrders, "id = $1::uuid", h.ID)
}

// close ends an order without a fill to post, in its own transaction.
func (d *Driver) close(ctx context.Context, h held, status, reason string, reroute bool) error {
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return d.finish(ctx, tx, h, status, reason, reroute, nil)
}

// finish ends the order in tx and commits: it releases its reservation,
// and routes what it did not fill again when reroute, or leaves it to a
// review when nothing routes it.
func (d *Driver) finish(ctx context.Context, tx *sql.Tx, h held, status, reason string, reroute bool, venueID *string) error {
	var children []Order
	if reroute && h.Remaining().IsPositive() {
		var err error
		children, err = rerouted(ctx, tx, d.Planner, d.Reroute, h, MaxAttempts, true)
		var none *routing.NoRoute
		switch {
		case errors.As(err, &none), errors.Is(err, ErrNotRoutable):
			status, reason = StatusManualReview, ReasonNoRoute
		case errors.Is(err, errAttempts):
			status, reason = StatusManualReview, ReasonAttemptsExhausted
		case err != nil:
			return err
		}
	}
	var why *string
	if reason != "" {
		why = &reason
	}
	if err := d.update(ctx, tx, h, status, why, venueID, nil); err != nil {
		return err
	}
	if h.ReservationID != nil {
		if err := d.State.ReleaseIn(ctx, tx, *h.ReservationID); err != nil && !errors.Is(err, state.ErrNotFound) {
			return err
		}
	}
	if err := transition(ctx, tx, h.Order, h.Status, status, d.Node, h.token, reason); err != nil {
		return err
	}
	if len(children) > 0 {
		log.Printf("routed: the remainder %s of the order %s at %s is routed again in %d orders", h.Remaining(), h.ID, h.Venue, len(children))
	}
	if err := d.release(ctx, tx, h, 0, &time.Time{}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("routed: commit the order %s: %w", h.ID, err)
	}
	if status == StatusManualReview {
		log.Printf("routed: the order %s at %s is left to a review (%s)", h.ID, h.Venue, reason)
	}
	return nil
}

var errAttempts = errors.New("routed: the attempts of the leg are exhausted")

// rerouted plans the orders of the remainder of h in tx, attempts below
// limit: without the venues that the leg already went to when exclude,
// as the driver routes a remainder, or at any venue, as a review retries
// one (#513). A remainder that the route gives to the inventory needs no
// order: the trade already took it from the inventory's positions.
func rerouted(ctx context.Context, tx *sql.Tx, p *Planner, reroute Reroute, h held, limit int, exclude bool) ([]Order, error) {
	if reroute == nil || p == nil {
		return nil, &routing.NoRoute{Reason: "no_router"}
	}
	var next int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt), 0) + 1 FROM otc_routed_orders WHERE trade_id = $1::uuid AND leg = $2`,
		h.TradeID, h.Leg).Scan(&next); err != nil {
		return nil, err
	}
	if next >= limit {
		return nil, errAttempts
	}
	var excluded []string
	if exclude {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT venue FROM otc_routed_orders WHERE trade_id = $1::uuid AND leg = $2 ORDER BY venue`,
			h.TradeID, h.Leg)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return nil, err
			}
			excluded = append(excluded, v)
		}
		rows.Close()
	}
	t, err := reroute(ctx, h.Order, h.Remaining(), excluded)
	if err != nil {
		return nil, err
	}
	var out []Order
	for _, l := range t.Route.Legs {
		if l.Source != routing.SourceVenue {
			continue
		}
		if next >= limit {
			return nil, errAttempts
		}
		o, err := p.plan(ctx, tx, t, h.Leg, next, &h.ID, l)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
		next++
	}
	return out, nil
}

// columns are the columns that read reads, before those of the seal.
const columns = `o.id::text, o.trade_id::text, o.leg, o.attempt, o.parent_id::text, o.venue, o.symbol, o.base_asset,
	o.quote_asset, o.base_currency_hk::text, o.quote_currency_hk::text, o.side, o.quantity::numeric, o.limit_price::numeric,
	o.trade_price::numeric, o.reference_price::numeric, o.client_id, o.venue_order_id, o.status, o.filled::numeric,
	o.filled_quote::numeric, o.reservation_id::text, o.reason, o.created_at, o.uncertain_since, o.placed_at`

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// read reads an order and checks its seal: an order changed in SQL is
// rowseal.ErrUnverified.
func read(ctx context.Context, q rowQuerier, id string) (Order, error) {
	var o Order
	sealed := rowseal.OTCRoutedOrders.NewRow()
	err := q.QueryRowContext(ctx, `SELECT `+columns+`, `+rowseal.OTCRoutedOrders.Fields("o")+`
		FROM otc_routed_orders o WHERE o.id = $1::uuid`, id).Scan(append([]any{
		&o.ID, &o.TradeID, &o.Leg, &o.Attempt, &o.ParentID, &o.Venue, &o.Symbol, &o.BaseAsset, &o.QuoteAsset, &o.BaseHK,
		&o.QuoteHK, &o.Side, &o.Quantity, &o.Limit, &o.TradePrice, &o.Reference, &o.ClientID, &o.VenueOrderID, &o.Status,
		&o.Filled, &o.FilledQuote, &o.ReservationID, &o.Reason, &o.CreatedAt, &o.UncertainSince, &o.PlacedAt,
	}, sealed.Dest()...)...)
	if err != nil {
		return Order{}, fmt.Errorf("routed: read the order %s: %w", id, err)
	}
	if err := rowseal.Require(ctx, rowseal.OTCRoutedOrders, sealed); err != nil {
		return Order{}, err
	}
	return o, nil
}

// Read reads the order id, checked against its seal.
func Read(ctx context.Context, db *sql.DB, id string) (Order, error) { return read(ctx, db, id) }

// OfTrade returns the orders of a trade, in the order of their legs and
// attempts.
func OfTrade(ctx context.Context, db *sql.DB, tradeID string) ([]Order, error) {
	rows, err := db.QueryContext(ctx, `SELECT id::text FROM otc_routed_orders WHERE trade_id = $1::uuid ORDER BY leg, attempt`, tradeID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]Order, 0, len(ids))
	for _, id := range ids {
		o, err := read(ctx, db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// _ checks that a Driver is a job.
var _ jobs.Job = (*Driver)(nil)
