package userauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/google/uuid"
)

// Account recovery (#307): how a user who lost the account password and
// the recovery codes gets the account back. A recovery proves every
// verified contact of the account with a code; in the strict mode the owner
// also proves their identity again. A proved recovery then waits, and only
// after the wait can it set a new password. Proving the password at any
// time cancels it, so the owner of an account stops a recovery that someone
// else started by signing in.

// The modes and the identity steps of a recovery.
const (
	RecoveryStandard = config.RecoveryStandard
	RecoveryStrict   = config.RecoveryStrict

	StepNationalID      = config.RecoveryStepNationalID
	StepMobileOwnership = config.RecoveryStepMobileOwnership
)

// The states of a recovery.
const (
	RecoveryVerifying = "verifying"
	RecoveryWaiting   = "waiting"
	RecoveryCompleted = "completed"
	RecoveryCancelled = "cancelled"
	RecoveryFailed    = "failed"
)

const (
	// recoveryProofWindow is how long a recovery has to prove the
	// contacts and the identity.
	recoveryProofWindow = 24 * time.Hour
	// recoveryCompleteWindow is how long a recovery can set the new
	// password once its wait has ended.
	recoveryCompleteWindow = 7 * 24 * time.Hour
	// MaxRecoveryIdentityFailures is how many failed identity checks end
	// a recovery.
	MaxRecoveryIdentityFailures = 3
	// recoveryCooldown is how long after a recovery that failed its
	// identity checks the account cannot start another.
	recoveryCooldown = 24 * time.Hour

	reasonRecoveryExpired  = "expired"
	reasonRecoveryIdentity = "identity_failed"
	// ReasonRecoveryPasswordProven ends a recovery of an account whose
	// password was proved meanwhile.
	ReasonRecoveryPasswordProven = "password_proven"
	// ReasonRecoveryFactorProven ends one when another second factor of
	// the account was proved.
	ReasonRecoveryFactorProven  = "second_factor_proven"
	reasonRecoveryPasswordReset = "password_reset"
)

// The refusals of a recovery.
var (
	// ErrNoRecovery reports an account without a recovery that waits.
	ErrNoRecovery = errors.New("the account has no recovery in progress")
	// ErrRecoveryContact reports a contact that the recovery cannot use:
	// it is not a verified contact of the account, or the recovery did
	// not prove it.
	ErrRecoveryContact = errors.New("the contact is not part of the recovery")
	// ErrRecoveryIdentity reports an identity check that failed. The
	// recovery goes on while it has attempts left.
	ErrRecoveryIdentity = errors.New("the identity check of the recovery failed")
	// ErrRecoveryFailed reports a recovery that its failed identity
	// checks ended.
	ErrRecoveryFailed = errors.New("the recovery failed its identity checks")
)

// RecoveryCooldownError reports an account that cannot start a recovery
// until Until, after one that failed.
type RecoveryCooldownError struct{ Until time.Time }

func (e *RecoveryCooldownError) Error() string {
	return "no recovery until " + e.Until.UTC().Format(time.RFC3339)
}

// RecoveryNotReadyError reports a recovery whose wait ends at ReadyAt, or
// which has more to prove when ReadyAt is zero.
type RecoveryNotReadyError struct{ ReadyAt time.Time }

func (e *RecoveryNotReadyError) Error() string { return "the recovery is not ready" }

