// Package state keeps the state of the external venues that cover the OTC
// trades (#385), in PostgreSQL so that every node sees the same: what we
// hold at each venue, its markets, and its networks, from the snapshots
// that a job of cmd/market-worker takes under a lease; the reservations of
// the balances that routed orders plan to spend; and the holds that a
// difference between a venue and the books puts on a currency of it.
//
// The router (#386) asks Routable before it plans an order through a venue,
// and Reserve before it sends one. See docs/venues.md.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/venues"
)

// The kinds of a snapshot, as venue_snapshots names them.
const (
	KindBalances       = "balances"
	KindInstruments    = "instruments"
	KindNetworks       = "networks"
	KindReconciliation = "reconciliation"
	KindFees           = "fees"
)

// The defaults of Options.
const (
	DefaultMaxAge      = 5 * time.Minute
	DefaultConfirmRuns = 2
)

// Errors of the reservations.
var (
	// ErrNotRoutable refuses a reservation of a currency that is not
	// routable at the venue; Routability says why.
	ErrNotRoutable = errors.New("venues: the currency is not routable at the venue")
	// ErrInsufficient refuses a reservation beyond the free balance less
	// the open reservations.
	ErrInsufficient = errors.New("venues: the venue balance does not cover the reservation")
	// ErrReferenceReused refuses a reservation whose reference holds
	// another amount.
	ErrReferenceReused = errors.New("venues: the reference already reserves another amount")
	// ErrNotFound is a reservation or a hold that does not exist.
	ErrNotFound = errors.New("venues: not found")
	// ErrAlreadyCleared is a hold that an operator already cleared.
	ErrAlreadyCleared = errors.New("venues: the hold is already cleared")
)

// Options configure a Store.
type Options struct {
	// MaxAge is how old a snapshot may be before its values count as
	// unknown; DefaultMaxAge when zero.
	MaxAge time.Duration
	// ConfirmRuns is how many reconciliations in a row must find a
	// difference before a hold opens; DefaultConfirmRuns when zero.
	ConfirmRuns int
	// Tolerances are the differences that a reconciliation accepts, by our
	// currency symbol; a currency that is not here accepts none.
	Tolerances map[string]shared.Decimal
}

// Store reads and writes the state of the venues.
type Store struct {
	DB     *sql.DB
	Ledger ledger.Ledger
	Opts   Options
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// New returns a Store with the defaults of opts filled in.
func New(db *sql.DB, opts Options) *Store {
	if opts.MaxAge <= 0 {
		opts.MaxAge = DefaultMaxAge
	}
	if opts.ConfirmRuns <= 0 {
		opts.ConfirmRuns = DefaultConfirmRuns
	}
	return &Store{DB: db, Ledger: ledger.Ledger{DB: db}, Opts: opts}
}

// Clock returns the time of now on the database's clock, which every node
// shares (#502): the snapshots, their freshness, and the ledger entries that
// the reconciliation compares them with are all on it. Now replaces it in
// the tests. Should the database not answer, the node's clock is used; the
// write that follows fails anyway.
func (s *Store) Clock(ctx context.Context) time.Time {
	if s.Now != nil {
		return s.Now()
	}
	var now time.Time
	if err := s.DB.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Now()
	}
	return now
}

// fresh reports whether a value taken at takenAt is still known at now.
func (s *Store) fresh(now, takenAt time.Time) bool { return now.Sub(takenAt) <= s.Opts.MaxAge }

