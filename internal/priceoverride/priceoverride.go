// Package priceoverride keeps the prices that administrators set for a
// market until a time, in place of the aggregated reference of the price
// engine (#59): for when the providers are wrong, or down, and a market
// must keep or stop a price that an administrator chooses.
//
// The overrides are rows of PostgreSQL, sealed from the first one (#139),
// and audited. Every instance of the server keeps the overrides in force in
// memory through a Watcher, which reads them every second, so that a new
// one applies on every instance within it, and the readers of the prices
// (price.Reader) take one without a query. An override expires by itself;
// an administrator may end it before.
//
// An override whose mid is within the bound of the live reference mid
// applies at once. One beyond it, or for a market without a live
// reference, is pending (#453): it is served only once a second
// administrator approves it, before approve_by, and lapses otherwise. A
// delisting records the price of an override as its settlement price only
// when it was approved so.
package priceoverride

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/price"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/shopspring/decimal"
)

// The errors of the store.
var (
	// ErrExists is a market that has an override in force already: end it
	// first.
	ErrExists = errors.New("priceoverride: the market has an override in force")
	// ErrNotFound is an override that does not exist.
	ErrNotFound = errors.New("priceoverride: no such override")
	// ErrEnded is an override that has ended or expired.
	ErrEnded = errors.New("priceoverride: the override has ended")
	// ErrInvalid is an override whose prices or expiry are not valid.
	ErrInvalid = errors.New("priceoverride: invalid override")
	// ErrNotPending is an approval of an override that does not wait for
	// one.
	ErrNotPending = errors.New("priceoverride: the override does not wait for an approval")
	// ErrLapsed is an approval of an override after its approve_by.
	ErrLapsed = errors.New("priceoverride: the override lapsed without an approval")
	// ErrSameAdmin is an approval by the administrator who set it.
	ErrSameAdmin = errors.New("priceoverride: an override is approved by another administrator")
)