// Recovery is the state of an open recovery.
type Recovery struct {
	ID string `json:"id"`
	// Mode is "standard" or "strict".
	Mode string `json:"mode" example:"standard"`
	// Status is "verifying" while there is more to prove, and "waiting"
	// once everything is proved.
	Status string `json:"status" example:"waiting"`
	// ContactsRequired and ContactsProven count the verified email
	// addresses and mobile numbers of the account, and those of them that
	// the recovery proved.
	ContactsRequired int `json:"contacts_required" example:"2"`
	ContactsProven   int `json:"contacts_proven" example:"2"`
	// StepsRequired are the identity steps; StepsRemaining those that
	// have not passed.
	StepsRequired  []string `json:"steps_required"`
	StepsRemaining []string `json:"steps_remaining"`
	// IdentityAttemptsLeft is how many identity checks may still fail.
	IdentityAttemptsLeft int       `json:"identity_attempts_left" example:"3"`
	StartedAt            time.Time `json:"started_at"`
	// ReadyAt is when the wait ends and the new password can be set.
	ReadyAt *time.Time `json:"ready_at,omitempty"`
	// ExpiresAt is when the recovery stops being usable.
	ExpiresAt time.Time `json:"expires_at"`
}

// RecoveryProof is what one request proved for a recovery.
type RecoveryProof struct {
	// ContactID is the contact whose code was checked.
	ContactID int64
	// StepsPassed are the identity steps that passed in this request.
	StepsPassed []string
	// IdentityFailed reports an identity step that was tried and failed.
	// Nothing else of the request counts then.
	IdentityFailed bool
}

// RecoveryPlan returns the mode and the identity steps of a recovery that
// starts now. An account without a verified national ID has no identity to
// check again, and recovers in the standard mode.
func RecoveryPlan(hasNationalID bool) (string, []string) {
	cfg := settings().Security.Recovery
	if cfg.Mode != RecoveryStrict || !hasNationalID {
		return RecoveryStandard, nil
	}
	steps := cfg.Steps
	if len(steps) == 0 {
		steps = []string{StepNationalID, StepMobileOwnership}
	}
	return RecoveryStrict, slices.Clone(steps)
}

// recoveryWait returns how long a proved recovery waits.
func (p *Passwords) recoveryWait() time.Duration {
	if p.RecoveryWait > 0 {
		return p.RecoveryWait
	}
	hours := settings().Security.Recovery.WaitHours
	if hours == 0 {
		hours = config.DefaultRecoveryWaitHours
	}
	return time.Duration(hours) * time.Hour
}

func splitList(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(s, ",")
}

