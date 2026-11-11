package userauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// The refusals of a password check or change.
var (
	// ErrNoPassword reports an account without a password.
	ErrNoPassword = errors.New("the account has no password")
	// ErrWrongPassword reports a password that does not match.
	ErrWrongPassword = errors.New("the password is wrong")
	// ErrCurrentRequired reports a change of a password without the
	// current one.
	ErrCurrentRequired = errors.New("the current password is required")
	// ErrSamePassword reports a change to the password the account has.
	ErrSamePassword = errors.New("the new password is the current one")
)

// LockedError reports an account whose password had too many wrong
// attempts. No password is checked until Until.
type LockedError struct{ Until time.Time }

func (e *LockedError) Error() string {
	return "too many wrong passwords; locked until " + e.Until.UTC().Format(time.RFC3339)
}

// Passwords keeps and checks the account passwords of users. The attempt
// counts are in PostgreSQL, so that every instance shares them and a
// failure of Redis cannot open them.
type Passwords struct {
	DB *sql.DB
	// Hasher hashes and checks the passwords.
	Hasher Hasher
	// MinLength, MaxFailures, and Lock replace the settings of
	// security.password when set: the least length of a new password in
	// characters, and how many wrong passwords in a row lock the password
	// for how long.
	MinLength   int
	MaxFailures int
	Lock        time.Duration
	// RecoveryWait replaces security.recovery.wait_hours when set.
	RecoveryWait time.Duration
}

// NewPasswords returns the store with the settings of security.password,
// read at each use; a setting that is not set takes its default.
func NewPasswords(db *sql.DB, hasher Hasher) *Passwords { return &Passwords{DB: db, Hasher: hasher} }

// settings returns the limits in force.
func (p *Passwords) settings() (minLength, maxFailures int, lock time.Duration) {
	cfg := config.Config.Security.Password
	minLength, maxFailures, lock = p.MinLength, p.MaxFailures, p.Lock
	if minLength == 0 {
		if minLength = cfg.MinLength; minLength == 0 {
			minLength = config.DefaultPasswordMinLength
		}
	}
	if maxFailures == 0 {
		if maxFailures = cfg.MaxFailures; maxFailures == 0 {
			maxFailures = config.DefaultPasswordMaxFailures
		}
	}
	if lock == 0 {
		minutes := cfg.LockMinutes
		if minutes == 0 {
			minutes = config.DefaultPasswordLockMinutes
		}
		lock = time.Duration(minutes) * time.Minute
	}
	return minLength, maxFailures, lock
}

// MinPasswordLength is the least length of a new password, in characters.
func (p *Passwords) MinPasswordLength() int {
	minLength, _, _ := p.settings()
	return minLength
}

// Status is what a user may know about their password.
type Status struct {
	// Set reports whether the account has a password.
	Set bool `json:"set"`
	// ChangedAt is when it was set or last changed.
	ChangedAt *time.Time `json:"changed_at,omitempty"`
}

// Status returns the password status of a user.
func (p *Passwords) Status(ctx context.Context, userHK string) (Status, error) {
	var changed time.Time
	err := p.DB.QueryRowContext(ctx, `SELECT changed_at FROM user_passwords WHERE user_hk = $1`, userHK).Scan(&changed)
	if errors.Is(err, sql.ErrNoRows) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, fmt.Errorf("read the password status: %w", err)
	}
	return Status{Set: true, ChangedAt: &changed}, nil
}

// Has reports whether a user has a password, which their sign-in then
// needs.
func (p *Passwords) Has(ctx context.Context, userHK string) (bool, error) {
	status, err := p.Status(ctx, userHK)
	return status.Set, err
}

