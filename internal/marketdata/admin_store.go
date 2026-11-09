package marketdata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/audit"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// The administration of the catalog (#270), for the admin routes and
// cmd/admin market-data. Each change is written with its audit event in one
// transaction, so that a change cannot happen without its event. A change
// that would change nothing writes no event and reports the row as it is.

// Errors of the administration.
var (
	// ErrAssetLinked refuses to hide a linked asset, or to link it to
	// another currency: unlink it first.
	ErrAssetLinked = errors.New("marketdata: the asset is linked to a listed currency")
	// ErrAssetHidden refuses to link a hidden asset: show it first.
	ErrAssetHidden = errors.New("marketdata: the asset is hidden")
	// ErrAssetNotLinked refuses to unlink an asset that is not linked.
	ErrAssetNotLinked = errors.New("marketdata: the asset is not linked")
	// ErrCurrencyNotFound refuses a link to a currency that does not
	// exist, or was deleted.
	ErrCurrencyNotFound = errors.New("marketdata: the currency does not exist")
	// ErrCurrencyLinked refuses a link to a currency that another asset is
	// linked to.
	ErrCurrencyLinked = errors.New("marketdata: another asset is linked to the currency")
	// ErrConflictResolved refuses to resolve a conflict twice.
	ErrConflictResolved = errors.New("marketdata: the conflict is already resolved")
	// ErrJobRunning refuses to run a job now while a worker runs it.
	ErrJobRunning = errors.New("marketdata: a worker is running the job")
	// ErrInvalid is a value that the administration refuses, such as a
	// note that is too long.
	ErrInvalid = errors.New("marketdata: invalid value")
)

// maxNote bounds the reasons and notes of the administrators.
const maxNote = 1000

// AdminPageLimit bounds the rows of one read of an admin list: a page of
// 200, and one more that tells whether a next page exists.
const AdminPageLimit = 201

func checkNote(note string, required bool) (string, error) {
	note = strings.TrimSpace(note)
	if required && note == "" {
		return "", fmt.Errorf("%w: a reason is required", ErrInvalid)
	}
	if utf8.RuneCountInString(note) > maxNote || strings.ContainsRune(note, 0) {
		return "", fmt.Errorf("%w: the reason has more than %d characters", ErrInvalid, maxNote)
	}
	return note, nil
}

func pageLimit(limit int) int {
	switch {
	case limit <= 0:
		return 50
	case limit > AdminPageLimit:
		return AdminPageLimit
	}
	return limit
}

// targetID keeps a target id within the 64 characters of the audit log;
// the event holds the whole name.
func targetID(id string) string {
	if len(id) > 64 {
		return id[:64]
	}
	return id
}

// inTx runs fn in a transaction, which it commits when fn succeeds.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func record(ctx context.Context, tx *sql.Tx, origin audit.Event, action, target, id string, before, after map[string]any) error {
	e := audit.Of(origin, action, target, targetID(id))
	e.Changes = &audit.Changes{Before: before, After: after}
	return audit.Record(ctx, tx, e)
}

// Sources and jobs.

// SourceControl is an administrator's pause of a source.
type SourceControl struct {
	Source    string
	Paused    bool
	Reason    string
	UpdatedAt *time.Time
}

// JobState is the state of a job: its lease and its last runs.
type JobState struct {
	Job string
	// Holder is the node that runs it now, when Running.
	Holder         string
	Running        bool
	NextRunAt      time.Time
	Failures       int
	LastStartedAt  *time.Time
	LastFinishedAt *time.Time
	LastSuccessAt  *time.Time
}

