package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/venues"
)

// Fee is a fee that a venue charged us, as it reports it.
type Fee struct {
	// ID is the venue's id of the fee, unique per venue: the fee is posted
	// once per id.
	ID     string
	Asset  string
	Amount shared.Decimal
}

// FeeReporter is a venue that reports the fees it charged apart from its
// fills, such as withdrawal and transfer fees. A venue whose fees are only
// in its balances reports none, and a fee then shows as a difference.
type FeeReporter interface {
	Fees(ctx context.Context) ([]Fee, error)
}

// Difference is the comparison of one currency of a venue with the books.
type Difference struct {
	Venue      string         `json:"venue"`
	Asset      string         `json:"asset"`
	Currency   string         `json:"currency"`
	CurrencyHK string         `json:"currency_hk"`
	Reported   shared.Decimal `json:"reported"`
	Booked     shared.Decimal `json:"booked"`
	Difference shared.Decimal `json:"difference"`
	// Beyond is true when the difference is beyond the tolerance of the
	// currency; MismatchRuns counts the runs in a row that found it so.
	Beyond       bool `json:"beyond"`
	MismatchRuns int  `json:"mismatch_runs"`
	// HoldOpened is true when this run opened a hold of the currency.
	HoldOpened bool `json:"hold_opened"`
}

// ReconcileReport is the outcome of a reconciliation of a venue.
type ReconcileReport struct {
	Venue       string       `json:"venue"`
	FeesPosted  int          `json:"fees_posted"`
	Differences []Difference `json:"differences"`
	// Skipped is true when the balances were not fresh, or a move of the
	// venue was booked after them: nothing was compared.
	Skipped bool `json:"skipped"`
}

// PostFees posts the fees that fees reports as venue_fee entries, each once
// by its id. A fee of an asset that is not mapped is logged and skipped:
// the reconciliation then finds it as a difference.
func (s *Store) PostFees(ctx context.Context, venue string, mapping venues.Mapping, list []Fee) (int, error) {
	posted := 0
	for _, f := range list {
		if !f.Amount.IsPositive() || f.ID == "" {
			continue
		}
		symbol, _, ok := mapping.Ours(f.Asset, "")
		if !ok {
			log.Printf("venues: a fee of %s in %s, which is not mapped, is not posted", venue, f.Asset)
			continue
		}
		hk, err := currencyHK(ctx, s.DB, symbol)
		if err != nil {
			return posted, err
		}
		res, err := s.Ledger.VenueFee(ctx, "venue_fee:"+venue+":"+f.ID, venue, ledger.Amount{Currency: hk, Value: f.Amount},
			ledger.Ref{Type: "venue_fee", ID: venue + ":" + f.ID})
		if err != nil {
			return posted, fmt.Errorf("venues: post the fee %s of %s: %w", f.ID, venue, err)
		}
		if !res.Replayed {
			posted++
		}
	}
	return posted, nil
}