// attempt counts an attempt before the password is checked, so that
// concurrent guesses cannot pass the limit, and returns the sealed hash. It
// refuses with a LockedError while the password is locked. The attempt that
// reaches MaxFailures is still checked, and locks the password unless it is
// right.
func (p *Passwords) attempt(ctx context.Context, userHK string) (string, error) {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("start the password attempt: %w", err)
	}
	defer tx.Rollback()

	row := rowseal.UserPasswords.NewRow()
	var failures int
	var lockedUntil sql.NullTime
	var now time.Time
	dest := append([]any{&failures, &lockedUntil, &now}, row.Dest()...)
	err = tx.QueryRowContext(ctx, `SELECT failed_attempts, locked_until, NOW(), `+rowseal.UserPasswords.Fields("")+`
		FROM user_passwords WHERE user_hk = $1 FOR UPDATE`, userHK).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoPassword
	}
	if err != nil {
		return "", fmt.Errorf("read the password: %w", err)
	}
	// A hash that was written outside the backend proves nothing (#139).
	if err := rowseal.Check(ctx, rowseal.UserPasswords, row); err != nil {
		return "", err
	}
	if lockedUntil.Valid {
		if lockedUntil.Time.After(now) {
			return "", &LockedError{Until: lockedUntil.Time}
		}
		failures, lockedUntil = 0, sql.NullTime{}
	}
	failures++
	_, maxFailures, lock := p.settings()
	if failures >= maxFailures {
		lockedUntil = sql.NullTime{Time: now.Add(lock), Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE user_passwords SET failed_attempts = $2, locked_until = $3 WHERE user_hk = $1`,
		userHK, failures, lockedUntil); err != nil {
		return "", fmt.Errorf("count the password attempt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("count the password attempt: %w", err)
	}
	hash := row.Values[0].String
	return hash, nil
}

// Verify checks the password of a user. It returns ErrNoPassword,
// ErrWrongPassword, or a *LockedError; nil means the password is right.
func (p *Passwords) Verify(ctx context.Context, userHK, password string) error {
	hash, err := p.attempt(ctx, userHK)
	if err != nil {
		return err
	}
	// The attempt is counted, and the password is not locked: only now is
	// the slow function run. A password over the most bytes cannot match.
	if len(password) > MaxPasswordBytes {
		return ErrWrongPassword
	}
	match, outdated, err := p.Hasher.Verify(ctx, hash, password)
	if err != nil {
		return err
	}
	if !match {
		return ErrWrongPassword
	}
	// A right password clears the count, also when it was the attempt
	// that would have locked.
	if _, err := p.DB.ExecContext(ctx, `UPDATE user_passwords SET failed_attempts = 0, locked_until = NULL
		WHERE user_hk = $1 AND password_hash = $2`, userHK, hash); err != nil {
		return fmt.Errorf("clear the password attempts: %w", err)
	}
	// The owner of the account is here: a recovery that waits to replace
	// this password ends (#307). A password that cannot end it is not
	// accepted, so that the recovery cannot outlive the sign-in.
	if err := p.cancelRecovery(ctx, userHK); err != nil {
		return err
	}
	if outdated {
		p.rehash(ctx, userHK, hash, password)
	}
	return nil
}

// rehash stores the password with the current cost settings. It is best
// effort: the old hash keeps working.
func (p *Passwords) rehash(ctx context.Context, userHK, oldHash, password string) {
	hash, err := p.Hasher.Hash(ctx, password)
	if err != nil {
		return
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE user_passwords SET password_hash = $3 WHERE user_hk = $1 AND password_hash = $2`,
		userHK, oldHash, hash); err != nil {
		return
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.UserPasswords, "user_hk = $1", userHK); err != nil {
		return
	}
	_ = tx.Commit()
}

// SetResult is the outcome of setting or changing a password.
type SetResult struct {
	// Changed reports a change of a password that the account had.
	Changed bool
	// HoldUntil is when the security hold that the change started ends.
	HoldUntil time.Time
	// RecoveryCodes are the new recovery codes, with a first password.
	RecoveryCodes []string
}