// SourceOf is the source of a job, the second part of its name, such as
// binance for tickers:binance or candles:binance:1h; empty for a job of no
// source, such as tiers.
func SourceOf(job string) string {
	parts := strings.Split(job, ":")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// SourceUsage is the requests made to a source in the current minute and
// month, across every worker.
type SourceUsage struct {
	Minute int
	Month  int
}

// SourceControls returns the pauses of the sources, by source.
func (s *Store) SourceControls(ctx context.Context) (map[string]SourceControl, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source, paused, reason, updated_at FROM market_source_controls`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SourceControl{}
	for rows.Next() {
		var c SourceControl
		var at time.Time
		if err := rows.Scan(&c.Source, &c.Paused, &c.Reason, &at); err != nil {
			return nil, err
		}
		c.UpdatedAt = &at
		out[c.Source] = c
	}
	return out, rows.Err()
}

// SourceUsages returns the requests of the current windows, by source. A
// window that has passed counts nothing.
func (s *Store) SourceUsages(ctx context.Context) (map[string]SourceUsage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source, window_kind, requests FROM market_source_usage
		WHERE window_start = date_trunc(window_kind, NOW() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SourceUsage{}
	for rows.Next() {
		var source, kind string
		var n int
		if err := rows.Scan(&source, &kind, &n); err != nil {
			return nil, err
		}
		u := out[source]
		if kind == "minute" {
			u.Minute = n
		} else {
			u.Month = n
		}
		out[source] = u
	}
	return out, rows.Err()
}

// JobStates returns the state of every job that a worker has run or
// claimed, ordered by name.
func (s *Store) JobStates(ctx context.Context) ([]JobState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT job, holder, lease_until > NOW(), next_run_at, failures, last_started_at, last_finished_at, last_success_at
		FROM market_job_leases ORDER BY job`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobState
	for rows.Next() {
		var j JobState
		if err := rows.Scan(&j.Job, &j.Holder, &j.Running, &j.NextRunAt, &j.Failures, &j.LastStartedAt,
			&j.LastFinishedAt, &j.LastSuccessAt); err != nil {
			return nil, err
		}
		if !j.Running {
			j.Holder = ""
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// SetSourcePaused pauses or resumes a source. Every worker stops claiming
// its jobs at once, and a run in progress stops at its next batch of
// writes. A pause needs a reason.
func (s *Store) SetSourcePaused(ctx context.Context, source string, paused bool, reason string, origin audit.Event) (SourceControl, error) {
	if !sourcePattern.MatchString(source) {
		return SourceControl{}, fmt.Errorf("%w: source %q", ErrInvalid, source)
	}
	reason, err := checkNote(reason, paused)
	if err != nil {
		return SourceControl{}, err
	}
	var out SourceControl
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var before SourceControl
		var at time.Time
		err := tx.QueryRowContext(ctx, `
			SELECT paused, reason, updated_at FROM market_source_controls WHERE source = $1 FOR UPDATE`,
			source).Scan(&before.Paused, &before.Reason, &at)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && before.Paused == paused {
			out = SourceControl{Source: source, Paused: before.Paused, Reason: before.Reason, UpdatedAt: &at}
			return nil
		}
		err = tx.QueryRowContext(ctx, `
			INSERT INTO market_source_controls (source, paused, reason) VALUES ($1, $2, $3)
			ON CONFLICT (source) DO UPDATE SET paused = excluded.paused, reason = excluded.reason, updated_at = NOW()
			RETURNING updated_at`, source, paused, reason).Scan(&at)
		if err != nil {
			return err
		}
		out = SourceControl{Source: source, Paused: paused, Reason: reason, UpdatedAt: &at}
		action := audit.ActionMarketSourceResume
		if paused {
			action = audit.ActionMarketSourcePause
		}
		return record(ctx, tx, origin, action, audit.TargetMarketSource, source,
			map[string]any{"paused": before.Paused}, map[string]any{"paused": paused, "reason": reason})
	})
	return out, err
}

// RunJobNow makes a job due now: the first worker that polls runs it. It
// returns ErrNotFound for a job that no worker has claimed yet, and
// ErrJobRunning while a worker runs it.
func (s *Store) RunJobNow(ctx context.Context, job string, origin audit.Event) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var running bool
		var next time.Time
		err := tx.QueryRowContext(ctx, `
			SELECT lease_until > NOW(), next_run_at FROM market_job_leases WHERE job = $1 FOR UPDATE`,
			job).Scan(&running, &next)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if running {
			return ErrJobRunning
		}
		if _, err := tx.ExecContext(ctx, `UPDATE market_job_leases SET next_run_at = NOW() WHERE job = $1`, job); err != nil {
			return err
		}
		return record(ctx, tx, origin, audit.ActionMarketJobRun, audit.TargetMarketJob, job,
			map[string]any{"job": job, "next_run_at": next.UTC().Format(time.RFC3339)}, map[string]any{"job": job, "next_run_at": "now"})
	})
}

// RunFilter selects runs of the history.
type RunFilter struct {
	// Job, when set, selects the runs of one job.
	Job string
	// Status, when set, is running, succeeded, or failed.
	Status string
	// BeforeID, when set, continues a list after the run of that id.
	BeforeID int64
	Limit    int
}

