package userauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/totp"
)

// Authenticator apps (#307): a second step that a user can prove instead of
// the account password. A user enrolls one after proving the password, so
// every account with an authenticator also has a password and its recovery
// codes. The attempt counts and the last accepted time step are in
// PostgreSQL, so every instance shares them.

// The refusals of the authenticator.
var (
	// ErrNoTOTP reports an account without a confirmed authenticator, or
	// without one to confirm.
	ErrNoTOTP = errors.New("the account has no authenticator")
	// ErrTOTPEnabled reports an enrollment of an account that has one.
	ErrTOTPEnabled = errors.New("the account already has an authenticator")
	// ErrWrongTOTP reports a code that is wrong, or was used before.
	ErrWrongTOTP = errors.New("the authenticator code is wrong")
	// ErrNoKeys reports that the data-protection keys, which seal the
	// secrets, are not loaded.
	ErrNoKeys = errors.New("the data-protection keys are not loaded")
)

// FieldSealer encrypts and decrypts fields with the data-protection keys
// (#138). *keys.Live implements it.
type FieldSealer interface {
	SealField(aad, plaintext []byte) ([]byte, error)
	OpenField(aad, sealed []byte) ([]byte, error)
}

// TOTP keeps and checks the authenticators of users.
type TOTP struct {
	DB *sql.DB
	// Sealer seals the secrets. Without it nothing is enrolled or checked.
	Sealer FieldSealer
	// Passwords gives the attempt limits, which the authenticator shares
	// with the password, and cancels a recovery when a code is right.
	Passwords *Passwords
	// Issuer names the exchange in authenticator apps.
	Issuer string
	// Now replaces the clock in tests.
	Now func() time.Time
}

// TOTPEnrollment is a secret that waits for its first code.
type TOTPEnrollment struct {
	// Secret is the secret in base32, for apps that cannot scan.
	Secret string `json:"secret" example:"JBSWY3DPEHPK3PXP"`
	// URI is the otpauth URI, which the client shows as a QR code.
	URI string `json:"uri" example:"otpauth://totp/Exchange:user?secret=JBSWY3DPEHPK3PXP&issuer=Exchange"`
}

func totpAAD(userHK string) []byte { return []byte("user_totp.secret_sealed:" + userHK) }

func (t *TOTP) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Enabled reports whether the user has a confirmed authenticator. The row
// is checked against its seal (#337): in enforce mode, a row changed in SQL,
// such as one turned back on, returns rowseal.ErrUnverified.
func (t *TOTP) Enabled(ctx context.Context, userHK string) (bool, error) {
	row := rowseal.UserTOTP.NewRow()
	err := t.DB.QueryRowContext(ctx, `SELECT `+rowseal.UserTOTP.Fields("")+` FROM user_totp WHERE user_hk = $1`, userHK).Scan(row.Dest()...)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the authenticator: %w", err)
	}
	if err := rowseal.Check(ctx, rowseal.UserTOTP, row); err != nil {
		return false, err
	}
	confirmed := row.Values[rowseal.UserTOTP.Index("confirmed_at")].Valid
	removed := row.Values[rowseal.UserTOTP.Index("removed_at")].Valid
	return confirmed && !removed, nil
}

