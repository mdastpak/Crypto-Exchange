// Package delisting schedules and runs the delistings of currencies (#216).
//
// A delisting is a schedule of lifecycle steps of one currency, from the
// announcement to delisted. The scheduler of every instance looks for the
// steps that are due and applies each in its own transaction, which claims
// the step with FOR UPDATE SKIP LOCKED and marks it applied: exactly one
// instance applies a step, and a step whose instance crashed before the
// commit is applied by the next run. The change of the currency's lifecycle,
// its audit event, and its currency event in the outbox commit together.
//
// The settlement of the balances that are left at the frozen step needs the
// ledger (#60), and the notifications of the holders need #170; a delisting
// records its settlement policy now, so that it is announced with the
// schedule.
package delisting

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/google/uuid"
)

// Kind is how a delisting starts.
type Kind string

const (
	// KindStandard starts with the announcement.
	KindStandard Kind = "standard"
	// KindEmergency is for a security or regulatory reason, and starts at
	// trading_halted or later.
	KindEmergency Kind = "emergency"
)

// Status is where a delisting is.
type Status string

const (
	StatusScheduled Status = "scheduled"
	StatusCompleted Status = "completed"
	StatusCancelled Status = "cancelled"
)

// Settlement is what happens to the balances that are left at frozen.
type Settlement string

const (
	// SettlementConvert converts them into another currency, at a price.
	SettlementConvert Settlement = "convert"
	// SettlementClaim holds them for their owners to claim, for a period.
	SettlementClaim Settlement = "claim"
)

// PriceSource is the price of a conversion.
type PriceSource string

const (
	PriceLastTrade PriceSource = "last_trade"
	PriceTWAP      PriceSource = "twap"
	PriceFixed     PriceSource = "fixed"
)

// Errors of the changes.
var (
	ErrCurrencyNotFound = errors.New("currency not found")
	// ErrNotListed is a delisting of a currency that is not approved.
	ErrNotListed = errors.New("the currency is not approved")
	// ErrInProgress is a delisting of a currency that is already being
	// delisted, or is delisted.
	ErrInProgress = errors.New("the currency is already being delisted")
	// ErrSettlementCurrency is a conversion into a currency that is not
	// approved.
	ErrSettlementCurrency = errors.New("the settlement currency is not approved")
	// ErrStepsBehind is a delisting of a currency that a delisting before
	// left on its way, whose first step is not after where it is.
	ErrStepsBehind = errors.New("the steps must start after the lifecycle state of the currency")
	ErrNotFound    = errors.New("delisting not found")
	// ErrNotScheduled is a cancel of a delisting that is completed or
	// cancelled.
	ErrNotScheduled = errors.New("the delisting is not scheduled")
	// ErrIrreversible is a cancel of a delisting that has reached frozen,
	// after which balances are settled.
	ErrIrreversible = errors.New("the delisting can no longer be cancelled")
)

// VersionConflict is a change based on another version of the delisting.
type VersionConflict struct{ Current int64 }

func (v *VersionConflict) Error() string {
	return fmt.Sprintf("the delisting is at version %d", v.Current)
}

// Step is a step of a delisting.
type Step struct {
	State currencies.Lifecycle `json:"state" enums:"delist_announced,deposits_disabled,sell_only,trading_halted,withdrawal_only,frozen,delisted"`
	At    time.Time            `json:"at"`
	// AppliedAt is when the scheduler applied it.
	AppliedAt *time.Time `json:"applied_at,omitempty"`
}