// ListRuns returns runs, newest first.
func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]Run, error) {
	if f.Status != "" && !slices.Contains([]string{"running", "succeeded", "failed"}, f.Status) {
		return nil, fmt.Errorf("%w: status %q", ErrInvalid, f.Status)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job, node, mode, status, started_at, finished_at, items_read, items_written, error
		FROM market_sync_runs
		WHERE ($1 = '' OR job = $1) AND ($2 = '' OR status = $2) AND ($3 = 0 OR id < $3)
		ORDER BY id DESC LIMIT $4`, f.Job, f.Status, f.BeforeID, pageLimit(f.Limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.Job, &r.Node, &r.Mode, &r.Status, &r.StartedAt, &r.FinishedAt,
			&r.ItemsRead, &r.ItemsWritten, &r.Error); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// Venues.

// SetVenueEnabled enables or disables a venue: the jobs of a disabled venue
// collect nothing, and the API leaves it out.
func (s *Store) SetVenueEnabled(ctx context.Context, id string, enabled bool, reason string, origin audit.Event) (Venue, error) {
	reason, err := checkNote(reason, false)
	if err != nil {
		return Venue{}, err
	}
	var v Venue
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT id, name, kind, region, country, website, attribution, enabled
			FROM market_venues WHERE id = $1 FOR UPDATE`, id).
			Scan(&v.ID, &v.Name, &v.Kind, &v.Region, &v.Country, &v.Website, &v.Attribution, &v.Enabled)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil || v.Enabled == enabled {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE market_venues SET enabled = $2, updated_at = NOW() WHERE id = $1`, id, enabled); err != nil {
			return err
		}
		v.Enabled = enabled
		action := audit.ActionMarketVenueDisable
		if enabled {
			action = audit.ActionMarketVenueEnable
		}
		return record(ctx, tx, origin, action, audit.TargetMarketVenue, id,
			map[string]any{"enabled": !enabled}, map[string]any{"enabled": enabled, "reason": reason})
	})
	return v, err
}

// Assets.

// AdminAsset is an asset as administrators see it, hidden ones included.
type AdminAsset struct {
	Asset
	// TierOverride is the administrators' tier of its pairs, if any.
	TierOverride *string
}

// AssetFilter selects assets for administrators.
type AssetFilter struct {
	// Status, when set, is unverified, hidden, or linked.
	Status string
	// Query, when set, matches the symbol exactly or the slug or name by
	// prefix, ignoring case.
	Query string
	// AfterID, when set, continues a list after the asset of that id.
	AfterID int64
	Limit   int
}

// AdminAssets returns assets in the order of their ids.
func (s *Store) AdminAssets(ctx context.Context, f AssetFilter) ([]AdminAsset, error) {
	if f.Status != "" && !slices.Contains([]string{StatusUnverified, StatusHidden, StatusLinked}, f.Status) {
		return nil, fmt.Errorf("%w: status %q", ErrInvalid, f.Status)
	}
	q := strings.TrimSpace(f.Query)
	prefix := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(q)) + "%"
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+assetColumns+`, tier_override FROM market_assets
		WHERE ($1 = '' OR status = $1)
			AND ($2 = '' OR symbol = upper($2) OR slug LIKE $3 OR lower(name) LIKE $3)
			AND id > $4
		ORDER BY id LIMIT $5`, f.Status, q, prefix, f.AfterID, pageLimit(f.Limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminAsset
	for rows.Next() {
		a, err := scanAdminAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func scanAdminAsset(row interface{ Scan(dest ...any) error }) (AdminAsset, error) {
	var tier *string
	a, err := scanAsset(scanTail{row, &tier})
	return AdminAsset{Asset: a, TierOverride: tier}, err
}

// scanTail scans the columns of an asset followed by one more.
type scanTail struct {
	row  interface{ Scan(dest ...any) error }
	tail any
}

func (s scanTail) Scan(dest ...any) error { return s.row.Scan(append(dest, s.tail)...) }

// AdminAsset returns an asset, hidden ones included, or ErrNotFound.
func (s *Store) AdminAsset(ctx context.Context, id int64) (AdminAsset, error) {
	a, err := scanAdminAsset(s.db.QueryRowContext(ctx, `SELECT `+assetColumns+`, tier_override FROM market_assets WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AdminAsset{}, ErrNotFound
	}
	return a, err
}

// lockAsset reads the status and currency of an asset and locks its row.
func lockAsset(ctx context.Context, tx *sql.Tx, id int64) (status string, currency *string, err error) {
	err = tx.QueryRowContext(ctx, `SELECT status, currency_hk::text FROM market_assets WHERE id = $1 FOR UPDATE`, id).
		Scan(&status, &currency)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	return status, currency, err
}