// recoveryQuerier reads rows: a *sql.DB or a *sql.Tx.
type recoveryQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// openRecovery reads the open recovery of a user, and nil when there is
// none. An open recovery past its time counts as none. forUpdate locks it.
func openRecovery(ctx context.Context, q recoveryQuerier, userHK string, forUpdate bool) (*Recovery, error) {
	query := `SELECT r.id, r.mode, r.status, array_to_string(r.steps_required, ','), array_to_string(r.steps_passed, ','),
			r.identity_failures, r.started_at, r.ready_at, r.expires_at,
			(SELECT COUNT(*) FROM user_contacts c WHERE c.user_hk = r.user_hk AND c.removed_at IS NULL AND c.verified_at IS NOT NULL AND c.type IN ('email', 'mobile')),
			(SELECT COUNT(*) FROM user_contacts c WHERE c.user_hk = r.user_hk AND c.removed_at IS NULL AND c.verified_at IS NOT NULL
				AND c.type IN ('email', 'mobile') AND c.id = ANY (r.proven_contacts)),
			(SELECT COUNT(*) FROM user_contacts c WHERE c.user_hk = r.user_hk AND c.removed_at IS NULL AND c.verified_at IS NOT NULL
				AND c.type = 'mobile'),
			` + rowseal.AccountRecoveries.Fields("r") + `
		FROM account_recoveries r
		WHERE r.user_hk = $1 AND r.status IN ('verifying', 'waiting') AND r.expires_at > NOW()`
	if forUpdate {
		query += ` FOR UPDATE OF r`
	}
	var r Recovery
	var required, passed string
	var failures, mobiles int
	var readyAt sql.NullTime
	row := rowseal.AccountRecoveries.NewRow()
	err := q.QueryRowContext(ctx, query, userHK).Scan(append([]any{&r.ID, &r.Mode, &r.Status, &required, &passed, &failures,
		&r.StartedAt, &readyAt, &r.ExpiresAt, &r.ContactsRequired, &r.ContactsProven, &mobiles}, row.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the recovery: %w", err)
	}
	// A recovery changed in SQL, such as one moved to waiting, or with
	// contacts it did not prove, proves nothing (#337).
	if err := rowseal.Check(ctx, rowseal.AccountRecoveries, row); err != nil {
		return nil, err
	}
	if readyAt.Valid {
		r.ReadyAt = &readyAt.Time
	}
	r.StepsRequired = splitList(required)
	done := splitList(passed)
	r.StepsRemaining = []string{}
	for _, step := range r.StepsRequired {
		// An account without a mobile number has none to check.
		if slices.Contains(done, step) || (step == StepMobileOwnership && mobiles == 0) {
			continue
		}
		r.StepsRemaining = append(r.StepsRemaining, step)
	}
	r.IdentityAttemptsLeft = max(0, MaxRecoveryIdentityFailures-failures)
	return &r, nil
}

// OpenRecovery returns the open recovery of a user, and nil when there is
// none.
func (p *Passwords) OpenRecovery(ctx context.Context, userHK string) (*Recovery, error) {
	return openRecovery(ctx, p.DB, userHK, false)
}

// sealRecovery seals, in tx, the recovery that a write of tx just changed
// (#337).
func sealRecovery(ctx context.Context, tx *sql.Tx, id string) error {
	return rowseal.Reseal(ctx, tx, rowseal.AccountRecoveries, "id = $1", id)
}

// endRecovery ends the open recovery of a user within tx, and records it.
// It reports whether there was one.
func endRecovery(ctx context.Context, tx *sql.Tx, userHK, status, reason string, event audit.Event) (bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, `UPDATE account_recoveries SET status = $2, ended_at = NOW(), end_reason = $3
		WHERE user_hk = $1 AND status IN ('verifying', 'waiting') RETURNING id`, userHK, status, reason).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("end the recovery: %w", err)
	}
	if err := sealRecovery(ctx, tx, id); err != nil {
		return false, err
	}
	event.Action, event.Outcome, event.Reason = audit.ActionUserRecoveryCancel, audit.Success, reason
	if event.ActorType == "" {
		event.ActorType, event.ActorID = audit.ActorUser, userHK
	}
	event.TargetType, event.TargetID = audit.TargetAccountRecovery, id
	if err := audit.Record(ctx, tx, event); err != nil {
		return false, fmt.Errorf("record the end of the recovery: %w", err)
	}
	return true, nil
}