// Override is a price that an administrator set.
type Override struct {
	ID     string         `json:"id" format:"uuid"`
	Market string         `json:"market" example:"BTC/USDT"`
	Bid    shared.Decimal `json:"bid" swaggertype:"string" example:"64000"`
	Ask    shared.Decimal `json:"ask" swaggertype:"string" example:"64100"`
	Mid    shared.Decimal `json:"mid" swaggertype:"string" example:"64050"`
	Reason string         `json:"reason"`
	// CreatedBy and EndedBy are the administrators' hk.
	CreatedBy string     `json:"created_by" format:"uuid"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	EndedBy   string     `json:"ended_by,omitempty" format:"uuid"`
	// InForce is whether it is served now.
	InForce bool `json:"in_force"`
	// ReferenceMid is the live reference mid when it was set, and
	// DeviationBPS how far its mid is from it, in basis points; both are
	// absent without a live reference.
	ReferenceMid *shared.Decimal `json:"reference_mid,omitempty" swaggertype:"string" example:"64000"`
	DeviationBPS *int64          `json:"deviation_bps,omitempty" example:"8"`
	// Pending is an override that waits for a second administrator until
	// ApproveBy (#453), and Lapsed one that nobody approved by then.
	// ApprovedBy and ApprovedAt record the approval.
	Pending    bool       `json:"pending"`
	Lapsed     bool       `json:"lapsed,omitempty"`
	ApproveBy  *time.Time `json:"approve_by,omitempty"`
	ApprovedBy string     `json:"approved_by,omitempty" format:"uuid"`
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
}

// New is an override to set.
type New struct {
	Market        string
	Bid, Ask, Mid shared.Decimal
	Reason        string
	// For is how long it lasts.
	For time.Duration
	// Reference is the live reference mid of the market, nil without one:
	// the price engine's, read when no override is in force.
	Reference *shared.Decimal
}

// MaxFor bounds how long an override lasts: an administrator sets a new one
// for longer, so that a forgotten one ends.
const MaxFor = 24 * time.Hour

// MaxList bounds a list.
const MaxList = 100

// The defaults of a Store.
const (
	DefaultMaxDeviationBPS = 300
	DefaultApprovalWindow  = 30 * time.Minute
)

// Store keeps the overrides.
type Store struct {
	DB *sql.DB
	// MaxDeviationBPS bounds the mid of an override that applies at once,
	// against the live reference mid; ApprovalWindow is how long one
	// beyond it waits for its approval. Zero is the default.
	MaxDeviationBPS int
	ApprovalWindow  time.Duration
}

func (s Store) bound() int64 {
	if s.MaxDeviationBPS <= 0 {
		return DefaultMaxDeviationBPS
	}
	return int64(s.MaxDeviationBPS)
}

func (s Store) window() time.Duration {
	if s.ApprovalWindow <= 0 {
		return DefaultApprovalWindow
	}
	return s.ApprovalWindow
}

// Deviation returns how far mid is from reference, in basis points,
// rounded up.
func Deviation(mid, reference shared.Decimal) int64 {
	bps := mid.Decimal().Sub(reference.Decimal()).Abs().Mul(decimal.NewFromInt(10000)).Div(reference.Decimal()).Ceil()
	return bps.IntPart()
}

const columns = `id::text, market, bid::text, ask::text, mid::text, reason, created_by::text, created_at, expires_at,
	ended_at, COALESCE(ended_by::text, ''), now(), reference_mid::text, approve_by, COALESCE(approved_by::text, ''), approved_at`

// served is the condition of an override that is served: not ended, not
// expired, and approved when it waits for an approval.
const served = `ended_at IS NULL AND expires_at > now() AND (approve_by IS NULL OR approved_at IS NOT NULL)`

func scan(scanner func(...any) error) (Override, *rowseal.Row, error) {
	var o Override
	var bid, ask, mid string
	var ended, approveBy, approvedAt sql.NullTime
	var reference sql.NullString
	var now time.Time
	row := rowseal.PriceOverrides.NewRow()
	if err := scanner(append([]any{&o.ID, &o.Market, &bid, &ask, &mid, &o.Reason, &o.CreatedBy, &o.CreatedAt, &o.ExpiresAt,
		&ended, &o.EndedBy, &now, &reference, &approveBy, &o.ApprovedBy, &approvedAt}, row.Dest()...)...); err != nil {
		return Override{}, nil, err
	}
	var err error
	if o.Bid, err = shared.NewDecimalFromString(bid); err != nil {
		return Override{}, nil, err
	}
	if o.Ask, err = shared.NewDecimalFromString(ask); err != nil {
		return Override{}, nil, err
	}
	if o.Mid, err = shared.NewDecimalFromString(mid); err != nil {
		return Override{}, nil, err
	}
	o.CreatedAt, o.ExpiresAt = o.CreatedAt.UTC(), o.ExpiresAt.UTC()
	if ended.Valid {
		t := ended.Time.UTC()
		o.EndedAt = &t
	}
	if reference.Valid {
		ref, err := shared.NewDecimalFromString(reference.String)
		if err != nil {
			return Override{}, nil, err
		}
		dev := Deviation(o.Mid, ref)
		o.ReferenceMid, o.DeviationBPS = &ref, &dev
	}
	if approveBy.Valid {
		t := approveBy.Time.UTC()
		o.ApproveBy = &t
	}
	if approvedAt.Valid {
		t := approvedAt.Time.UTC()
		o.ApprovedAt = &t
	}
	open := o.EndedAt == nil && now.Before(o.ExpiresAt)
	waiting := o.ApproveBy != nil && o.ApprovedAt == nil
	o.Pending = open && waiting && now.Before(*o.ApproveBy)
	o.Lapsed = waiting && !now.Before(*o.ApproveBy)
	o.InForce = open && !waiting
	return o, row, nil
}

// Validate checks an override to set.
func Validate(n New) error {
	switch {
	case !n.Bid.IsPositive():
		return fmt.Errorf("%w: the bid must be above 0", ErrInvalid)
	case n.Bid.GreaterThan(n.Mid) || n.Mid.GreaterThan(n.Ask):
		return fmt.Errorf("%w: the prices must be bid <= mid <= ask", ErrInvalid)
	case n.For < time.Second || n.For > MaxFor:
		return fmt.Errorf("%w: it must last from a second to %s", ErrInvalid, MaxFor)
	case len(n.Reason) == 0 || len(n.Reason) > 500:
		return fmt.Errorf("%w: the reason must be 1 to 500 characters", ErrInvalid)
	}
	if _, err := price.NormalizeMarket(n.Market); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}

// Set records an override of the administrator adminHK, with origin, the
// audit event of the request. A market has one override in force at most:
// one that expired is closed first; one in force answers ErrExists.
func (s Store) Set(ctx context.Context, n New, adminHK string, origin audit.Event) (Override, error) {
	if err := Validate(n); err != nil {
		return Override{}, err
	}
	n.Market, _ = price.NormalizeMarket(n.Market)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Override{}, err
	}
	defer tx.Rollback()
	// Close the override that expired, at its expiry, or that lapsed
	// without its approval, at its approve_by, so that the market takes a
	// new one.
	rows, err := tx.QueryContext(ctx, `UPDATE price_overrides
		SET ended_at = CASE WHEN approve_by IS NOT NULL AND approved_at IS NULL THEN LEAST(expires_at, approve_by) ELSE expires_at END,
			row_version = row_version + 1
		WHERE market = $1 AND ended_at IS NULL
		  AND (expires_at <= now() OR (approve_by IS NOT NULL AND approved_at IS NULL AND approve_by <= now()))
		RETURNING id`, n.Market)
	if err != nil {
		return Override{}, err
	}
	var expired []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Override{}, err
		}
		expired = append(expired, id)
	}
	rows.Close()
	for _, id := range expired {
		if err := rowseal.Reseal(ctx, tx, rowseal.PriceOverrides, "id = $1", id); err != nil {
			return Override{}, err
		}
	}
	// Within the bound of the live reference, it applies at once; beyond
	// it, or without a reference, it waits for a second administrator.
	var reference any
	pending := n.Reference == nil || !n.Reference.IsPositive()
	if !pending {
		reference = n.Reference.String()
		pending = Deviation(n.Mid, *n.Reference) > s.bound()
	}
	var approveBy any
	if pending {
		approveBy = s.window().Seconds()
	}
	var id string
	err = tx.QueryRowContext(ctx, `INSERT INTO price_overrides (market, bid, ask, mid, reason, created_by, expires_at, reference_mid, approve_by)
		VALUES ($1, $2, $3, $4, $5, $6, now() + make_interval(secs => $7), $8::numeric,
			CASE WHEN $9::float8 IS NULL THEN NULL ELSE now() + make_interval(secs => $9::float8) END) RETURNING id::text`,
		n.Market, n.Bid.String(), n.Ask.String(), n.Mid.String(), n.Reason, adminHK, n.For.Seconds(), reference, approveBy).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Override{}, ErrExists
	}
	if err != nil {
		return Override{}, err
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.PriceOverrides, "id = $1", id); err != nil {
		return Override{}, err
	}
	o, err := get(ctx, tx, id, false)
	if err != nil {
		return Override{}, err
	}
	event := origin
	event.ActorType, event.ActorID = audit.ActorAdmin, adminHK
	event.Action, event.TargetType, event.TargetID = audit.ActionPriceOverrideSet, audit.TargetPriceOverride, id
	if pending {
		event.Action = audit.ActionPriceOverrideRequest
	}
	event.Outcome = audit.Success
	after := map[string]any{
		"market": o.Market, "bid": o.Bid.String(), "ask": o.Ask.String(), "mid": o.Mid.String(),
		"expires_at": o.ExpiresAt, "reason": o.Reason, "pending": pending,
	}
	if o.ReferenceMid != nil {
		after["reference_mid"], after["deviation_bps"] = o.ReferenceMid.String(), *o.DeviationBPS
	}
	if o.ApproveBy != nil {
		after["approve_by"] = *o.ApproveBy
	}
	event.Changes = &audit.Changes{After: after}
	if err := audit.Record(ctx, tx, event); err != nil {
		return Override{}, err
	}
	return o, tx.Commit()
}

// End ends an override in force before its expiry.
func (s Store) End(ctx context.Context, id, adminHK string, origin audit.Event) (Override, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Override{}, err
	}
	defer tx.Rollback()
	o, err := get(ctx, tx, id, true)
	if err != nil {
		return Override{}, err
	}
	if !o.InForce && !o.Pending {
		return Override{}, ErrEnded
	}
	if _, err := tx.ExecContext(ctx, `UPDATE price_overrides SET ended_at = now(), ended_by = $2, row_version = row_version + 1
		WHERE id = $1`, id, adminHK); err != nil {
		return Override{}, err
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.PriceOverrides, "id = $1", id); err != nil {
		return Override{}, err
	}
	ended, err := get(ctx, tx, id, false)
	if err != nil {
		return Override{}, err
	}
	event := origin
	event.ActorType, event.ActorID = audit.ActorAdmin, adminHK
	event.Action, event.TargetType, event.TargetID = audit.ActionPriceOverrideEnd, audit.TargetPriceOverride, id
	event.Outcome = audit.Success
	event.Changes = &audit.Changes{
		Before: map[string]any{"market": o.Market, "expires_at": o.ExpiresAt},
		After:  map[string]any{"ended_at": ended.EndedAt},
	}
	if err := audit.Record(ctx, tx, event); err != nil {
		return Override{}, err
	}
	return ended, tx.Commit()
}

// Approve approves a pending override as the administrator adminHK, who
// is not the one who set it (#453): it is then served, until its expiry.
func (s Store) Approve(ctx context.Context, id, adminHK string, origin audit.Event) (Override, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Override{}, err
	}
	defer tx.Rollback()
	o, err := get(ctx, tx, id, true)
	if err != nil {
		return Override{}, err
	}
	switch {
	case o.Lapsed:
		return Override{}, ErrLapsed
	case !o.Pending && !o.InForce:
		return Override{}, ErrEnded
	case !o.Pending:
		return Override{}, ErrNotPending
	case o.CreatedBy == adminHK:
		return Override{}, ErrSameAdmin
	}
	if _, err := tx.ExecContext(ctx, `UPDATE price_overrides SET approved_by = $2, approved_at = now(), row_version = row_version + 1
		WHERE id = $1 AND approved_at IS NULL AND approve_by > now()`, id, adminHK); err != nil {
		return Override{}, err
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.PriceOverrides, "id = $1", id); err != nil {
		return Override{}, err
	}
	approved, err := get(ctx, tx, id, false)
	if err != nil {
		return Override{}, err
	}
	if approved.ApprovedAt == nil {
		// It lapsed between the read and the write.
		return Override{}, ErrLapsed
	}
	event := origin
	event.ActorType, event.ActorID = audit.ActorAdmin, adminHK
	event.Action, event.TargetType, event.TargetID = audit.ActionPriceOverrideApprove, audit.TargetPriceOverride, id
	event.Outcome = audit.Success
	event.Changes = &audit.Changes{
		Before: map[string]any{"market": o.Market, "mid": o.Mid.String(), "created_by": o.CreatedBy},
		After:  map[string]any{"approved_by": adminHK, "approved_at": approved.ApprovedAt},
	}
	if err := audit.Record(ctx, tx, event); err != nil {
		return Override{}, err
	}
	return approved, tx.Commit()
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// get reads an override and checks its seal; lock locks it.
func get(ctx context.Context, q querier, id string, lock bool) (Override, error) {
	query := `SELECT ` + columns + `, ` + rowseal.PriceOverrides.Fields("") + ` FROM price_overrides WHERE id = $1::uuid`
	if lock {
		query += ` FOR UPDATE`
	}
	o, row, err := scan(q.QueryRowContext(ctx, query, id).Scan)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return Override{}, ErrNotFound
	}
	if err != nil {
		return Override{}, err
	}
	if err := rowseal.Check(ctx, rowseal.PriceOverrides, row); err != nil {
		return Override{}, err
	}
	return o, nil
}

func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// List returns the overrides, newest first: those in force only, or the
// last limit of every state.
func (s Store) List(ctx context.Context, inForce bool, limit int) ([]Override, error) {
	if limit <= 0 || limit > MaxList {
		limit = MaxList
	}
	where := ""
	if inForce {
		where = "WHERE " + served
	}
	//nolint:gosec // G202: the columns and the condition are constants of this package.
	rows, err := s.DB.QueryContext(ctx, `SELECT `+columns+`, `+rowseal.PriceOverrides.Fields("")+` FROM price_overrides `+where+`
		ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Override{}
	for rows.Next() {
		o, row, err := scan(rows.Scan)
		if err != nil {
			return nil, err
		}
		if err := rowseal.Check(ctx, rowseal.PriceOverrides, row); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// inForce returns the overrides in force, leaving out, and reporting, those
// that fail their seal.
func (s Store) inForce(ctx context.Context) ([]Override, error) {
	//nolint:gosec // G202: the columns are constants of this package.
	rows, err := s.DB.QueryContext(ctx, `SELECT `+columns+`, `+rowseal.PriceOverrides.Fields("")+` FROM price_overrides
		WHERE `+served)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Override
	for rows.Next() {
		o, row, err := scan(rows.Scan)
		if err != nil {
			return nil, err
		}
		if rowseal.Check(ctx, rowseal.PriceOverrides, row) != nil {
			continue
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// DefaultPoll is how often a Watcher reads the overrides.
const DefaultPoll = time.Second

// Watcher keeps the overrides in force in memory on one instance. While it
// cannot read them, it keeps those it read last, each until its expiry.
type Watcher struct {
	store  Store
	poll   time.Duration
	logger *log.Logger

	mu       sync.RWMutex
	byMarket map[string]price.Override
	failing  bool
}

var _ price.Overrides = (*Watcher)(nil)

// NewWatcher returns a watcher of the overrides of db.
func NewWatcher(db *sql.DB, poll time.Duration, logger *log.Logger) *Watcher {
	if poll <= 0 {
		poll = DefaultPoll
	}
	if logger == nil {
		logger = log.Default()
	}
	return &Watcher{store: Store{DB: db}, poll: poll, logger: logger, byMarket: map[string]price.Override{}}
}

// Active implements price.Overrides.
func (w *Watcher) Active(market string, now time.Time) (price.Override, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	o, ok := w.byMarket[market]
	return o, ok && now.Before(o.Expires)
}

// Refresh reads the overrides now.
func (w *Watcher) Refresh(ctx context.Context) error {
	list, err := w.store.inForce(ctx)
	if err != nil {
		return err
	}
	byMarket := make(map[string]price.Override, len(list))
	for _, o := range list {
		byMarket[o.Market] = price.Override{ID: o.ID, Market: o.Market, Bid: o.Bid, Ask: o.Ask, Mid: o.Mid, Set: o.CreatedAt,
			Expires: o.ExpiresAt, Approved: o.ApprovedBy != ""}
	}
	w.mu.Lock()
	w.byMarket = byMarket
	w.mu.Unlock()
	return nil
}

// Run reads the overrides every poll until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.poll)
	defer ticker.Stop()
	for {
		readCtx, cancel := context.WithTimeout(ctx, max(w.poll, time.Second))
		err := w.Refresh(readCtx)
		cancel()
		switch {
		case err != nil && ctx.Err() == nil && !w.failing:
			w.failing = true
			w.logger.Printf("price overrides: reading them failed; keeping the last ones until they expire: %v", err)
		case err == nil && w.failing:
			w.failing = false
			w.logger.Printf("price overrides: read again")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