// Begin gives the user a new secret, which works once Confirm has checked a
// code of it. A secret that was not confirmed is replaced. account names
// the user in the app.
func (t *TOTP) Begin(ctx context.Context, userHK, account string) (TOTPEnrollment, error) {
	if t.Sealer == nil {
		return TOTPEnrollment{}, ErrNoKeys
	}
	secret, err := totp.GenerateSecret()
	if err != nil {
		return TOTPEnrollment{}, err
	}
	sealed, err := t.Sealer.SealField(totpAAD(userHK), secret)
	if err != nil {
		return TOTPEnrollment{}, fmt.Errorf("seal the authenticator secret: %w", err)
	}
	tx, err := t.DB.BeginTx(ctx, nil)
	if err != nil {
		return TOTPEnrollment{}, fmt.Errorf("start the authenticator secret: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO user_totp (user_hk, secret_sealed) VALUES ($1, $2)
		ON CONFLICT (user_hk) DO UPDATE SET secret_sealed = EXCLUDED.secret_sealed, created_at = NOW(),
			confirmed_at = NULL, removed_at = NULL, last_step = 0, failed_attempts = 0, locked_until = NULL
		WHERE user_totp.confirmed_at IS NULL OR user_totp.removed_at IS NOT NULL`, userHK, sealed)
	if err != nil {
		return TOTPEnrollment{}, fmt.Errorf("store the authenticator secret: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return TOTPEnrollment{}, ErrTOTPEnabled
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.UserTOTP, "user_hk = $1", userHK); err != nil {
		return TOTPEnrollment{}, err
	}
	if err := tx.Commit(); err != nil {
		return TOTPEnrollment{}, fmt.Errorf("store the authenticator secret: %w", err)
	}
	issuer := t.Issuer
	if issuer == "" {
		issuer = "Exchange"
	}
	return TOTPEnrollment{Secret: totp.EncodeSecret(secret), URI: totp.ProvisioningURI(issuer, account, secret)}, nil
}

// check counts an attempt and checks code against the secret of the user,
// in one transaction: the check is fast, and the count cannot be raced. A
// code is accepted once. confirmed says which secret is checked: the one in
// use, or the one that waits for its first code. On success it runs then
// within the transaction.
func (t *TOTP) check(ctx context.Context, userHK, code string, confirmed bool, then func(tx *sql.Tx) error) error {
	if t.Sealer == nil {
		return ErrNoKeys
	}
	tx, err := t.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("start the authenticator check: %w", err)
	}
	defer tx.Rollback()
	var sealed []byte
	var lastStep int64
	var failures int
	var lockedUntil sql.NullTime
	var now time.Time
	row := rowseal.UserTOTP.NewRow()
	err = tx.QueryRowContext(ctx, `SELECT secret_sealed, last_step, failed_attempts, locked_until, NOW(), `+rowseal.UserTOTP.Fields("")+`
		FROM user_totp WHERE user_hk = $1 AND removed_at IS NULL AND (confirmed_at IS NOT NULL) = $2 FOR UPDATE`, userHK, confirmed).
		Scan(append([]any{&sealed, &lastStep, &failures, &lockedUntil, &now}, row.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoTOTP
	}
	if err != nil {
		return fmt.Errorf("read the authenticator: %w", err)
	}
	// An authenticator turned on, or back on, in SQL proves nothing (#337).
	if err := rowseal.Check(ctx, rowseal.UserTOTP, row); err != nil {
		return err
	}
	if lockedUntil.Valid && lockedUntil.Time.After(now) {
		return &LockedError{Until: lockedUntil.Time}
	}
	if lockedUntil.Valid {
		failures = 0
	}
	secret, err := t.Sealer.OpenField(totpAAD(userHK), sealed)
	if err != nil {
		// A secret that does not open was not written by the backend.
		return fmt.Errorf("open the authenticator secret: %w", err)
	}
	step, ok := totp.Verify(secret, code, t.now())
	if !ok || step <= lastStep {
		failures++
		var lock sql.NullTime
		_, maxFailures, lockFor := t.limits()
		if failures >= maxFailures {
			lock = sql.NullTime{Time: now.Add(lockFor), Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE user_totp SET failed_attempts = $2, locked_until = $3 WHERE user_hk = $1`,
			userHK, failures, lock); err != nil {
			return fmt.Errorf("count the authenticator attempt: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("count the authenticator attempt: %w", err)
		}
		return ErrWrongTOTP
	}
	if _, err := tx.ExecContext(ctx, `UPDATE user_totp SET last_step = $2, failed_attempts = 0, locked_until = NULL
		WHERE user_hk = $1`, userHK, step); err != nil {
		return fmt.Errorf("record the authenticator code: %w", err)
	}
	if then != nil {
		if err := then(tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record the authenticator code: %w", err)
	}
	return nil
}

func (t *TOTP) limits() (int, int, time.Duration) {
	if t.Passwords != nil {
		return t.Passwords.limits()
	}
	return (&Passwords{}).limits()
}

// Confirm turns the authenticator on with the first code of its secret. It
// is a change of the second factor, so the security hold starts; it returns
// when the hold ends.
func (t *TOTP) Confirm(ctx context.Context, userHK, code string, event audit.Event) (time.Time, error) {
	var holdUntil time.Time
	err := t.check(ctx, userHK, code, false, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE user_totp SET confirmed_at = NOW() WHERE user_hk = $1`, userHK); err != nil {
			return fmt.Errorf("confirm the authenticator: %w", err)
		}
		if err := rowseal.Reseal(ctx, tx, rowseal.UserTOTP, "user_hk = $1", userHK); err != nil {
			return err
		}
		var err error
		if holdUntil, err = restrictions.StartSecurityHold(ctx, tx, userHK, holdDuration()); err != nil {
			return err
		}
		event.Action, event.Outcome = audit.ActionUserTOTPEnable, audit.Success
		event.TargetType, event.TargetID = audit.TargetUser, userHK
		if err := audit.Record(ctx, tx, event); err != nil {
			return fmt.Errorf("record the authenticator: %w", err)
		}
		return nil
	})
	return holdUntil, err
}

// Verify checks a code of the user's authenticator. A right code proves the
// owner, so it cancels an account recovery, as the password does.
func (t *TOTP) Verify(ctx context.Context, userHK, code string) error {
	return t.check(ctx, userHK, code, true, func(tx *sql.Tx) error {
		_, err := endRecovery(ctx, tx, userHK, RecoveryCancelled, ReasonRecoveryFactorProven, audit.Event{})
		return err
	})
}

// Remove turns the authenticator of the user off, also one that waits for
// its first code. It is a change of the second factor, so the security hold
// starts. It reports ErrNoTOTP when there was none.
func (t *TOTP) Remove(ctx context.Context, userHK string, event audit.Event) (time.Time, error) {
	tx, err := t.DB.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, fmt.Errorf("start the removal of the authenticator: %w", err)
	}
	defer tx.Rollback()
	// The row stays, marked and without its secret (#248).
	result, err := tx.ExecContext(ctx, `UPDATE user_totp SET removed_at = NOW(), secret_sealed = ''
		WHERE user_hk = $1 AND removed_at IS NULL`, userHK)
	if err != nil {
		return time.Time{}, fmt.Errorf("remove the authenticator: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return time.Time{}, ErrNoTOTP
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.UserTOTP, "user_hk = $1", userHK); err != nil {
		return time.Time{}, err
	}
	holdUntil, err := restrictions.StartSecurityHold(ctx, tx, userHK, holdDuration())
	if err != nil {
		return time.Time{}, err
	}
	event.Action, event.Outcome = audit.ActionUserTOTPDisable, audit.Success
	event.TargetType, event.TargetID = audit.TargetUser, userHK
	if err := audit.Record(ctx, tx, event); err != nil {
		return time.Time{}, fmt.Errorf("record the removal of the authenticator: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return time.Time{}, fmt.Errorf("remove the authenticator: %w", err)
	}
	return holdUntil, nil
}