// cancelRecovery cancels the open recovery of a user whose password was
// just proved. Most accounts have none, and nothing is written for them.
func (p *Passwords) cancelRecovery(ctx context.Context, userHK string) error {
	var open bool
	if err := p.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM account_recoveries
		WHERE user_hk = $1 AND status IN ('verifying', 'waiting'))`, userHK).Scan(&open); err != nil {
		return fmt.Errorf("read the recovery: %w", err)
	}
	if !open {
		return nil
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("start the cancel of the recovery: %w", err)
	}
	defer tx.Rollback()
	if _, err := endRecovery(ctx, tx, userHK, RecoveryCancelled, ReasonRecoveryPasswordProven, audit.Event{}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("cancel the recovery: %w", err)
	}
	return nil
}

// AdvanceRecovery starts the recovery of a user, or adds what a request
// proved to the open one. When every verified contact and every identity
// step is proved, the recovery starts to wait. A failed identity check
// answers ErrRecoveryIdentity with the state, and ErrRecoveryFailed when it
// was the last one allowed.
func (p *Passwords) AdvanceRecovery(ctx context.Context, userHK string, proof RecoveryProof, plan func() (string, []string), event audit.Event) (*Recovery, error) {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("start the recovery: %w", err)
	}
	defer tx.Rollback()
	// The user row orders the recovery with the password changes.
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM users WHERE hk = $1 FOR UPDATE`, userHK); err != nil {
		return nil, fmt.Errorf("lock the user: %w", err)
	}
	// The record of the users row decides (#337): a password row deleted in
	// SQL is a finding, not an account without a password.
	hasPassword, err := has(ctx, tx, userHK)
	if err != nil {
		return nil, err
	}
	if !hasPassword {
		return nil, ErrNoPassword
	}
	var verified bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM user_contacts
		WHERE id = $2 AND user_hk = $1 AND removed_at IS NULL AND verified_at IS NOT NULL)`, userHK, proof.ContactID).Scan(&verified); err != nil {
		return nil, fmt.Errorf("read the contact: %w", err)
	}
	if !verified {
		return nil, ErrRecoveryContact
	}
	// A recovery past its time makes room for a new one.
	expired, err := tx.QueryContext(ctx, `UPDATE account_recoveries SET status = 'failed', ended_at = NOW(), end_reason = $2
		WHERE user_hk = $1 AND status IN ('verifying', 'waiting') AND expires_at <= NOW() RETURNING id`, userHK, reasonRecoveryExpired)
	if err != nil {
		return nil, fmt.Errorf("end the expired recovery: %w", err)
	}
	var ended []string
	for expired.Next() {
		var id string
		if err := expired.Scan(&id); err != nil {
			expired.Close()
			return nil, fmt.Errorf("end the expired recovery: %w", err)
		}
		ended = append(ended, id)
	}
	expired.Close()
	if err := expired.Err(); err != nil {
		return nil, fmt.Errorf("end the expired recovery: %w", err)
	}
	for _, id := range ended {
		if err := sealRecovery(ctx, tx, id); err != nil {
			return nil, err
		}
	}

	event.ActorType, event.ActorID = audit.ActorUser, userHK
	event.TargetType = audit.TargetAccountRecovery
	current, err := openRecovery(ctx, tx, userHK, true)
	if err != nil {
		return nil, err
	}
	if current == nil {
		var until sql.NullTime
		if err := tx.QueryRowContext(ctx, `SELECT MAX(ended_at) + make_interval(secs => $3) FROM account_recoveries
			WHERE user_hk = $1 AND status = 'failed' AND end_reason = $2
				AND ended_at + make_interval(secs => $3) > NOW()`,
			userHK, reasonRecoveryIdentity, recoveryCooldown.Seconds()).Scan(&until); err != nil {
			return nil, fmt.Errorf("read the last recovery: %w", err)
		}
		if until.Valid {
			return nil, &RecoveryCooldownError{Until: until.Time}
		}
		mode, steps := plan()
		id := uuid.NewString()
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_recoveries (id, user_hk, mode, steps_required, expires_at, ip, user_agent)
			VALUES ($1, $2, $3, COALESCE(string_to_array(NULLIF($4, ''), ','), '{}'), NOW() + make_interval(secs => $5), NULLIF($6, '')::inet, NULLIF($7, ''))`,
			id, userHK, mode, strings.Join(steps, ","), recoveryProofWindow.Seconds(), event.IP, event.UserAgent); err != nil {
			return nil, fmt.Errorf("start the recovery: %w", err)
		}
		if err := sealRecovery(ctx, tx, id); err != nil {
			return nil, err
		}
		start := event
		start.Action, start.Outcome, start.TargetID, start.Reason = audit.ActionUserRecoveryStart, audit.Success, id, mode
		if err := audit.Record(ctx, tx, start); err != nil {
			return nil, fmt.Errorf("record the recovery: %w", err)
		}
		if current, err = openRecovery(ctx, tx, userHK, true); err != nil || current == nil {
			return nil, fmt.Errorf("read the new recovery: %w", errors.Join(err, sql.ErrNoRows))
		}
	}
	event.TargetID = current.ID

	if proof.IdentityFailed && current.Status == RecoveryVerifying {
		left := current.IdentityAttemptsLeft - 1
		failure := event
		failure.Action, failure.Outcome, failure.Reason = audit.ActionUserRecoveryIdentity, audit.Failure, "identity_mismatch"
		if _, err := tx.ExecContext(ctx, `UPDATE account_recoveries SET identity_failures = identity_failures + 1 WHERE id = $1`, current.ID); err != nil {
			return nil, fmt.Errorf("count the identity failure: %w", err)
		}
		if err := sealRecovery(ctx, tx, current.ID); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, failure); err != nil {
			return nil, fmt.Errorf("record the identity failure: %w", err)
		}
		result := ErrRecoveryIdentity
		if left <= 0 {
			if _, err := endRecovery(ctx, tx, userHK, RecoveryFailed, reasonRecoveryIdentity, event); err != nil {
				return nil, err
			}
			result = ErrRecoveryFailed
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("count the identity failure: %w", err)
		}
		current.IdentityAttemptsLeft = max(0, left)
		return current, result
	}

	if current.Status == RecoveryVerifying {
		if _, err := tx.ExecContext(ctx, `UPDATE account_recoveries SET
				proven_contacts = CASE WHEN $2 = ANY (proven_contacts) THEN proven_contacts ELSE array_append(proven_contacts, $2) END,
				steps_passed = ARRAY(SELECT DISTINCT s FROM unnest(steps_passed || string_to_array(NULLIF($3, ''), ',')) AS s WHERE s IS NOT NULL)
			WHERE id = $1`, current.ID, proof.ContactID, strings.Join(proof.StepsPassed, ",")); err != nil {
			return nil, fmt.Errorf("store the proof of the recovery: %w", err)
		}
		if err := sealRecovery(ctx, tx, current.ID); err != nil {
			return nil, err
		}
		if current, err = openRecovery(ctx, tx, userHK, true); err != nil || current == nil {
			return nil, fmt.Errorf("read the recovery: %w", errors.Join(err, sql.ErrNoRows))
		}
		if current.ContactsProven >= current.ContactsRequired && len(current.StepsRemaining) == 0 {
			wait := p.recoveryWait()
			if _, err := tx.ExecContext(ctx, `UPDATE account_recoveries SET status = 'waiting',
					ready_at = NOW() + make_interval(secs => $2), expires_at = NOW() + make_interval(secs => $3)
				WHERE id = $1`, current.ID, wait.Seconds(), (wait + recoveryCompleteWindow).Seconds()); err != nil {
				return nil, fmt.Errorf("start the wait of the recovery: %w", err)
			}
			if err := sealRecovery(ctx, tx, current.ID); err != nil {
				return nil, err
			}
			if current, err = openRecovery(ctx, tx, userHK, true); err != nil || current == nil {
				return nil, fmt.Errorf("read the recovery: %w", errors.Join(err, sql.ErrNoRows))
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store the recovery: %w", err)
	}
	return current, nil
}

// CompleteRecovery sets a new password with a recovery whose wait has
// ended. contactID is the contact whose code the request proved; it must be
// one that the recovery proved. The account gets new recovery codes, which
// are returned, and the security hold starts. The sessions of the user are
// the caller's to end.
func (p *Passwords) CompleteRecovery(ctx context.Context, userHK string, contactID int64, password string, event audit.Event) (SetResult, error) {
	if err := CheckPolicy(password, p.MinPasswordLength()); err != nil {
		return SetResult{}, err
	}
	// Whether the recovery is ready is read before the slow function runs.
	ready := func(q recoveryQuerier, lock bool) (*Recovery, error) {
		r, err := openRecovery(ctx, q, userHK, lock)
		if err != nil {
			return nil, err
		}
		if r == nil {
			return nil, ErrNoRecovery
		}
		var proven, due bool
		if err := q.QueryRowContext(ctx, `SELECT $2 = ANY (proven_contacts), status = 'waiting' AND ready_at <= NOW()
			FROM account_recoveries WHERE id = $1`, r.ID, contactID).Scan(&proven, &due); err != nil {
			return nil, fmt.Errorf("read the recovery: %w", err)
		}
		if !proven {
			return nil, ErrRecoveryContact
		}
		if !due {
			notReady := &RecoveryNotReadyError{}
			if r.ReadyAt != nil {
				notReady.ReadyAt = *r.ReadyAt
			}
			return nil, notReady
		}
		return r, nil
	}
	if _, err := ready(p.DB, false); err != nil {
		return SetResult{}, err
	}
	hash, err := p.Hasher.Hash(ctx, password)
	if err != nil {
		return SetResult{}, err
	}

	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return SetResult{}, fmt.Errorf("start the completion of the recovery: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM users WHERE hk = $1 FOR UPDATE`, userHK); err != nil {
		return SetResult{}, fmt.Errorf("lock the user: %w", err)
	}
	// Read again under the lock: a password that was proved meanwhile
	// cancelled the recovery.
	recovery, err := ready(tx, true)
	if err != nil {
		return SetResult{}, err
	}
	// The row is checked like at a sign-in: a recovery never seals over a
	// hash that was written outside the backend (#139).
	row := rowseal.UserPasswords.NewRow()
	err = tx.QueryRowContext(ctx, `SELECT `+rowseal.UserPasswords.Fields("")+` FROM user_passwords WHERE user_hk = $1 FOR UPDATE`, userHK).Scan(row.Dest()...)
	if errors.Is(err, sql.ErrNoRows) {
		return SetResult{}, ErrNoPassword
	}
	if err != nil {
		return SetResult{}, fmt.Errorf("read the password: %w", err)
	}
	if err := rowseal.Check(ctx, rowseal.UserPasswords, row); err != nil {
		return SetResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE user_passwords SET password_hash = $2, changed_at = NOW(),
		failed_attempts = 0, locked_until = NULL WHERE user_hk = $1`, userHK, hash); err != nil {
		return SetResult{}, fmt.Errorf("store the password: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.UserPasswords, "user_hk = $1", userHK); err != nil {
		return SetResult{}, err
	}
	// StartSecurityHold, below, seals the record.
	if err := recordPassword(ctx, tx, userHK); err != nil {
		return SetResult{}, err
	}
	codes, err := replaceRecoveryCodes(ctx, tx, userHK)
	if err != nil {
		return SetResult{}, err
	}
	holdUntil, err := restrictions.StartSecurityHold(ctx, tx, userHK, holdDuration())
	if err != nil {
		return SetResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE account_recoveries SET status = 'completed', ended_at = NOW(), end_reason = 'completed'
		WHERE id = $1`, recovery.ID); err != nil {
		return SetResult{}, fmt.Errorf("complete the recovery: %w", err)
	}
	if err := sealRecovery(ctx, tx, recovery.ID); err != nil {
		return SetResult{}, err
	}
	event.Action, event.Outcome, event.Reason = audit.ActionUserRecoveryComplete, audit.Success, recovery.Mode
	event.ActorType, event.ActorID = audit.ActorUser, userHK
	event.TargetType, event.TargetID = audit.TargetAccountRecovery, recovery.ID
	if err := audit.Record(ctx, tx, event); err != nil {
		return SetResult{}, fmt.Errorf("record the recovery: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SetResult{}, fmt.Errorf("complete the recovery: %w", err)
	}
	return SetResult{Changed: true, HoldUntil: holdUntil, RecoveryCodes: codes}, nil
}

// CancelRecoveryFor ends the open recovery of a user within tx, the
// transaction in which a second factor of the account, other than the
// password, was proved.
func CancelRecoveryFor(ctx context.Context, tx *sql.Tx, userHK string) error {
	_, err := endRecovery(ctx, tx, userHK, RecoveryCancelled, ReasonRecoveryFactorProven, audit.Event{})
	return err
}