// Set gives a user a password, or changes it. A change needs the current
// password, which counts as an attempt. It records event, as
// user.password_set or user.password_change, in the transaction, and
// reports whether it was a change.
//
// Either one is a change of the second factor, so it starts the security
// hold (#251) in the same transaction: someone who took over the contacts
// of an account, set its password, and proved it cannot move funds out at
// once.
//
// The first password comes with the recovery codes of the account, which
// reset a forgotten password. They are returned once and never stored.
func (p *Passwords) Set(ctx context.Context, userHK, current, password string, event audit.Event) (SetResult, error) {
	if err := CheckPolicy(password, p.MinPasswordLength()); err != nil {
		return SetResult{}, err
	}
	has, err := p.Has(ctx, userHK)
	if err != nil {
		return SetResult{}, err
	}
	if has {
		if current == "" {
			return SetResult{Changed: true}, ErrCurrentRequired
		}
		if err := p.Verify(ctx, userHK, current); err != nil {
			return SetResult{Changed: true}, err
		}
		if current == password {
			return SetResult{Changed: true}, ErrSamePassword
		}
	}
	hash, err := p.Hasher.Hash(ctx, password)
	if err != nil {
		return SetResult{Changed: has}, err
	}

	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return SetResult{Changed: has}, fmt.Errorf("start the password change: %w", err)
	}
	defer tx.Rollback()
	// The user row orders this change with the other changes of the
	// account, such as a concurrent first password.
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM users WHERE hk = $1 FOR UPDATE`, userHK); err != nil {
		return SetResult{Changed: has}, fmt.Errorf("lock the user: %w", err)
	}
	var inserted bool
	err = tx.QueryRowContext(ctx, `
		INSERT INTO user_passwords (user_hk, password_hash) VALUES ($1, $2)
		ON CONFLICT (user_hk) DO UPDATE SET password_hash = EXCLUDED.password_hash, changed_at = NOW(),
			failed_attempts = 0, locked_until = NULL
		RETURNING (xmax = 0)`, userHK, hash).Scan(&inserted)
	if err != nil {
		return SetResult{Changed: has}, fmt.Errorf("store the password: %w", err)
	}
	if inserted == has {
		// Another request set or changed the password since it was read:
		// what this one proved no longer holds.
		return SetResult{Changed: has}, ErrCurrentRequired
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.UserPasswords, "user_hk = $1", userHK); err != nil {
		return SetResult{Changed: has}, err
	}
	holdUntil, err := restrictions.StartSecurityHold(ctx, tx, userHK, holdDuration())
	if err != nil {
		return SetResult{Changed: has}, err
	}
	var codes []string
	if !has {
		if codes, err = replaceRecoveryCodes(ctx, tx, userHK); err != nil {
			return SetResult{}, err
		}
	}
	event.Action = audit.ActionUserPasswordSet
	if has {
		event.Action = audit.ActionUserPasswordChange
	}
	event.Outcome, event.TargetType, event.TargetID = audit.Success, audit.TargetUser, userHK
	if err := audit.Record(ctx, tx, event); err != nil {
		return SetResult{Changed: has}, fmt.Errorf("record the password change: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SetResult{Changed: has}, fmt.Errorf("store the password: %w", err)
	}
	return SetResult{Changed: has, HoldUntil: holdUntil, RecoveryCodes: codes}, nil
}

// holdDuration returns security.change_hold_hours, or its default.
func holdDuration() time.Duration {
	hours := config.Config.Security.ChangeHoldHours
	if hours == 0 {
		hours = config.DefaultSecurityChangeHoldHours
	}
	return time.Duration(hours) * time.Hour
}

// GenerateRecoveryCodes replaces the recovery codes of a user who has a
// password, and records event, as user.recovery_codes_generate, in the
// transaction. The codes that were not used stop working.
func (p *Passwords) GenerateRecoveryCodes(ctx context.Context, userHK string, event audit.Event) ([]string, error) {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("start the recovery codes: %w", err)
	}
	defer tx.Rollback()
	var has bool
	// The lock of the password row orders this with a reset that uses a
	// code.
	err = tx.QueryRowContext(ctx, `SELECT TRUE FROM user_passwords WHERE user_hk = $1 FOR UPDATE`, userHK).Scan(&has)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoPassword
	}
	if err != nil {
		return nil, fmt.Errorf("read the password: %w", err)
	}
	codes, err := replaceRecoveryCodes(ctx, tx, userHK)
	if err != nil {
		return nil, err
	}
	event.Action, event.Outcome = audit.ActionUserRecoveryCodesGenerate, audit.Success
	event.TargetType, event.TargetID = audit.TargetUser, userHK
	if err := audit.Record(ctx, tx, event); err != nil {
		return nil, fmt.Errorf("record the recovery codes: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store the recovery codes: %w", err)
	}
	return codes, nil
}

// Reset replaces a forgotten password of a user with a recovery code, which
// it uses up. The caller has already checked a code sent to a contact of
// the account. A wrong recovery code counts toward the lock of the
// password, like a wrong password, and the new password is hashed only
// after the code was found. Reset records event, as user.password_reset,
// and starts the security hold, in the transaction, and returns when the
// hold ends. It returns ErrNoPassword, ErrWrongRecoveryCode, a
// *LockedError, or an error of the policy.
func (p *Passwords) Reset(ctx context.Context, userHK, recoveryCode, password string, event audit.Event) (time.Time, error) {
	none := time.Time{}
	if err := CheckPolicy(password, p.MinPasswordLength()); err != nil {
		return none, err
	}
	// The attempt is counted first, and refused while the password is
	// locked, so that the codes cannot be guessed past the limit.
	if _, err := p.attempt(ctx, userHK); err != nil {
		return none, err
	}
	code, ok := normalizeRecoveryCode(recoveryCode)
	if !ok {
		return none, ErrWrongRecoveryCode
	}
	codeHash := hashRecoveryCode(userHK, code)
	var id int64
	err := p.DB.QueryRowContext(ctx, `SELECT id FROM user_recovery_codes
		WHERE user_hk = $1 AND code_hash = $2 AND retired_at IS NULL`, userHK, codeHash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return none, ErrWrongRecoveryCode
	}
	if err != nil {
		return none, fmt.Errorf("read the recovery code: %w", err)
	}
	hash, err := p.Hasher.Hash(ctx, password)
	if err != nil {
		return none, err
	}

	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return none, fmt.Errorf("start the password reset: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM users WHERE hk = $1 FOR UPDATE`, userHK); err != nil {
		return none, fmt.Errorf("lock the user: %w", err)
	}
	// The code resets once: of two requests with it, one retires it.
	used, err := tx.ExecContext(ctx, `UPDATE user_recovery_codes SET retired_at = NOW(), retired_reason = 'used'
		WHERE id = $1 AND retired_at IS NULL`, id)
	if err != nil {
		return none, fmt.Errorf("use the recovery code: %w", err)
	}
	if n, err := used.RowsAffected(); err != nil || n != 1 {
		return none, ErrWrongRecoveryCode
	}
	if _, err := tx.ExecContext(ctx, `UPDATE user_passwords SET password_hash = $2, changed_at = NOW(),
		failed_attempts = 0, locked_until = NULL WHERE user_hk = $1`, userHK, hash); err != nil {
		return none, fmt.Errorf("store the password: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.UserPasswords, "user_hk = $1", userHK); err != nil {
		return none, err
	}
	holdUntil, err := restrictions.StartSecurityHold(ctx, tx, userHK, holdDuration())
	if err != nil {
		return none, err
	}
	event.Action, event.Outcome = audit.ActionUserPasswordReset, audit.Success
	event.TargetType, event.TargetID = audit.TargetUser, userHK
	if err := audit.Record(ctx, tx, event); err != nil {
		return none, fmt.Errorf("record the password reset: %w", err)
	}
	// The account is back with a recovery code: a recovery that waits has
	// nothing left to do.
	if _, err := endRecovery(ctx, tx, userHK, RecoveryCancelled, reasonRecoveryPasswordReset, event); err != nil {
		return none, err
	}
	if err := tx.Commit(); err != nil {
		return none, fmt.Errorf("store the password: %w", err)
	}
	return holdUntil, nil
}