// Delisting is a delisting as administrators see it.
type Delisting struct {
	ID                   string      `json:"id" format:"uuid"`
	CurrencyHK           string      `json:"currency_hk" format:"uuid"`
	Kind                 Kind        `json:"kind" enums:"standard,emergency"`
	Status               Status      `json:"status" enums:"scheduled,completed,cancelled"`
	Settlement           Settlement  `json:"settlement" enums:"convert,claim"`
	SettlementCurrencyHK *string     `json:"settlement_currency_hk,omitempty" format:"uuid"`
	PriceSource          PriceSource `json:"price_source,omitempty" enums:"last_trade,twap,fixed"`
	// FixedPrice is the price of a fixed conversion, in the settlement
	// currency, as a decimal string.
	FixedPrice      *string    `json:"fixed_price,omitempty" example:"0.0125"`
	ClaimDays       *int       `json:"claim_days,omitempty" example:"180"`
	AnnouncementURL string     `json:"announcement_url,omitempty"`
	Note            string     `json:"note,omitempty"`
	Steps           []Step     `json:"steps"`
	CreatedBy       string     `json:"created_by" format:"uuid"`
	CreatedAt       time.Time  `json:"created_at"`
	CancelledAt     *time.Time `json:"cancelled_at,omitempty"`
	CancelledBy     *string    `json:"cancelled_by,omitempty" format:"uuid"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	Version         int64      `json:"version" example:"1"`
}

// Plan is a delisting to schedule.
type Plan struct {
	Kind  Kind   `json:"kind" enums:"standard,emergency"`
	Steps []Step `json:"steps"`
	// Settlement is convert, with settlement_currency_hk and price_source,
	// or claim, with claim_days.
	Settlement           Settlement  `json:"settlement" enums:"convert,claim"`
	SettlementCurrencyHK string      `json:"settlement_currency_hk,omitempty" format:"uuid"`
	PriceSource          PriceSource `json:"price_source,omitempty" enums:"last_trade,twap,fixed"`
	FixedPrice           string      `json:"fixed_price,omitempty" example:"0.0125"`
	ClaimDays            int         `json:"claim_days,omitempty" example:"180"`
	AnnouncementURL      string      `json:"announcement_url,omitempty"`
	Note                 string      `json:"note,omitempty"`
}

const (
	maxNote = 1000
	maxURL  = 2048
	// pastSlack is how far in the past a step may be scheduled, for the
	// clock of the administrator's client. A step that is due is applied at
	// once.
	pastSlack = 5 * time.Minute
)

// Validate checks p, and returns each invalid field with what is wrong.
func (p *Plan) Validate(now time.Time) map[string]string {
	errs := map[string]string{}
	switch p.Kind {
	case KindStandard, KindEmergency:
	default:
		errs["kind"] = "must be standard or emergency"
	}
	if len(p.Steps) == 0 {
		errs["steps"] = "must have at least one step"
	}
	for i, s := range p.Steps {
		rank := s.State.Rank()
		switch {
		case rank <= currencies.LifecycleActive.Rank():
			errs["steps"] = fmt.Sprintf("step %d: unknown state %q", i, s.State)
		case i > 0 && rank <= p.Steps[i-1].State.Rank():
			errs["steps"] = "must be in the order of the lifecycle, each state once"
		case i > 0 && !s.At.After(p.Steps[i-1].At):
			errs["steps"] = "must each be later than the step before"
		case s.At.Before(now.Add(-pastSlack)):
			errs["steps"] = fmt.Sprintf("step %d: is in the past", i)
		case s.AppliedAt != nil:
			errs["steps"] = "must not have applied_at"
		}
		if _, ok := errs["steps"]; ok {
			break
		}
	}
	if _, bad := errs["steps"]; !bad && len(p.Steps) > 0 {
		first := p.Steps[0].State
		switch {
		case p.Kind == KindStandard && first != currencies.LifecycleDelistAnnounced:
			errs["steps"] = "of a standard delisting must start with delist_announced"
		case p.Kind == KindEmergency && first.Rank() < currencies.LifecycleTradingHalted.Rank():
			errs["steps"] = "of an emergency delisting must start at trading_halted or later"
		}
	}
	switch p.Settlement {
	case SettlementConvert:
		if _, err := uuid.Parse(p.SettlementCurrencyHK); err != nil {
			errs["settlement_currency_hk"] = "is required for a conversion, as a UUID"
		} else {
			p.SettlementCurrencyHK = uuid.MustParse(p.SettlementCurrencyHK).String()
		}
		switch p.PriceSource {
		case PriceLastTrade, PriceTWAP:
			if p.FixedPrice != "" {
				errs["fixed_price"] = "is only for a fixed price"
			}
		case PriceFixed:
			if f, err := strconv.ParseFloat(p.FixedPrice, 64); err != nil || f <= 0 || !currencies.ValidDecimal(p.FixedPrice) {
				errs["fixed_price"] = "must be a positive decimal number"
			}
		default:
			errs["price_source"] = "must be last_trade, twap, or fixed"
		}
		if p.ClaimDays != 0 {
			errs["claim_days"] = "is only for a claim"
		}
	case SettlementClaim:
		if p.ClaimDays < 1 || p.ClaimDays > 3650 {
			errs["claim_days"] = "must be between 1 and 3650"
		}
		if p.SettlementCurrencyHK != "" || p.PriceSource != "" || p.FixedPrice != "" {
			errs["settlement"] = "claim takes no settlement currency or price"
		}
	default:
		errs["settlement"] = "must be convert or claim"
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

// Change is a committed change of the lifecycle of a currency.
type Change = currencies.Change

// currency is the locked row of a currency.
type currency struct {
	status    string
	info      models.CurrencyInfo
	lifecycle currencies.Lifecycle
}

func (c currency) public() bool { return c.status == models.CurrencyStatusApproved }

// lockCurrency takes the update lock of a currency that is not deleted,
// which orders every change of its lifecycle, and the checks of its
// capabilities, which take a share lock.
func lockCurrency(ctx context.Context, tx *sql.Tx, hk string) (currency, error) {
	var c currency
	var info []byte
	err := tx.QueryRowContext(ctx, `SELECT status, info, lifecycle FROM currencies
		WHERE hk = $1 AND deleted_at IS NULL FOR UPDATE`, hk).Scan(&c.status, &info, &c.lifecycle)
	if errors.Is(err, sql.ErrNoRows) {
		return currency{}, ErrCurrencyNotFound
	}
	if err != nil {
		return currency{}, fmt.Errorf("failed to lock the currency: %w", err)
	}
	if err := json.Unmarshal(info, &c.info); err != nil {
		return currency{}, fmt.Errorf("currency %s: %w", hk, err)
	}
	return c, nil
}

// announce writes the currency event of a change of the lifecycle, with the
// lifecycle after it, when the currency is public.
func announce(ctx context.Context, tx *sql.Tx, hk string, c currency) (bool, error) {
	if !c.public() {
		return false, nil
	}
	l, err := currencies.ReadLifecycle(ctx, tx, hk, false)
	if err != nil {
		return false, err
	}
	return currencies.EnqueueEvent(ctx, tx, true, models.Currency{HK: hk, Status: c.status, Info: c.info, Lifecycle: l})
}

// Schedule schedules a delisting of a currency. origin is the event of the
// administrator's request, with the actor.
func Schedule(ctx context.Context, db *sql.DB, currencyHK string, p Plan, origin audit.Event) (Delisting, Change, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Delisting{}, Change{}, fmt.Errorf("failed to start the schedule: %w", err)
	}
	defer tx.Rollback()
	cur, err := lockCurrency(ctx, tx, currencyHK)
	if err != nil {
		return Delisting{}, Change{}, err
	}
	if !cur.public() {
		return Delisting{}, Change{}, ErrNotListed
	}
	// A delisting under way has steps left. One whose steps are all applied
	// and that stopped before delisted, such as an emergency halt, is
	// superseded: the new one takes the currency on from where it is.
	var open sql.NullString
	var pending bool
	err = tx.QueryRowContext(ctx, `SELECT d.hk, EXISTS (SELECT 1 FROM currency_delisting_steps s WHERE s.delisting_hk = d.hk AND s.applied_at IS NULL)
		FROM currency_delistings d WHERE d.currency_hk = $1 AND d.status = 'scheduled' FOR UPDATE`, currencyHK).Scan(&open, &pending)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Delisting{}, Change{}, fmt.Errorf("failed to read the delistings: %w", err)
	}
	if pending || cur.lifecycle == currencies.LifecycleDelisted {
		return Delisting{}, Change{}, ErrInProgress
	}
	if cur.lifecycle != currencies.LifecycleActive {
		if p.Steps[0].State.Rank() <= cur.lifecycle.Rank() {
			return Delisting{}, Change{}, ErrStepsBehind
		}
	}
	if open.Valid {
		if _, err := tx.ExecContext(ctx, `UPDATE currency_delistings SET status = 'completed', completed_at = NOW(), version = version + 1
			WHERE hk = $1`, open.String); err != nil {
			return Delisting{}, Change{}, fmt.Errorf("failed to close the delisting before: %w", err)
		}
	}
	if p.Settlement == SettlementConvert {
		var ok bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM currencies
			WHERE hk = $1 AND hk <> $2 AND status = 'approved' AND deleted_at IS NULL)`,
			p.SettlementCurrencyHK, currencyHK).Scan(&ok); err != nil {
			return Delisting{}, Change{}, fmt.Errorf("failed to read the settlement currency: %w", err)
		}
		if !ok {
			return Delisting{}, Change{}, ErrSettlementCurrency
		}
	}

	hk := uuid.NewString()
	_, err = tx.ExecContext(ctx, `INSERT INTO currency_delistings (hk, currency_hk, kind, settlement, settlement_currency_hk,
			price_source, fixed_price, claim_days, announcement_url, note, created_by)
		VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid, NULLIF($6, ''), NULLIF($7, '')::numeric, NULLIF($8, 0),
			NULLIF($9, ''), NULLIF($10, ''), $11)`,
		hk, currencyHK, p.Kind, p.Settlement, p.SettlementCurrencyHK, p.PriceSource, p.FixedPrice, p.ClaimDays,
		p.AnnouncementURL, p.Note, origin.ActorID)
	if err != nil {
		return Delisting{}, Change{}, fmt.Errorf("failed to schedule the delisting: %w", err)
	}
	steps := make([]map[string]any, 0, len(p.Steps))
	for _, s := range p.Steps {
		if _, err := tx.ExecContext(ctx, `INSERT INTO currency_delisting_steps (delisting_hk, state, scheduled_at) VALUES ($1, $2, $3)`,
			hk, s.State, s.At); err != nil {
			return Delisting{}, Change{}, fmt.Errorf("failed to schedule the delisting: %w", err)
		}
		steps = append(steps, map[string]any{"state": s.State, "at": s.At.UTC().Format(time.RFC3339)})
	}
	e := audit.Of(origin, audit.ActionCurrencyDelistingSchedule, audit.TargetCurrency, currencyHK)
	e.Changes = &audit.Changes{After: map[string]any{"delisting": hk, "kind": p.Kind, "steps": steps, "settlement": p.Settlement}}
	if err := audit.Record(ctx, tx, e); err != nil {
		return Delisting{}, Change{}, err
	}
	public, err := announce(ctx, tx, currencyHK, cur)
	if err != nil {
		return Delisting{}, Change{}, err
	}
	d, err := get(ctx, tx, currencyHK, hk)
	if err != nil {
		return Delisting{}, Change{}, err
	}
	if err := tx.Commit(); err != nil {
		return Delisting{}, Change{}, fmt.Errorf("failed to schedule the delisting: %w", err)
	}
	return d, Change{CurrencyHK: currencyHK, Public: public}, nil
}