// Reconcile compares what venue reported holding, in its last balances, with
// what the books say it holds, for every mapped currency of either. A
// difference beyond the tolerance of the currency, found by ConfirmRuns
// runs in a row, opens a hold: the currency is not routable at the venue
// until an operator clears it. A balance snapshot that is not fresh, or
// older than the last venue entry of the venue, is not compared, as a move
// booked after it is not in it.
//
// It posts nothing: the fees are posted by PostFees before the balances are
// taken.
func (s *Store) Reconcile(ctx context.Context, venue string, mapping venues.Mapping) (ReconcileReport, error) {
	report := ReconcileReport{Venue: venue}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return report, fmt.Errorf("venues: reconcile %s: %w", venue, err)
	}
	defer func() { _ = tx.Rollback() }()
	// One reconciliation of a venue at a time, across the nodes.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "venue_reconcile:"+venue); err != nil {
		return report, fmt.Errorf("venues: reconcile %s: %w", venue, err)
	}
	var takenAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT taken_at FROM venue_snapshots WHERE venue = $1 AND kind = $2`,
		venue, KindBalances).Scan(&takenAt); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return report, fmt.Errorf("venues: reconcile %s: %w", venue, err)
	}
	var lastEntry sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT max(e.created_at) FROM ledger_entries e
		WHERE EXISTS (SELECT 1 FROM ledger_postings p WHERE p.entry_id = e.id AND p.account = $1)`,
		ledger.VenueAccount(venue)).Scan(&lastEntry); err != nil {
		return report, fmt.Errorf("venues: reconcile %s: %w", venue, err)
	}
	if !takenAt.Valid || !s.fresh(s.Clock(ctx), takenAt.Time) || (lastEntry.Valid && lastEntry.Time.After(takenAt.Time)) {
		report.Skipped = true
		return report, nil
	}

	booked, err := s.Ledger.VenueHoldings(ctx, venue)
	if err != nil {
		return report, err
	}
	reported := map[string]shared.Decimal{}
	runs := map[string]int{}
	rows, err := tx.QueryContext(ctx, `SELECT asset, free::numeric + locked::numeric, mismatch_runs FROM venue_balances
		WHERE venue = $1 FOR UPDATE`, venue)
	if err != nil {
		return report, fmt.Errorf("venues: reconcile %s: %w", venue, err)
	}
	for rows.Next() {
		var asset string
		var total shared.Decimal
		var n int
		if err := rows.Scan(&asset, &total, &n); err != nil {
			rows.Close()
			return report, fmt.Errorf("venues: reconcile %s: %w", venue, err)
		}
		reported[asset], runs[asset] = total, n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, fmt.Errorf("venues: reconcile %s: %w", venue, err)
	}

	assets := make([]string, 0, len(mapping))
	for asset := range mapping {
		assets = append(assets, asset)
	}
	sort.Strings(assets)
	for _, asset := range assets {
		symbol, _, _ := mapping.Ours(asset, "")
		hk, err := currencyHK(ctx, tx, symbol)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return report, err
		}
		_, has := reported[asset]
		b, inBooks := booked[hk.String()]
		if !has && !inBooks {
			continue
		}
		d := Difference{Venue: venue, Asset: asset, Currency: symbol, CurrencyHK: hk.String(),
			Reported: reported[asset], Booked: b, Difference: reported[asset].Sub(b)}
		tolerance := s.Opts.Tolerances[symbol]
		d.Beyond = d.Difference.Abs().GreaterThan(tolerance)
		if d.Beyond {
			d.MismatchRuns = runs[asset] + 1
		}
		if has {
			if _, err := tx.ExecContext(ctx, `UPDATE venue_balances SET mismatch_runs = $3 WHERE venue = $1 AND asset = $2`,
				venue, asset, d.MismatchRuns); err != nil {
				return report, fmt.Errorf("venues: reconcile %s: %w", venue, err)
			}
		} else if d.Beyond {
			// The books hold what the venue does not report at all: the
			// difference counts at once, as there is no row to count runs.
			d.MismatchRuns = s.Opts.ConfirmRuns
		}
		if d.Beyond && d.MismatchRuns >= s.Opts.ConfirmRuns {
			opened, err := s.openHold(ctx, tx, d)
			if err != nil {
				return report, err
			}
			d.HoldOpened = opened
		}
		report.Differences = append(report.Differences, d)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO venue_snapshots (venue, kind, taken_at) VALUES ($1, $2, $3)
		ON CONFLICT (venue, kind) DO UPDATE SET taken_at = EXCLUDED.taken_at`, venue, KindReconciliation, s.Clock(ctx)); err != nil {
		return report, fmt.Errorf("venues: reconcile %s: %w", venue, err)
	}
	if err := tx.Commit(); err != nil {
		return report, fmt.Errorf("venues: reconcile %s: %w", venue, err)
	}
	for _, d := range report.Differences {
		if d.HoldOpened {
			log.Printf("SECURITY: venue %s reports %s %s and the books %s: %s is not routable there until an operator clears its hold",
				venue, d.Reported, d.Asset, d.Booked, d.Currency)
		}
	}
	return report, nil
}

// openHold opens a hold of the currency of d, unless one is open; it
// reports whether it opened one.
func (s *Store) openHold(ctx context.Context, tx *sql.Tx, d Difference) (bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, `INSERT INTO venue_holds (venue, asset, currency_hk, reported, booked, difference, detected_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (venue, asset) WHERE cleared_at IS NULL DO NOTHING RETURNING id::text`,
		d.Venue, d.Asset, d.CurrencyHK, d.Reported.String(), d.Booked.String(), d.Difference.String(), s.Clock(ctx)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("venues: open a hold of %s at %s: %w", d.Asset, d.Venue, err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.VenueHolds, "id = $1::uuid", id); err != nil {
		return false, err
	}
	return true, nil
}

// Hold is a hold of a currency of a venue.
type Hold struct {
	ID          string         `json:"id"`
	Venue       string         `json:"venue"`
	Asset       string         `json:"asset"`
	CurrencyHK  string         `json:"currency_hk"`
	Reported    shared.Decimal `json:"reported"`
	Booked      shared.Decimal `json:"booked"`
	Difference  shared.Decimal `json:"difference"`
	DetectedAt  time.Time      `json:"detected_at"`
	ClearedAt   *time.Time     `json:"cleared_at,omitempty"`
	ClearedBy   string         `json:"cleared_by,omitempty"`
	ClearReason string         `json:"clear_reason,omitempty"`
}

// ClearHold clears a hold after an operator reviewed its difference, with
// the reason, audited as admin.venue_hold_clear in the same transaction.
// The reconciliation opens a new one if the difference stays.
func (s *Store) ClearHold(ctx context.Context, id string, origin audit.Event, reason string) (Hold, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Hold{}, fmt.Errorf("venues: clear the hold %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	t := rowseal.VenueHolds
	r := t.NewRow()
	err = tx.QueryRowContext(ctx, `SELECT `+t.Fields("")+` FROM venue_holds WHERE id = $1::uuid FOR UPDATE`, id).Scan(r.Dest()...)
	if errors.Is(err, sql.ErrNoRows) {
		return Hold{}, ErrNotFound
	}
	if err != nil {
		return Hold{}, fmt.Errorf("venues: clear the hold %s: %w", id, err)
	}
	if err := rowseal.Check(ctx, t, r); err != nil {
		return Hold{}, err
	}
	if r.Values[t.Index("cleared_at")].Valid {
		return Hold{}, ErrAlreadyCleared
	}
	if _, err := tx.ExecContext(ctx, `UPDATE venue_holds SET cleared_at = $2, cleared_by = $3::uuid, clear_reason = $4 WHERE id = $1::uuid`,
		id, s.Clock(ctx), origin.ActorID, reason); err != nil {
		return Hold{}, fmt.Errorf("venues: clear the hold %s: %w", id, err)
	}
	if err := rowseal.Reseal(ctx, tx, t, "id = $1::uuid", id); err != nil {
		return Hold{}, err
	}
	h, err := readHold(ctx, tx, id)
	if err != nil {
		return Hold{}, err
	}
	ev := audit.Of(origin, audit.ActionAdminVenueHoldClear, audit.TargetVenueHold, id)
	ev.Changes = &audit.Changes{
		Before: map[string]interface{}{"cleared": false},
		After: map[string]interface{}{"cleared": true, "venue": h.Venue, "asset": h.Asset,
			"difference": h.Difference.String(), "reason": reason},
	}
	if err := audit.Record(ctx, tx, ev); err != nil {
		return Hold{}, err
	}
	if err := tx.Commit(); err != nil {
		return Hold{}, fmt.Errorf("venues: clear the hold %s: %w", id, err)
	}
	return h, nil
}

func readHold(ctx context.Context, q querier, id string) (Hold, error) {
	var h Hold
	var clearedBy, reason sql.NullString
	err := q.QueryRowContext(ctx, `SELECT id::text, venue, asset, currency_hk::text, reported, booked, difference, detected_at,
			cleared_at, cleared_by::text, clear_reason FROM venue_holds WHERE id = $1::uuid`, id).Scan(
		&h.ID, &h.Venue, &h.Asset, &h.CurrencyHK, &h.Reported, &h.Booked, &h.Difference, &h.DetectedAt, &h.ClearedAt, &clearedBy, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	if err != nil {
		return h, fmt.Errorf("venues: read the hold %s: %w", id, err)
	}
	h.ClearedBy, h.ClearReason = clearedBy.String, reason.String
	return h, nil
}

// currencyHK returns the hk of our approved currency symbol; ErrNotFound
// when there is none.
func currencyHK(ctx context.Context, q querier, symbol string) (shared.UUID, error) {
	var hk shared.UUID
	err := q.QueryRowContext(ctx, `SELECT hk FROM currencies WHERE info->>'symbol' = $1 AND status = 'approved' AND deleted_at IS NULL`,
		symbol).Scan(&hk)
	if errors.Is(err, sql.ErrNoRows) {
		return hk, fmt.Errorf("%w: currency %s", ErrNotFound, symbol)
	}
	if err != nil {
		return hk, fmt.Errorf("venues: read the currency %s: %w", symbol, err)
	}
	return hk, nil
}