func (s *Store) setAssetStatus(ctx context.Context, tx *sql.Tx, id int64, status string, currency *string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE market_assets SET status = $2, currency_hk = $3::uuid, updated_at = NOW() WHERE id = $1`,
		id, status, currency)
	return err
}

// SetAssetHidden hides an asset, which the API then never serves, or shows
// it again as unverified. A linked asset is refused with ErrAssetLinked.
func (s *Store) SetAssetHidden(ctx context.Context, id int64, hidden bool, reason string, origin audit.Event) (AdminAsset, error) {
	reason, err := checkNote(reason, hidden)
	if err != nil {
		return AdminAsset{}, err
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		status, _, err := lockAsset(ctx, tx, id)
		if err != nil {
			return err
		}
		if status == StatusLinked {
			return ErrAssetLinked
		}
		want, action := StatusUnverified, audit.ActionMarketAssetUnhide
		if hidden {
			want, action = StatusHidden, audit.ActionMarketAssetHide
		}
		if status == want {
			return nil
		}
		if err := s.setAssetStatus(ctx, tx, id, want, nil); err != nil {
			return err
		}
		return record(ctx, tx, origin, action, audit.TargetMarketAsset, strconv.FormatInt(id, 10),
			map[string]any{"status": status}, map[string]any{"status": want, "reason": reason})
	})
	if err != nil {
		return AdminAsset{}, err
	}
	return s.AdminAsset(ctx, id)
}

// LinkAsset links an asset to a listed currency: the asset is then the
// same as the currency for the catalog. It only reads the currency, never
// changes it. A currency is linked to one asset at most.
func (s *Store) LinkAsset(ctx context.Context, id int64, currencyHK, reason string, origin audit.Event) (AdminAsset, error) {
	hk, err := uuid.Parse(currencyHK)
	if err != nil {
		return AdminAsset{}, fmt.Errorf("%w: currency %q", ErrInvalid, currencyHK)
	}
	currencyHK = hk.String()
	if reason, err = checkNote(reason, false); err != nil {
		return AdminAsset{}, err
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		status, current, err := lockAsset(ctx, tx, id)
		if err != nil {
			return err
		}
		switch {
		case status == StatusHidden:
			return ErrAssetHidden
		case current != nil && *current == currencyHK:
			return nil
		case current != nil:
			return ErrAssetLinked
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM currencies WHERE hk = $1 AND deleted_at IS NULL)`,
			currencyHK).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrCurrencyNotFound
		}
		if err := s.setAssetStatus(ctx, tx, id, StatusLinked, &currencyHK); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrCurrencyLinked
			}
			return err
		}
		return record(ctx, tx, origin, audit.ActionMarketAssetLink, audit.TargetMarketAsset, strconv.FormatInt(id, 10),
			map[string]any{"status": status}, map[string]any{"status": StatusLinked, "currency_hk": currencyHK, "reason": reason})
	})
	if err != nil {
		return AdminAsset{}, err
	}
	return s.AdminAsset(ctx, id)
}

// UnlinkAsset ends the link of an asset to its currency; the asset is
// unverified again. The currency is not changed.
func (s *Store) UnlinkAsset(ctx context.Context, id int64, reason string, origin audit.Event) (AdminAsset, error) {
	reason, err := checkNote(reason, false)
	if err != nil {
		return AdminAsset{}, err
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		status, current, err := lockAsset(ctx, tx, id)
		if err != nil {
			return err
		}
		if current == nil {
			return ErrAssetNotLinked
		}
		if err := s.setAssetStatus(ctx, tx, id, StatusUnverified, nil); err != nil {
			return err
		}
		return record(ctx, tx, origin, audit.ActionMarketAssetUnlink, audit.TargetMarketAsset, strconv.FormatInt(id, 10),
			map[string]any{"status": status, "currency_hk": *current}, map[string]any{"status": StatusUnverified, "reason": reason})
	})
	if err != nil {
		return AdminAsset{}, err
	}
	return s.AdminAsset(ctx, id)
}

// Pairs.

// AdminPair is a pair as administrators see it.
type AdminPair struct {
	VenuePair
	Tier         string
	TierOverride *string
}

// PairFilter selects pairs for administrators.
type PairFilter struct {
	// Venue, when set, selects the pairs of one venue.
	Venue string
	// Unmapped selects the pairs without a base or quote asset.
	Unmapped bool
	// AfterID, when set, continues a list after the pair of that id.
	AfterID int64
	Limit   int
}