// Cancel cancels a scheduled delisting, which returns the currency to
// active, before it reaches frozen. With version, it is refused when the
// delisting has changed since.
func Cancel(ctx context.Context, db *sql.DB, currencyHK, hk string, version *int64, origin audit.Event) (Delisting, Change, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Delisting{}, Change{}, fmt.Errorf("failed to start the cancel: %w", err)
	}
	defer tx.Rollback()
	// The currency first, then the delisting, as every change takes them.
	cur, err := lockCurrency(ctx, tx, currencyHK)
	if errors.Is(err, ErrCurrencyNotFound) {
		return Delisting{}, Change{}, ErrNotFound
	}
	if err != nil {
		return Delisting{}, Change{}, err
	}
	var status Status
	var current int64
	err = tx.QueryRowContext(ctx, `SELECT status, version FROM currency_delistings WHERE hk = $1 AND currency_hk = $2 FOR UPDATE`,
		hk, currencyHK).Scan(&status, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return Delisting{}, Change{}, ErrNotFound
	}
	if err != nil {
		return Delisting{}, Change{}, fmt.Errorf("failed to read the delisting: %w", err)
	}
	if version != nil && *version != current {
		return Delisting{}, Change{}, &VersionConflict{Current: current}
	}
	if status != StatusScheduled {
		return Delisting{}, Change{}, ErrNotScheduled
	}
	if !cur.lifecycle.Reversible() {
		return Delisting{}, Change{}, ErrIrreversible
	}
	if _, err := tx.ExecContext(ctx, `UPDATE currency_delistings SET status = 'cancelled', cancelled_at = NOW(), cancelled_by = $2,
		version = version + 1 WHERE hk = $1`, hk, origin.ActorID); err != nil {
		return Delisting{}, Change{}, fmt.Errorf("failed to cancel the delisting: %w", err)
	}
	if cur.lifecycle != currencies.LifecycleActive {
		if _, err := tx.ExecContext(ctx, `UPDATE currencies SET lifecycle = 'active', lifecycle_changed_at = NOW() WHERE hk = $1`,
			currencyHK); err != nil {
			return Delisting{}, Change{}, fmt.Errorf("failed to restore the currency: %w", err)
		}
	}
	e := audit.Of(origin, audit.ActionCurrencyDelistingCancel, audit.TargetCurrency, currencyHK)
	e.Changes = &audit.Changes{
		Before: map[string]any{"delisting": hk, "lifecycle": cur.lifecycle},
		After:  map[string]any{"delisting": hk, "lifecycle": currencies.LifecycleActive},
	}
	if err := audit.Record(ctx, tx, e); err != nil {
		return Delisting{}, Change{}, err
	}
	public, err := announce(ctx, tx, currencyHK, cur)
	if err != nil {
		return Delisting{}, Change{}, err
	}
	d, err := get(ctx, tx, currencyHK, hk)
	if err != nil {
		return Delisting{}, Change{}, err
	}
	if err := tx.Commit(); err != nil {
		return Delisting{}, Change{}, fmt.Errorf("failed to cancel the delisting: %w", err)
	}
	return d, Change{CurrencyHK: currencyHK, Public: public}, nil
}

