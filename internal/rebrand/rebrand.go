// Package rebrand schedules and runs the migrations of currencies (#215): a
// rebrand, a redenomination, or a merger that moves the holders of one or
// more source currencies to one target, each source at its own ratio.
//
// A migration goes through scheduled phases. The scheduler of every
// instance claims each phase that is due with FOR UPDATE SKIP LOCKED and
// applies it in one transaction with its audit event and the currency
// events of its sources, so each phase applies exactly once. The phases of
// a forced migration narrow the capabilities of its sources, which every
// flow checks: deposits and withdrawals pause, then trading and conversions
// halt, and the sources leave the lists once completed.
//
// The conversion of balances, idempotent per migration and account, with
// the dust recorded, needs the ledger (#60); the notices to holders need
// #170.
package rebrand

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/google/uuid"
)

// Phase is where a migration is.
type Phase string

const (
	PhaseScheduled      Phase = "scheduled"
	PhaseAnnounced      Phase = "announced"
	PhaseDepositsPaused Phase = "deposits_paused"
	PhaseTradingHalted  Phase = "trading_halted"
	PhaseConverting     Phase = "converting"
	PhaseCompleted      Phase = "completed"
	PhaseCancelled      Phase = "cancelled"
)

// phases are the phases that can be scheduled, in order.
var phases = []Phase{PhaseAnnounced, PhaseDepositsPaused, PhaseTradingHalted, PhaseConverting, PhaseCompleted}

func (p Phase) rank() int { return slices.Index(phases, p) }

// Errors of the changes.
var (
	// ErrTargetNotListed is a migration into a currency that is not
	// approved.
	ErrTargetNotListed = errors.New("the target is not an approved currency")
	// ErrSourceNotListed is a source that is not approved.
	ErrSourceNotListed = errors.New("a source is not an approved currency")
	// ErrSourceInMigration is a source of another open migration.
	ErrSourceInMigration = errors.New("a source is already being migrated")
	// ErrTargetInMigration is a target that is itself being migrated.
	ErrTargetInMigration = errors.New("the target is being migrated")
	ErrNotFound          = errors.New("migration not found")
	// ErrNotOpen is a cancel of a completed or cancelled migration.
	ErrNotOpen = errors.New("the migration is completed or cancelled")
	// ErrIrreversible is a cancel of a migration that is converting.
	ErrIrreversible = errors.New("the migration is converting and can no longer be cancelled")
)

// VersionConflict is a change based on another version of the migration.
type VersionConflict struct{ Current int64 }

func (v *VersionConflict) Error() string {
	return fmt.Sprintf("the migration is at version %d", v.Current)
}

// Source is a source currency of a migration and its ratio: the units of
// the target that one unit of the source becomes, as a decimal string.
type Source struct {
	CurrencyHK string `json:"currency_hk" format:"uuid"`
	Ratio      string `json:"ratio" example:"0.01"`
}

// Step is a phase of a migration and its time.
type Step struct {
	Phase     Phase      `json:"phase" enums:"announced,deposits_paused,trading_halted,converting,completed"`
	At        time.Time  `json:"at"`
	AppliedAt *time.Time `json:"applied_at,omitempty"`
}