// SaveBalances replaces the balances of venue with a snapshot taken at at.
// A balance that the venue no longer reports is zeroed, never deleted, as
// its reservations refer to it (#248).
func (s *Store) SaveBalances(ctx context.Context, venue string, balances []venues.Balance, at time.Time) error {
	return s.save(ctx, venue, KindBalances, at, func(tx *sql.Tx) error {
		assets := make([]string, 0, len(balances))
		for _, b := range balances {
			assets = append(assets, b.Asset)
			if _, err := tx.ExecContext(ctx, `INSERT INTO venue_balances (venue, asset, free, locked, taken_at)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (venue, asset) DO UPDATE SET free = EXCLUDED.free, locked = EXCLUDED.locked, taken_at = EXCLUDED.taken_at`,
				venue, b.Asset, b.Free.String(), b.Locked.String(), at); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE venue_balances SET free = 0, locked = 0, taken_at = $3
			WHERE venue = $1 AND NOT (asset = ANY ($2::text[]))`, venue, assets, at)
		return err
	})
}

// SaveInstruments replaces the markets of venue. A market that the venue
// no longer lists is kept, not trading.
func (s *Store) SaveInstruments(ctx context.Context, venue string, instruments []venues.Instrument, at time.Time) error {
	return s.save(ctx, venue, KindInstruments, at, func(tx *sql.Tx) error {
		symbols := make([]string, 0, len(instruments))
		for _, i := range instruments {
			symbols = append(symbols, i.Symbol)
			if _, err := tx.ExecContext(ctx, `INSERT INTO venue_instruments (venue, symbol, base, quote, trading, tick_size,
					lot_size, min_quantity, min_notional, taken_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
				ON CONFLICT (venue, symbol) DO UPDATE SET base = EXCLUDED.base, quote = EXCLUDED.quote, trading = EXCLUDED.trading,
					tick_size = EXCLUDED.tick_size, lot_size = EXCLUDED.lot_size, min_quantity = EXCLUDED.min_quantity,
					min_notional = EXCLUDED.min_notional, taken_at = EXCLUDED.taken_at`,
				venue, i.Symbol, i.Base, i.Quote, i.Trading, i.TickSize.String(), i.LotSize.String(),
				i.MinQuantity.String(), i.MinNotional.String(), at); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE venue_instruments SET trading = false, taken_at = $3
			WHERE venue = $1 AND NOT (symbol = ANY ($2::text[]))`, venue, symbols, at)
		return err
	})
}

// SaveNetworks replaces the networks of the assets of networks at venue. A
// network that the venue no longer reports for one of those assets is kept,
// with deposits and withdrawals off; the networks of other assets are left
// as they are.
func (s *Store) SaveNetworks(ctx context.Context, venue string, networks map[string][]venues.Network, at time.Time) error {
	return s.save(ctx, venue, KindNetworks, at, func(tx *sql.Tx) error {
		for asset, list := range networks {
			names := make([]string, 0, len(list))
			for _, n := range list {
				names = append(names, n.Network)
				if _, err := tx.ExecContext(ctx, `INSERT INTO venue_networks (venue, asset, network, deposit_enabled,
						withdraw_enabled, withdraw_fee, withdraw_min, taken_at)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
					ON CONFLICT (venue, asset, network) DO UPDATE SET deposit_enabled = EXCLUDED.deposit_enabled,
						withdraw_enabled = EXCLUDED.withdraw_enabled, withdraw_fee = EXCLUDED.withdraw_fee,
						withdraw_min = EXCLUDED.withdraw_min, taken_at = EXCLUDED.taken_at`,
					venue, asset, n.Network, n.DepositEnabled, n.WithdrawEnabled, n.WithdrawFee.String(), n.WithdrawMin.String(), at); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE venue_networks SET deposit_enabled = false, withdraw_enabled = false, taken_at = $4
				WHERE venue = $1 AND asset = $2 AND NOT (network = ANY ($3::text[]))`, venue, asset, names, at); err != nil {
				return err
			}
		}
		return nil
	})
}

// save runs write and records the snapshot of kind in one transaction.
func (s *Store) save(ctx context.Context, venue, kind string, at time.Time, write func(tx *sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("venues: save the %s of %s: %w", kind, venue, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := write(tx); err != nil {
		return fmt.Errorf("venues: save the %s of %s: %w", kind, venue, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO venue_snapshots (venue, kind, taken_at) VALUES ($1, $2, $3)
		ON CONFLICT (venue, kind) DO UPDATE SET taken_at = EXCLUDED.taken_at`, venue, kind, at); err != nil {
		return fmt.Errorf("venues: save the %s of %s: %w", kind, venue, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("venues: save the %s of %s: %w", kind, venue, err)
	}
	return nil
}

// RecordFailure records that a snapshot of kind failed, by the kind of its
// error only: the venue's text may carry an address or an amount.
func (s *Store) RecordFailure(ctx context.Context, venue, kind string, err error) error {
	_, werr := s.DB.ExecContext(ctx, `INSERT INTO venue_snapshots (venue, kind, failed_at, failure) VALUES ($1, $2, $3, $4)
		ON CONFLICT (venue, kind) DO UPDATE SET failed_at = EXCLUDED.failed_at, failure = EXCLUDED.failure`,
		venue, kind, s.Clock(ctx), string(venues.KindOf(err)))
	if werr != nil {
		return fmt.Errorf("venues: record the failure of the %s of %s: %w", kind, venue, werr)
	}
	return nil
}

// The reasons that a currency is not routable at a venue.
const (
	ReasonNoBalance        = "no_balance"
	ReasonStaleBalance     = "stale_balance"
	ReasonHold             = "reconciliation_hold"
	ReasonNetworkUnknown   = "network_unknown"
	ReasonNetworkStale     = "network_stale"
	ReasonNetworkSuspended = "network_suspended"
	ReasonMarketUnknown    = "market_unknown"
	ReasonMarketStale      = "market_stale"
	ReasonMarketHalted     = "market_halted"
)

// Routability says whether the router may plan through a currency of a
// venue, and why not.
type Routability struct {
	Routable bool     `json:"routable"`
	Reasons  []string `json:"reasons,omitempty"`
}

func (r *Routability) not(reason string) {
	r.Routable = false
	r.Reasons = append(r.Reasons, reason)
}

// Routable reports whether asset is routable at venue: its balance is from
// a fresh snapshot, and no reconciliation holds it. A non-empty network
// also needs the network fresh, with deposits and withdrawals on.
func (s *Store) Routable(ctx context.Context, venue, asset, network string) (Routability, error) {
	return s.routable(ctx, s.DB, venue, asset, network)
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *Store) routable(ctx context.Context, q querier, venue, asset, network string) (Routability, error) {
	r := Routability{Routable: true}
	var takenAt time.Time
	err := q.QueryRowContext(ctx, `SELECT taken_at FROM venue_balances WHERE venue = $1 AND asset = $2`, venue, asset).Scan(&takenAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		r.not(ReasonNoBalance)
	case err != nil:
		return r, fmt.Errorf("venues: read the balance of %s at %s: %w", asset, venue, err)
	case !s.fresh(s.Clock(ctx), takenAt):
		r.not(ReasonStaleBalance)
	}
	held, err := s.held(ctx, q, venue, asset)
	if err != nil {
		return r, err
	}
	if held {
		r.not(ReasonHold)
	}
	if network != "" {
		var deposit, withdraw bool
		err := q.QueryRowContext(ctx, `SELECT deposit_enabled, withdraw_enabled, taken_at FROM venue_networks
			WHERE venue = $1 AND asset = $2 AND network = $3`, venue, asset, network).Scan(&deposit, &withdraw, &takenAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			r.not(ReasonNetworkUnknown)
		case err != nil:
			return r, fmt.Errorf("venues: read the network %s of %s at %s: %w", network, asset, venue, err)
		case !s.fresh(s.Clock(ctx), takenAt):
			r.not(ReasonNetworkStale)
		case !deposit || !withdraw:
			r.not(ReasonNetworkSuspended)
		}
	}
	return r, nil
}

// MarketRoutable reports whether the market symbol of venue may take an
// order: listed in a fresh snapshot, and trading.
func (s *Store) MarketRoutable(ctx context.Context, venue, symbol string) (Routability, error) {
	r := Routability{Routable: true}
	var trading bool
	var takenAt time.Time
	err := s.DB.QueryRowContext(ctx, `SELECT trading, taken_at FROM venue_instruments WHERE venue = $1 AND symbol = $2`,
		venue, symbol).Scan(&trading, &takenAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		r.not(ReasonMarketUnknown)
	case err != nil:
		return r, fmt.Errorf("venues: read the market %s at %s: %w", symbol, venue, err)
	case !s.fresh(s.Clock(ctx), takenAt):
		r.not(ReasonMarketStale)
	case !trading:
		r.not(ReasonMarketHalted)
	}
	return r, nil
}

// held reports whether an open hold, checked against its seal, holds asset
// at venue. A hold cleared in SQL fails its seal and still holds.
func (s *Store) held(ctx context.Context, q querier, venue, asset string) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+rowseal.VenueHolds.Fields("")+` FROM venue_holds
		WHERE venue = $1 AND asset = $2 ORDER BY detected_at DESC LIMIT 20`, venue, asset)
	if err != nil {
		return false, fmt.Errorf("venues: read the holds of %s at %s: %w", asset, venue, err)
	}
	defer rows.Close()
	for rows.Next() {
		r := rowseal.VenueHolds.NewRow()
		if err := rows.Scan(r.Dest()...); err != nil {
			return false, fmt.Errorf("venues: read the holds of %s at %s: %w", asset, venue, err)
		}
		if err := rowseal.Check(ctx, rowseal.VenueHolds, r); err != nil {
			// A hold that fails its seal counts as open.
			return true, nil
		}
		if !r.Values[rowseal.VenueHolds.Index("cleared_at")].Valid {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Reservation is a reservation of a venue balance by a routed order.
type Reservation struct {
	ID            string         `json:"id"`
	Venue         string         `json:"venue"`
	Asset         string         `json:"asset"`
	Amount        shared.Decimal `json:"amount"`
	ReferenceType string         `json:"reference_type"`
	ReferenceID   string         `json:"reference_id"`
}

// Reserve reserves r.Amount of the balance of r.Asset at r.Venue for its
// reference, in its own transaction; see ReserveIn.
func (s *Store) Reserve(ctx context.Context, r Reservation) (Reservation, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Reservation{}, fmt.Errorf("venues: reserve: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	out, err := s.ReserveIn(ctx, tx, r)
	if err != nil {
		return Reservation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Reservation{}, fmt.Errorf("venues: reserve: %w", err)
	}
	return out, nil
}

// ReserveIn reserves in tx, so that the router can reserve with what else
// its plan writes. The row of the balance is locked first: two nodes that
// reserve the same balance at once take turns, and the second sees the
// reservation of the first. The balance must be routable (ErrNotRoutable),
// and its free amount less the open reservations must cover the amount
// (ErrInsufficient). A reference that already reserved the same amount
// returns that reservation; another amount is ErrReferenceReused.
func (s *Store) ReserveIn(ctx context.Context, tx *sql.Tx, r Reservation) (Reservation, error) {
	if !r.Amount.IsPositive() || r.ReferenceType == "" || r.ReferenceID == "" {
		return Reservation{}, fmt.Errorf("venues: a reservation needs a positive amount and a reference")
	}
	var free shared.Decimal
	err := tx.QueryRowContext(ctx, `SELECT free FROM venue_balances WHERE venue = $1 AND asset = $2 FOR UPDATE`,
		r.Venue, r.Asset).Scan(&free)
	if errors.Is(err, sql.ErrNoRows) {
		return Reservation{}, fmt.Errorf("%w: %s", ErrNotRoutable, ReasonNoBalance)
	}
	if err != nil {
		return Reservation{}, fmt.Errorf("venues: lock the balance of %s at %s: %w", r.Asset, r.Venue, err)
	}
	open, reserved, err := openReservations(ctx, tx, r.Venue, r.Asset)
	if err != nil {
		return Reservation{}, err
	}
	for _, o := range open {
		if o.ReferenceType == r.ReferenceType && o.ReferenceID == r.ReferenceID {
			if !o.Amount.Equal(r.Amount) {
				return Reservation{}, ErrReferenceReused
			}
			return o, nil
		}
	}
	routable, err := s.routable(ctx, tx, r.Venue, r.Asset, "")
	if err != nil {
		return Reservation{}, err
	}
	if !routable.Routable {
		return Reservation{}, fmt.Errorf("%w: %v", ErrNotRoutable, routable.Reasons)
	}
	if free.Sub(reserved).LessThan(r.Amount) {
		return Reservation{}, ErrInsufficient
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO venue_reservations (venue, asset, amount, reference_type, reference_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (venue, asset, reference_type, reference_id) DO NOTHING RETURNING id::text`,
		r.Venue, r.Asset, r.Amount.String(), r.ReferenceType, r.ReferenceID, s.Clock(ctx)).Scan(&r.ID)
	if errors.Is(err, sql.ErrNoRows) {
		// The reference reserved before and was released: it is not
		// reserved again.
		return Reservation{}, ErrReferenceReused
	}
	if err != nil {
		return Reservation{}, fmt.Errorf("venues: reserve %s at %s: %w", r.Asset, r.Venue, err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.VenueReservations, "id = $1", r.ID); err != nil {
		return Reservation{}, err
	}
	return r, nil
}

// openReservations returns the open reservations of a balance, each
// checked against its seal, and their sum. A reservation changed in SQL is
// refused: the balance cannot be planned on until it is reviewed.
func openReservations(ctx context.Context, q querier, venue, asset string) ([]Reservation, shared.Decimal, error) {
	sum := shared.Decimal{}
	rows, err := q.QueryContext(ctx, `SELECT `+rowseal.VenueReservations.Fields("")+` FROM venue_reservations
		WHERE venue = $1 AND asset = $2 AND released_at IS NULL`, venue, asset)
	if err != nil {
		return nil, sum, fmt.Errorf("venues: read the reservations of %s at %s: %w", asset, venue, err)
	}
	defer rows.Close()
	var out []Reservation
	t := rowseal.VenueReservations
	for rows.Next() {
		r := t.NewRow()
		if err := rows.Scan(r.Dest()...); err != nil {
			return nil, sum, fmt.Errorf("venues: read the reservations of %s at %s: %w", asset, venue, err)
		}
		if err := rowseal.Check(ctx, t, r); err != nil {
			return nil, sum, err
		}
		amount, err := shared.NewDecimalFromString(r.Values[t.Index("amount")].String)
		if err != nil {
			return nil, sum, fmt.Errorf("venues: read the reservations of %s at %s: %w", asset, venue, err)
		}
		sum = sum.Add(amount)
		out = append(out, Reservation{ID: r.Key, Venue: venue, Asset: asset, Amount: amount,
			ReferenceType: r.Values[t.Index("reference_type")].String, ReferenceID: r.Values[t.Index("reference_id")].String})
	}
	return out, sum, rows.Err()
}

// Release ends a reservation, once its order filled or failed: its amount
// is free to plan on again. A reservation already released is left as it
// is.
func (s *Store) Release(ctx context.Context, id string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("venues: release: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.ReleaseIn(ctx, tx, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("venues: release: %w", err)
	}
	return nil
}

// ReleaseIn is Release in tx.
func (s *Store) ReleaseIn(ctx context.Context, tx *sql.Tx, id string) error {
	t := rowseal.VenueReservations
	r := t.NewRow()
	err := tx.QueryRowContext(ctx, `SELECT `+t.Fields("")+` FROM venue_reservations WHERE id = $1::uuid FOR UPDATE`, id).Scan(r.Dest()...)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("venues: release %s: %w", id, err)
	}
	if err := rowseal.Check(ctx, t, r); err != nil {
		return err
	}
	if r.Values[t.Index("released_at")].Valid {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE venue_reservations SET released_at = $2 WHERE id = $1::uuid`, id, s.Clock(ctx)); err != nil {
		return fmt.Errorf("venues: release %s: %w", id, err)
	}
	return rowseal.Reseal(ctx, tx, t, "id = $1::uuid", id)
}

// Available returns the free balance of asset at venue less its open
// reservations: what the router may still plan on.
func (s *Store) Available(ctx context.Context, venue, asset string) (shared.Decimal, error) {
	var free shared.Decimal
	err := s.DB.QueryRowContext(ctx, `SELECT free FROM venue_balances WHERE venue = $1 AND asset = $2`, venue, asset).Scan(&free)
	if errors.Is(err, sql.ErrNoRows) {
		return shared.Decimal{}, nil
	}
	if err != nil {
		return shared.Decimal{}, fmt.Errorf("venues: read the balance of %s at %s: %w", asset, venue, err)
	}
	_, reserved, err := openReservations(ctx, s.DB, venue, asset)
	if err != nil {
		return shared.Decimal{}, err
	}
	return free.Sub(reserved), nil
}

// OptionsOf are the options of venues.state.
func OptionsOf(c config.VenueStateConfig) Options {
	tolerances := map[string]shared.Decimal{}
	for symbol, d := range c.VenueTolerances() {
		tolerances[symbol] = shared.Decimal(d)
	}
	return Options{MaxAge: time.Duration(c.MaxAgeSeconds) * time.Second, ConfirmRuns: c.ConfirmRuns, Tolerances: tolerances}
}

// MappingsOf are the asset mappings of the enabled venues of c, by name,
// which the admin overview and the metrics read without the connectors.
func MappingsOf(c config.VenuesConfig) map[string]venues.Mapping {
	out := map[string]venues.Mapping{}
	for _, name := range c.EnabledVenues() {
		m := venues.Mapping{}
		for asset, a := range c.Exchanges[name].Assets {
			m[asset] = venues.AssetMapping{Currency: a.Currency, Networks: a.Networks}
		}
		out[name] = m
	}
	return out
}

// Instrument is a market of a venue as its last snapshot lists it.
type Instrument struct {
	Symbol                              string
	Tick, Lot, MinQuantity, MinNotional shared.Decimal
}

// InstrumentOf returns the market of a venue whose base and quote are the
// venue's assets base and quote; ok is false when none is listed. The
// router (#386) checks it with MarketRoutable.
func (s *Store) InstrumentOf(ctx context.Context, venue, base, quote string) (Instrument, bool, error) {
	var in Instrument
	err := s.DB.QueryRowContext(ctx, `SELECT symbol, tick_size, lot_size, min_quantity, min_notional FROM venue_instruments
		WHERE venue = $1 AND base = $2 AND quote = $3 ORDER BY symbol LIMIT 1`, venue, base, quote).
		Scan(&in.Symbol, &in.Tick, &in.Lot, &in.MinQuantity, &in.MinNotional)
	if errors.Is(err, sql.ErrNoRows) {
		return Instrument{}, false, nil
	}
	if err != nil {
		return Instrument{}, false, fmt.Errorf("venues: read the market %s/%s at %s: %w", base, quote, venue, err)
	}
	return in, true, nil
}

// WithdrawFee returns the fee of a withdrawal of asset on network at
// venue, as its last snapshot of the networks has it; zero when it is not
// listed, which Routable refuses.
func (s *Store) WithdrawFee(ctx context.Context, venue, asset, network string) (shared.Decimal, error) {
	var fee shared.Decimal
	err := s.DB.QueryRowContext(ctx, `SELECT withdraw_fee FROM venue_networks WHERE venue = $1 AND asset = $2 AND network = $3`,
		venue, asset, network).Scan(&fee)
	if errors.Is(err, sql.ErrNoRows) {
		return shared.Decimal{}, nil
	}
	if err != nil {
		return shared.Decimal{}, fmt.Errorf("venues: read the network %s of %s at %s: %w", network, asset, venue, err)
	}
	return fee, nil
}