// Override sets a capability of a currency, whatever its lifecycle says, or
// with allowed nil removes the override.
func Override(ctx context.Context, db *sql.DB, currencyHK string, k currencies.Capability, allowed *bool, note string, origin audit.Event) (*models.CurrencyLifecycle, Change, error) {
	if !slices.Contains(currencies.Capabilities, k) {
		return nil, Change{}, fmt.Errorf("unknown capability %q", k)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, Change{}, fmt.Errorf("failed to start the override: %w", err)
	}
	defer tx.Rollback()
	cur, err := lockCurrency(ctx, tx, currencyHK)
	if err != nil {
		return nil, Change{}, err
	}
	var before sql.NullBool
	err = tx.QueryRowContext(ctx, `SELECT allowed FROM currency_capability_overrides WHERE currency_hk = $1 AND capability = $2 AND cleared_at IS NULL`,
		currencyHK, k).Scan(&before)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, Change{}, fmt.Errorf("failed to read the override: %w", err)
	}
	if allowed == nil {
		// The row stays, marked, for the record (#248).
		_, err = tx.ExecContext(ctx, `UPDATE currency_capability_overrides SET cleared_at = NOW(), cleared_by = $3
			WHERE currency_hk = $1 AND capability = $2 AND cleared_at IS NULL`, currencyHK, k, origin.ActorID)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO currency_capability_overrides (currency_hk, capability, allowed, note, created_by)
			VALUES ($1, $2, $3, NULLIF($4, ''), $5)
			ON CONFLICT (currency_hk, capability) DO UPDATE SET allowed = EXCLUDED.allowed, note = EXCLUDED.note,
				created_by = EXCLUDED.created_by, created_at = NOW(), cleared_at = NULL, cleared_by = NULL`, currencyHK, k, *allowed, note, origin.ActorID)
	}
	if err != nil {
		return nil, Change{}, fmt.Errorf("failed to change the override: %w", err)
	}
	value := func(b sql.NullBool) any {
		if !b.Valid {
			return nil
		}
		return b.Bool
	}
	var after sql.NullBool
	if allowed != nil {
		after = sql.NullBool{Bool: *allowed, Valid: true}
	}
	e := audit.Of(origin, audit.ActionCurrencyCapabilityOverride, audit.TargetCurrency, currencyHK)
	e.Changes = &audit.Changes{
		Before: map[string]any{"capability": k, "override": value(before)},
		After:  map[string]any{"capability": k, "override": value(after)},
	}
	if err := audit.Record(ctx, tx, e); err != nil {
		return nil, Change{}, err
	}
	public, err := announce(ctx, tx, currencyHK, cur)
	if err != nil {
		return nil, Change{}, err
	}
	l, err := currencies.ReadLifecycle(ctx, tx, currencyHK, false)
	if err != nil {
		return nil, Change{}, err
	}
	if err := tx.Commit(); err != nil {
		return nil, Change{}, fmt.Errorf("failed to change the override: %w", err)
	}
	return l, Change{CurrencyHK: currencyHK, Public: public}, nil
}

// Advance applies the steps that are due at now, each exactly once on all
// instances, at most limit of them. It returns the changes that it
// committed.
func Advance(ctx context.Context, db *sql.DB, now time.Time, limit int) ([]Change, error) {
	var changes []Change
	for len(changes) < limit {
		change, ok, err := advanceOne(ctx, db, now)
		if err != nil {
			return changes, err
		}
		if !ok {
			break
		}
		if change.CurrencyHK != "" {
			changes = append(changes, change)
		}
	}
	return changes, nil
}

// advanceOne applies the earliest step that is due and that no other
// instance is applying, and reports false when there is none.
func advanceOne(ctx context.Context, db *sql.DB, now time.Time) (Change, bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Change{}, false, fmt.Errorf("failed to start a step: %w", err)
	}
	defer tx.Rollback()
	// The steps of a delisting apply in the order of the lifecycle: only its
	// earliest step that is not applied can be claimed, so while one instance
	// holds it, another skips the delisting rather than claim a later step
	// and apply it first. Only the scheduler
	// locks steps, so claiming one first, and then the currency and the
	// delisting, in the order of every other change, cannot deadlock.
	var delistingHK, currencyHK string
	var state currencies.Lifecycle
	err = tx.QueryRowContext(ctx, `
		SELECT s.delisting_hk, s.state, d.currency_hk
		FROM currency_delisting_steps s JOIN currency_delistings d ON d.hk = s.delisting_hk
		WHERE d.status = 'scheduled' AND s.applied_at IS NULL AND s.scheduled_at <= $1
		  AND NOT EXISTS (SELECT 1 FROM currency_delisting_steps e
		    WHERE e.delisting_hk = s.delisting_hk AND e.applied_at IS NULL AND e.scheduled_at < s.scheduled_at)
		ORDER BY s.scheduled_at, s.delisting_hk
		FOR UPDATE OF s SKIP LOCKED LIMIT 1`, now).Scan(&delistingHK, &state, &currencyHK)
	if errors.Is(err, sql.ErrNoRows) {
		return Change{}, false, nil
	}
	if err != nil {
		return Change{}, false, fmt.Errorf("failed to claim a step: %w", err)
	}
	cur, err := lockCurrency(ctx, tx, currencyHK)
	if errors.Is(err, ErrCurrencyNotFound) {
		// A deleted currency is not delisted further; its delisting ends.
		if _, err := tx.ExecContext(ctx, `UPDATE currency_delistings SET status = 'cancelled', cancelled_at = NOW(),
			cancelled_by = created_by, version = version + 1 WHERE hk = $1`, delistingHK); err != nil {
			return Change{}, false, fmt.Errorf("failed to end the delisting of a deleted currency: %w", err)
		}
		return Change{}, true, tx.Commit()
	}
	if err != nil {
		return Change{}, false, err
	}
	// A cancel or a superseding schedule that came first ends the step.
	var status Status
	if err := tx.QueryRowContext(ctx, `SELECT status FROM currency_delistings WHERE hk = $1 FOR UPDATE`, delistingHK).Scan(&status); err != nil {
		return Change{}, false, fmt.Errorf("failed to lock the delisting: %w", err)
	}
	if status != StatusScheduled {
		return Change{}, true, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE currency_delisting_steps SET applied_at = $3 WHERE delisting_hk = $1 AND state = $2`,
		delistingHK, state, now); err != nil {
		return Change{}, false, fmt.Errorf("failed to apply a step: %w", err)
	}
	if state.Rank() > cur.lifecycle.Rank() {
		if _, err := tx.ExecContext(ctx, `UPDATE currencies SET lifecycle = $2, lifecycle_changed_at = $3 WHERE hk = $1`,
			currencyHK, state, now); err != nil {
			return Change{}, false, fmt.Errorf("failed to apply a step: %w", err)
		}
	}
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM currency_delisting_steps WHERE delisting_hk = $1 AND applied_at IS NULL)`,
		delistingHK).Scan(&pending); err != nil {
		return Change{}, false, fmt.Errorf("failed to read the steps: %w", err)
	}
	// A delisting completes at delisted. One that stops before, such as an
	// emergency halt, stays scheduled, so that it can still be cancelled to
	// relist the currency, until it is cancelled or a new delisting takes
	// the currency further.
	if !pending && state == currencies.LifecycleDelisted {
		if _, err := tx.ExecContext(ctx, `UPDATE currency_delistings SET status = 'completed', completed_at = $2, version = version + 1
			WHERE hk = $1`, delistingHK, now); err != nil {
			return Change{}, false, fmt.Errorf("failed to complete the delisting: %w", err)
		}
	}
	e := audit.Of(audit.Event{}, audit.ActionCurrencyLifecycleChange, audit.TargetCurrency, currencyHK)
	e.Changes = &audit.Changes{
		Before: map[string]any{"lifecycle": cur.lifecycle},
		After:  map[string]any{"lifecycle": state, "delisting": delistingHK},
	}
	if err := audit.Record(ctx, tx, e); err != nil {
		return Change{}, false, err
	}
	public, err := announce(ctx, tx, currencyHK, cur)
	if err != nil {
		return Change{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Change{}, false, fmt.Errorf("failed to apply a step: %w", err)
	}
	return Change{CurrencyHK: currencyHK, Public: public}, true, nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

const columns = `hk, currency_hk, kind, status, settlement, settlement_currency_hk, COALESCE(price_source, ''),
	trim_scale(fixed_price)::TEXT, claim_days, COALESCE(announcement_url, ''), COALESCE(note, ''), created_by, created_at,
	cancelled_at, cancelled_by, completed_at, version`

// List returns the delistings of a currency, newest first, with their
// steps.
func List(ctx context.Context, q querier, currencyHK string) ([]Delisting, error) {
	return list(ctx, q, `SELECT `+columns+` FROM currency_delistings WHERE currency_hk = $1 ORDER BY created_at DESC, hk DESC LIMIT 100`, currencyHK)
}

func get(ctx context.Context, q querier, currencyHK, hk string) (Delisting, error) {
	found, err := list(ctx, q, `SELECT `+columns+` FROM currency_delistings WHERE currency_hk = $1 AND hk = $2`, currencyHK, hk)
	if err != nil {
		return Delisting{}, err
	}
	if len(found) == 0 {
		return Delisting{}, ErrNotFound
	}
	return found[0], nil
}

func list(ctx context.Context, q querier, query string, args ...any) ([]Delisting, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to read the delistings: %w", err)
	}
	out := []Delisting{}
	for rows.Next() {
		var d Delisting
		var claim sql.NullInt64
		if err := rows.Scan(&d.ID, &d.CurrencyHK, &d.Kind, &d.Status, &d.Settlement, &d.SettlementCurrencyHK, &d.PriceSource,
			&d.FixedPrice, &claim, &d.AnnouncementURL, &d.Note, &d.CreatedBy, &d.CreatedAt,
			&d.CancelledAt, &d.CancelledBy, &d.CompletedAt, &d.Version); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to read the delistings: %w", err)
		}
		if claim.Valid {
			n := int(claim.Int64)
			d.ClaimDays = &n
		}
		d.Steps = []Step{}
		out = append(out, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read the delistings: %w", err)
	}
	for i := range out {
		steps, err := q.QueryContext(ctx, `SELECT state, scheduled_at, applied_at FROM currency_delisting_steps
			WHERE delisting_hk = $1 ORDER BY scheduled_at`, out[i].ID)
		if err != nil {
			return nil, fmt.Errorf("failed to read the steps: %w", err)
		}
		for steps.Next() {
			var s Step
			if err := steps.Scan(&s.State, &s.At, &s.AppliedAt); err != nil {
				steps.Close()
				return nil, fmt.Errorf("failed to read the steps: %w", err)
			}
			out[i].Steps = append(out[i].Steps, s)
		}
		steps.Close()
		if err := steps.Err(); err != nil {
			return nil, fmt.Errorf("failed to read the steps: %w", err)
		}
	}
	return out, nil
}