// Migration is a migration as administrators see it.
type Migration struct {
	ID              string     `json:"id" format:"uuid"`
	TargetHK        string     `json:"target_hk" format:"uuid"`
	Sources         []Source   `json:"sources"`
	Forced          bool       `json:"forced"`
	Phase           Phase      `json:"phase" enums:"scheduled,announced,deposits_paused,trading_halted,converting,completed,cancelled"`
	Steps           []Step     `json:"steps"`
	AnnouncementURL string     `json:"announcement_url,omitempty"`
	Note            string     `json:"note,omitempty"`
	CreatedBy       string     `json:"created_by" format:"uuid"`
	CreatedAt       time.Time  `json:"created_at"`
	CancelledAt     *time.Time `json:"cancelled_at,omitempty"`
	CancelledBy     *string    `json:"cancelled_by,omitempty" format:"uuid"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	Version         int64      `json:"version" example:"1"`
}

// Plan is a migration to schedule into a target.
type Plan struct {
	Sources []Source `json:"sources"`
	// Forced converts every holding, and pauses and retires the sources.
	// Without it, the conversion is offered and the sources stay.
	Forced          bool   `json:"forced"`
	Steps           []Step `json:"steps"`
	AnnouncementURL string `json:"announcement_url,omitempty"`
	Note            string `json:"note,omitempty"`
}

const (
	maxSources = 20
	maxNote    = 1000
	maxURL     = 2048
	pastSlack  = 5 * time.Minute
)

// Validate checks p, a migration into targetHK, and returns each invalid
// field with what is wrong. It puts the hks in their canonical form.
func (p *Plan) Validate(targetHK string, now time.Time) map[string]string {
	errs := map[string]string{}
	if len(p.Sources) == 0 || len(p.Sources) > maxSources {
		errs["sources"] = fmt.Sprintf("must have 1 to %d sources", maxSources)
	}
	seen := map[string]bool{}
	for i := range p.Sources {
		s := &p.Sources[i]
		id, err := uuid.Parse(s.CurrencyHK)
		switch {
		case err != nil:
			errs["sources"] = fmt.Sprintf("source %d: currency_hk must be a UUID", i)
		case id.String() == targetHK:
			errs["sources"] = "must not include the target"
		case seen[id.String()]:
			errs["sources"] = "must name each currency once"
		case !currencies.ValidDecimal(s.Ratio) || !positive(s.Ratio):
			errs["sources"] = fmt.Sprintf("source %d: ratio must be a positive decimal number, at most 18 digits after the point", i)
		}
		if err == nil {
			s.CurrencyHK = id.String()
			seen[s.CurrencyHK] = true
		}
	}
	for i, s := range p.Steps {
		switch {
		case s.Phase.rank() < 0:
			errs["steps"] = fmt.Sprintf("step %d: unknown phase %q", i, s.Phase)
		case !p.Forced && (s.Phase == PhaseDepositsPaused || s.Phase == PhaseTradingHalted):
			errs["steps"] = "of an optional migration must not pause the sources"
		case i > 0 && s.Phase.rank() <= p.Steps[i-1].Phase.rank():
			errs["steps"] = "must be in the order of the phases, each once"
		case i > 0 && !s.At.After(p.Steps[i-1].At):
			errs["steps"] = "must each be later than the step before"
		case s.At.Before(now.Add(-pastSlack)):
			errs["steps"] = fmt.Sprintf("step %d: is in the past", i)
		case s.AppliedAt != nil:
			errs["steps"] = "must not have applied_at"
		}
		if _, bad := errs["steps"]; bad {
			break
		}
	}
	if _, bad := errs["steps"]; !bad {
		if len(p.Steps) < 2 || p.Steps[0].Phase != PhaseAnnounced || p.Steps[len(p.Steps)-1].Phase != PhaseCompleted {
			errs["steps"] = "must start with announced and end with completed"
		}
	}
	if p.AnnouncementURL != "" {
		u, err := url.Parse(p.AnnouncementURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || len(p.AnnouncementURL) > maxURL {
			errs["announcement_url"] = "must be an https URL of at most 2048 characters"
		}
	}
	if len([]rune(p.Note)) > maxNote {
		errs["note"] = "must be at most 1000 characters"
	}
	return errs
}

func positive(s string) bool {
	for _, r := range s {
		if r >= '1' && r <= '9' {
			return true
		}
	}
	return false
}

// currency is a locked currency.
type currency struct {
	hk     string
	status string
	info   models.CurrencyInfo
}

func (c currency) public() bool { return c.status == models.CurrencyStatusApproved }

// lockCurrencies takes the update locks of currencies that are not deleted,
// in the order of their hks, so that two changes of overlapping currencies
// cannot deadlock. A missing or deleted one is left out.
func lockCurrencies(ctx context.Context, tx *sql.Tx, hks []string) (map[string]currency, error) {
	sorted := slices.Clone(hks)
	slices.Sort(sorted)
	out := map[string]currency{}
	for _, hk := range slices.Compact(sorted) {
		c := currency{hk: hk}
		var info []byte
		err := tx.QueryRowContext(ctx, `SELECT status, info FROM currencies WHERE hk = $1 AND deleted_at IS NULL FOR UPDATE`, hk).Scan(&c.status, &info)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("failed to lock a currency: %w", err)
		}
		if err := json.Unmarshal(info, &c.info); err != nil {
			return nil, fmt.Errorf("currency %s: %w", hk, err)
		}
		out[hk] = c
	}
	return out, nil
}

// announce writes the currency events of the sources that are public, with
// their state after the change, and returns their changes.
func announce(ctx context.Context, tx *sql.Tx, locked map[string]currency, sources []string) ([]currencies.Change, error) {
	var changes []currencies.Change
	for _, hk := range sources {
		c, ok := locked[hk]
		if !ok {
			continue
		}
		change := currencies.Change{CurrencyHK: hk}
		if c.public() {
			l, err := currencies.ReadLifecycle(ctx, tx, hk, false)
			if err != nil {
				return nil, err
			}
			if change.Public, err = currencies.EnqueueEvent(ctx, tx, true, models.Currency{HK: hk, Status: c.status, Info: c.info, Lifecycle: l}); err != nil {
				return nil, err
			}
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func sourceHKs(sources []Source) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.CurrencyHK)
	}
	return out
}

// Schedule schedules a migration into targetHK. Nothing shows until its
// announced phase applies.
func Schedule(ctx context.Context, db *sql.DB, targetHK string, p Plan, origin audit.Event) (Migration, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Migration{}, fmt.Errorf("failed to start the schedule: %w", err)
	}
	defer tx.Rollback()
	sources := sourceHKs(p.Sources)
	locked, err := lockCurrencies(ctx, tx, append([]string{targetHK}, sources...))
	if err != nil {
		return Migration{}, err
	}
	if c, ok := locked[targetHK]; !ok || !c.public() {
		return Migration{}, ErrTargetNotListed
	}
	for _, hk := range sources {
		if c, ok := locked[hk]; !ok || !c.public() {
			return Migration{}, ErrSourceNotListed
		}
	}
	// The locks of the currencies order this read against the schedules of
	// other migrations of the same currencies.
	var busy, targetBusy bool
	if err := tx.QueryRowContext(ctx, `SELECT
			EXISTS (SELECT 1 FROM currency_migration_sources s JOIN currency_migrations m ON m.hk = s.migration_hk
				WHERE s.currency_hk = ANY(string_to_array($1, ',')::uuid[]) AND m.phase NOT IN ('completed', 'cancelled')),
			EXISTS (SELECT 1 FROM currency_migration_sources s JOIN currency_migrations m ON m.hk = s.migration_hk
				WHERE s.currency_hk = $2 AND m.phase NOT IN ('completed', 'cancelled'))`,
		joinHKs(sources), targetHK).Scan(&busy, &targetBusy); err != nil {
		return Migration{}, fmt.Errorf("failed to read the migrations: %w", err)
	}
	if busy {
		return Migration{}, ErrSourceInMigration
	}
	if targetBusy {
		return Migration{}, ErrTargetInMigration
	}

	hk := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `INSERT INTO currency_migrations (hk, target_hk, forced, announcement_url, note, created_by)
		VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6)`,
		hk, targetHK, p.Forced, p.AnnouncementURL, p.Note, origin.ActorID); err != nil {
		return Migration{}, fmt.Errorf("failed to schedule the migration: %w", err)
	}
	logged := []map[string]any{}
	for _, s := range p.Sources {
		if _, err := tx.ExecContext(ctx, `INSERT INTO currency_migration_sources (migration_hk, currency_hk, ratio) VALUES ($1, $2, $3::numeric)`,
			hk, s.CurrencyHK, s.Ratio); err != nil {
			return Migration{}, fmt.Errorf("failed to schedule the migration: %w", err)
		}
		logged = append(logged, map[string]any{"currency_hk": s.CurrencyHK, "ratio": s.Ratio})
	}
	steps := []map[string]any{}
	for _, s := range p.Steps {
		if _, err := tx.ExecContext(ctx, `INSERT INTO currency_migration_phases (migration_hk, phase, scheduled_at) VALUES ($1, $2, $3)`,
			hk, s.Phase, s.At); err != nil {
			return Migration{}, fmt.Errorf("failed to schedule the migration: %w", err)
		}
		steps = append(steps, map[string]any{"phase": s.Phase, "at": s.At.UTC().Format(time.RFC3339)})
	}
	e := audit.Of(origin, audit.ActionCurrencyMigrationSchedule, audit.TargetCurrency, targetHK)
	e.Changes = &audit.Changes{After: map[string]any{"migration": hk, "sources": logged, "forced": p.Forced, "steps": steps}}
	if err := audit.Record(ctx, tx, e); err != nil {
		return Migration{}, err
	}
	m, err := get(ctx, tx, hk)
	if err != nil {
		return Migration{}, err
	}
	if err := tx.Commit(); err != nil {
		return Migration{}, fmt.Errorf("failed to schedule the migration: %w", err)
	}
	return m, nil
}

// joinHKs joins hks for string_to_array: they are UUIDs, without commas.
func joinHKs(hks []string) string { return strings.Join(hks, ",") }

// Cancel cancels a migration of targetHK before it converts: the phases
// that are left do not apply, and the sources get back what the phases
// paused. With version, it is refused when the migration has changed since.
func Cancel(ctx context.Context, db *sql.DB, targetHK, hk string, version *int64, origin audit.Event) (Migration, []currencies.Change, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Migration{}, nil, fmt.Errorf("failed to start the cancel: %w", err)
	}
	defer tx.Rollback()
	m, err := get(ctx, tx, hk)
	if errors.Is(err, ErrNotFound) || err == nil && m.TargetHK != targetHK {
		return Migration{}, nil, ErrNotFound
	}
	if err != nil {
		return Migration{}, nil, err
	}
	sources := sourceHKs(m.Sources)
	locked, err := lockCurrencies(ctx, tx, append([]string{targetHK}, sources...))
	if err != nil {
		return Migration{}, nil, err
	}
	var phase Phase
	var current int64
	if err := tx.QueryRowContext(ctx, `SELECT phase, version FROM currency_migrations WHERE hk = $1 FOR UPDATE`, hk).Scan(&phase, &current); err != nil {
		return Migration{}, nil, fmt.Errorf("failed to read the migration: %w", err)
	}
	if version != nil && *version != current {
		return Migration{}, nil, &VersionConflict{Current: current}
	}
	switch phase {
	case PhaseCompleted, PhaseCancelled:
		return Migration{}, nil, ErrNotOpen
	case PhaseConverting:
		return Migration{}, nil, ErrIrreversible
	}
	if _, err := tx.ExecContext(ctx, `UPDATE currency_migrations SET phase = 'cancelled', cancelled_at = NOW(), cancelled_by = $2,
		version = version + 1 WHERE hk = $1`, hk, origin.ActorID); err != nil {
		return Migration{}, nil, fmt.Errorf("failed to cancel the migration: %w", err)
	}
	e := audit.Of(origin, audit.ActionCurrencyMigrationCancel, audit.TargetCurrency, targetHK)
	e.Changes = &audit.Changes{Before: map[string]any{"migration": hk, "phase": phase}, After: map[string]any{"migration": hk, "phase": PhaseCancelled}}
	if err := audit.Record(ctx, tx, e); err != nil {
		return Migration{}, nil, err
	}
	var changes []currencies.Change
	if phase != PhaseScheduled {
		if changes, err = announce(ctx, tx, locked, sources); err != nil {
			return Migration{}, nil, err
		}
	}
	m, err = get(ctx, tx, hk)
	if err != nil {
		return Migration{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return Migration{}, nil, fmt.Errorf("failed to cancel the migration: %w", err)
	}
	return m, changes, nil
}

// Advance applies the phases that are due at now, each exactly once on all
// instances, at most limit of them. It is a currencies.Advancer.
func Advance(ctx context.Context, db *sql.DB, now time.Time, limit int) ([]currencies.Change, error) {
	var changes []currencies.Change
	for applied := 0; applied < limit; applied++ {
		c, ok, err := advanceOne(ctx, db, now)
		if err != nil {
			return changes, err
		}
		if !ok {
			break
		}
		changes = append(changes, c...)
	}
	return changes, nil
}

func advanceOne(ctx context.Context, db *sql.DB, now time.Time) ([]currencies.Change, bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("failed to start a phase: %w", err)
	}
	defer tx.Rollback()
	// The phases of a migration are due in their order, so its earliest due
	// phase is its next one. Only the scheduler locks phases, so claiming
	// one first, and then the currencies and the migration, in the order of
	// every other change, cannot deadlock.
	var hk, targetHK string
	var phase, before Phase
	err = tx.QueryRowContext(ctx, `
		SELECT ph.migration_hk, ph.phase, m.target_hk
		FROM currency_migration_phases ph JOIN currency_migrations m ON m.hk = ph.migration_hk
		WHERE m.phase NOT IN ('completed', 'cancelled') AND ph.applied_at IS NULL AND ph.scheduled_at <= $1
		ORDER BY ph.scheduled_at, ph.migration_hk
		FOR UPDATE OF ph SKIP LOCKED LIMIT 1`, now).Scan(&hk, &phase, &targetHK)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("failed to claim a phase: %w", err)
	}
	m, err := get(ctx, tx, hk)
	if err != nil {
		return nil, false, err
	}
	sources := sourceHKs(m.Sources)
	locked, err := lockCurrencies(ctx, tx, append([]string{targetHK}, sources...))
	if err != nil {
		return nil, false, err
	}
	// A cancel that came first ends the phase.
	if err := tx.QueryRowContext(ctx, `SELECT phase FROM currency_migrations WHERE hk = $1 FOR UPDATE`, hk).Scan(&before); err != nil {
		return nil, false, fmt.Errorf("failed to lock the migration: %w", err)
	}
	if before == PhaseCompleted || before == PhaseCancelled {
		return nil, true, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE currency_migration_phases SET applied_at = $3 WHERE migration_hk = $1 AND phase = $2`,
		hk, phase, now); err != nil {
		return nil, false, fmt.Errorf("failed to apply a phase: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE currency_migrations SET phase = $2, version = version + 1,
		completed_at = CASE WHEN $2 = 'completed' THEN $3::timestamptz END WHERE hk = $1`, hk, phase, now); err != nil {
		return nil, false, fmt.Errorf("failed to apply a phase: %w", err)
	}
	e := audit.Of(audit.Event{}, audit.ActionCurrencyMigrationPhase, audit.TargetCurrency, targetHK)
	e.Changes = &audit.Changes{Before: map[string]any{"migration": hk, "phase": before}, After: map[string]any{"migration": hk, "phase": phase}}
	if err := audit.Record(ctx, tx, e); err != nil {
		return nil, false, err
	}
	changes, err := announce(ctx, tx, locked, sources)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("failed to apply a phase: %w", err)
	}
	return changes, true, nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

const columns = `hk, target_hk, forced, phase, COALESCE(announcement_url, ''), COALESCE(note, ''), created_by, created_at,
	cancelled_at, cancelled_by, completed_at, version`

// List returns the migrations of a currency, as their target or a source,
// newest first.
func List(ctx context.Context, q querier, currencyHK string) ([]Migration, error) {
	return list(ctx, q, `SELECT `+columns+` FROM currency_migrations m WHERE target_hk = $1
		OR EXISTS (SELECT 1 FROM currency_migration_sources s WHERE s.migration_hk = m.hk AND s.currency_hk = $1)
		ORDER BY created_at DESC, hk DESC LIMIT 100`, currencyHK)
}

func get(ctx context.Context, q querier, hk string) (Migration, error) {
	found, err := list(ctx, q, `SELECT `+columns+` FROM currency_migrations WHERE hk = $1`, hk)
	if err != nil {
		return Migration{}, err
	}
	if len(found) == 0 {
		return Migration{}, ErrNotFound
	}
	return found[0], nil
}

func list(ctx context.Context, q querier, query string, args ...any) ([]Migration, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to read the migrations: %w", err)
	}
	out := []Migration{}
	for rows.Next() {
		var m Migration
		if err := rows.Scan(&m.ID, &m.TargetHK, &m.Forced, &m.Phase, &m.AnnouncementURL, &m.Note, &m.CreatedBy, &m.CreatedAt,
			&m.CancelledAt, &m.CancelledBy, &m.CompletedAt, &m.Version); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to read the migrations: %w", err)
		}
		m.Sources, m.Steps = []Source{}, []Step{}
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read the migrations: %w", err)
	}
	for i := range out {
		if err := details(ctx, q, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func details(ctx context.Context, q querier, m *Migration) error {
	rows, err := q.QueryContext(ctx, `SELECT currency_hk, trim_scale(ratio)::TEXT FROM currency_migration_sources
		WHERE migration_hk = $1 ORDER BY currency_hk`, m.ID)
	if err != nil {
		return fmt.Errorf("failed to read the sources: %w", err)
	}
	for rows.Next() {
		var s Source
		if err := rows.Scan(&s.CurrencyHK, &s.Ratio); err != nil {
			rows.Close()
			return fmt.Errorf("failed to read the sources: %w", err)
		}
		m.Sources = append(m.Sources, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read the sources: %w", err)
	}
	rows, err = q.QueryContext(ctx, `SELECT phase, scheduled_at, applied_at FROM currency_migration_phases
		WHERE migration_hk = $1 ORDER BY scheduled_at`, m.ID)
	if err != nil {
		return fmt.Errorf("failed to read the phases: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s Step
		if err := rows.Scan(&s.Phase, &s.At, &s.AppliedAt); err != nil {
			return fmt.Errorf("failed to read the phases: %w", err)
		}
		m.Steps = append(m.Steps, s)
	}
	return rows.Err()
}