// AdminPairs returns pairs in the order of their ids.
func (s *Store) AdminPairs(ctx context.Context, f PairFilter) ([]AdminPair, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, venue_id, venue_symbol, base_symbol, quote_symbol, base_asset_id, quote_asset_id, active, tier, tier_override
		FROM market_venue_pairs
		WHERE ($1 = '' OR venue_id = $1) AND (NOT $2 OR base_asset_id IS NULL OR quote_asset_id IS NULL) AND id > $3
		ORDER BY id LIMIT $4`, f.Venue, f.Unmapped, f.AfterID, pageLimit(f.Limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminPair
	for rows.Next() {
		var p AdminPair
		if err := rows.Scan(&p.ID, &p.VenueID, &p.VenueSymbol, &p.BaseSymbol, &p.QuoteSymbol, &p.BaseAssetID,
			&p.QuoteAssetID, &p.Active, &p.Tier, &p.TierOverride); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MapPair sets the base and quote assets of a pair, for a pair that the
// jobs could not map, or mapped wrongly. The jobs keep a mapping once it is
// set. It returns ErrNotFound when the pair or an asset does not exist.
func (s *Store) MapPair(ctx context.Context, pairID, baseID, quoteID int64, origin audit.Event) error {
	if baseID == quoteID {
		return fmt.Errorf("%w: the base and the quote are the same asset", ErrInvalid)
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var base, quote *int64
		err := tx.QueryRowContext(ctx, `SELECT base_asset_id, quote_asset_id FROM market_venue_pairs WHERE id = $1 FOR UPDATE`,
			pairID).Scan(&base, &quote)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if base != nil && quote != nil && *base == baseID && *quote == quoteID {
			return nil
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM market_assets WHERE id IN ($1, $2)`, baseID, quoteID).Scan(&n); err != nil {
			return err
		}
		if n != 2 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE market_venue_pairs SET base_asset_id = $2, quote_asset_id = $3, updated_at = NOW() WHERE id = $1`,
			pairID, baseID, quoteID); err != nil {
			return err
		}
		return record(ctx, tx, origin, audit.ActionMarketPairMap, audit.TargetMarketPair, strconv.FormatInt(pairID, 10),
			map[string]any{"base_asset_id": base, "quote_asset_id": quote},
			map[string]any{"base_asset_id": baseID, "quote_asset_id": quoteID})
	})
}

// Conflicts.

// AdminConflict is a conflict of identity, as administrators see it.
type AdminConflict struct {
	ID int64
	Conflict
	CreatedAt    time.Time
	ResolvedAt   *time.Time
	ResolvedNote string
}

// ConflictFilter selects conflicts.
type ConflictFilter struct {
	Source string
	// Resolved selects the resolved conflicts instead of the open ones.
	Resolved bool
	// AfterID, when set, continues a list after the conflict of that id.
	AfterID int64
	Limit   int
}

// ListConflicts returns conflicts, oldest first.
func (s *Store) ListConflicts(ctx context.Context, f ConflictFilter) ([]AdminConflict, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, source, external_id, kind, subject, asset_id, claimed_by, detail, created_at, resolved_at, resolved_note
		FROM market_identity_conflicts
		WHERE ($1 = '' OR source = $1) AND (resolved_at IS NOT NULL) = $2 AND id > $3
		ORDER BY id LIMIT $4`, f.Source, f.Resolved, f.AfterID, pageLimit(f.Limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminConflict
	for rows.Next() {
		var c AdminConflict
		if err := rows.Scan(&c.ID, &c.Source, &c.ExternalID, &c.Kind, &c.Subject, &c.AssetID, &c.ClaimedBy, &c.Detail,
			&c.CreatedAt, &c.ResolvedAt, &c.ResolvedNote); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ResolveConflict closes a conflict with a note of what was decided; the
// fix itself, such as hiding an asset or mapping a pair, is its own change.
// A conflict that the sync meets again later opens anew.
func (s *Store) ResolveConflict(ctx context.Context, id int64, note string, origin audit.Event) error {
	note, err := checkNote(note, true)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var resolved *time.Time
		var source, external, kind, subject string
		err := tx.QueryRowContext(ctx, `
			SELECT resolved_at, source, external_id, kind, subject FROM market_identity_conflicts WHERE id = $1 FOR UPDATE`,
			id).Scan(&resolved, &source, &external, &kind, &subject)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if resolved != nil {
			return ErrConflictResolved
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE market_identity_conflicts SET resolved_at = NOW(), resolved_note = $2 WHERE id = $1`, id, note); err != nil {
			return err
		}
		return record(ctx, tx, origin, audit.ActionMarketConflictResolve, audit.TargetMarketConflict, strconv.FormatInt(id, 10),
			map[string]any{"source": source, "external_id": external, "kind": kind, "subject": subject},
			map[string]any{"note": note})
	})
}
