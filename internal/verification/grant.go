package verification

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/google/uuid"
)

// Granting a level (#440). The identity checks (#158, #162) and the KYC
// review (#72, #71) will grant the levels of users. Until they exist, an
// operator sets the level of a user with cmd/admin verification grant: for
// the testnet run of M2, on the same level policy as production, and for
// manual review in the first months. The checks call SetLevel too.

// MaxReason is the longest reason of a change of level, in characters.
const MaxReason = 500

// Errors of SetLevel.
var (
	ErrInvalidLevel   = fmt.Errorf("the level must be from %d to %d", Level0, Level2)
	ErrReasonRequired = fmt.Errorf("a reason of at most %d characters is required", MaxReason)
	// ErrNotAUser is an administrator's account, which has no level.
	ErrNotAUser = errors.New("the account is an administrator, not a user")
	// ErrInactive is an account that is not active, such as a suspended
	// or banned one.
	ErrInactive = errors.New("the account is not active")
)

// TierOf returns the KYC tier and status that make level: level 0 is no
// tier, pending, as a new account has; level 1 is BASIC and level 2
// STANDARD, each approved. LevelOf gives the level back.
func TierOf(level Level) (shared.KYCTier, shared.KYCStatus, error) {
	switch level {
	case Level0:
		return shared.KYCTierNone, shared.KYCStatusPending, nil
	case Level1:
		return shared.KYCTierBasic, shared.KYCStatusApproved, nil
	case Level2:
		return shared.KYCTierStandard, shared.KYCStatusApproved, nil
	}
	return "", "", ErrInvalidLevel
}

// Change is a change of the level of a user.
type Change struct {
	UserHK string
	// Before and After are the verification of the user before the change
	// and after it.
	Before, After Status
}

// Changed reports whether the change wrote anything: a user who has the
// tier and status of the level already is left as they are.
func (c Change) Changed() bool {
	return c.Before.Tier != c.After.Tier || c.Before.Status != c.After.Status
}

// SetLevel sets the verification level of the user to level, in tx, and
// records it in the audit log with origin, the operator or administrator
// who set it, and reason.
//
// It locks the user's row first, as every change of a user does (the user,
// then confirmations, withdrawals, balances, and sessions). An operation in
// flight holds a share lock of the row (restrictions.Lock) and commits
// first; the next one reads the new level, on every instance, since the
// level is read from the row each time.
//
// A level can go down as well as up, as when a check is revoked. Lowering
// it stops nothing that was requested already: it applies from the next
// request.
//
// A row that does not match its seal is not changed: the error wraps
// rowseal.ErrUnverified, and its finding stands. ErrUserNotFound,
// ErrNotAUser, and ErrInactive refuse an account that does not exist or
// was deleted, an administrator, and an account that is not active.
func SetLevel(ctx context.Context, tx *sql.Tx, userHK string, level Level, reason string, origin audit.Event) (Change, error) {
	tier, status, err := TierOf(level)
	if err != nil {
		return Change{}, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > MaxReason {
		return Change{}, ErrReasonRequired
	}
	if _, err := uuid.Parse(userHK); err != nil {
		return Change{}, ErrUserNotFound
	}

	var before Status
	var role, account string
	row := rowseal.Users.NewRow()
	err = tx.QueryRowContext(ctx, `SELECT kyc_tier, kyc_status, role, status, `+rowseal.Users.Fields("")+`
		FROM users WHERE hk = $1 AND deleted_at IS NULL FOR UPDATE`, userHK).
		Scan(append([]any{&before.Tier, &before.Status, &role, &account}, row.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Change{}, ErrUserNotFound
	}
	if err != nil {
		return Change{}, fmt.Errorf("read the user: %w", err)
	}
	// A tier or status changed in SQL is not built on.
	if err := rowseal.Check(ctx, rowseal.Users, row); err != nil {
		return Change{}, err
	}
	switch {
	case role != "user":
		return Change{}, ErrNotAUser
	case account != "ACTIVE":
		return Change{}, fmt.Errorf("%w: %s", ErrInactive, account)
	}
	before.Level = LevelOf(before.Tier, before.Status)
	change := Change{UserHK: userHK, Before: before, After: Status{Level: level, Tier: tier, Status: status}}
	if !change.Changed() {
		return change, nil
	}

	if _, err := tx.ExecContext(ctx, `UPDATE users SET kyc_tier = $2, kyc_status = $3, updated_at = NOW() WHERE hk = $1`,
		userHK, string(tier), string(status)); err != nil {
		return Change{}, fmt.Errorf("set the level: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.Users, "hk = $1", userHK); err != nil {
		return Change{}, err
	}
	e := audit.Of(origin, audit.ActionUserVerificationSet, audit.TargetUser, userHK)
	e.Changes = &audit.Changes{
		Before: map[string]any{"level": int(before.Level), "kyc_tier": string(before.Tier), "kyc_status": string(before.Status)},
		After:  map[string]any{"level": int(level), "kyc_tier": string(tier), "kyc_status": string(status), "reason": reason},
	}
	if err := audit.Record(ctx, tx, e); err != nil {
		return Change{}, err
	}
	return change, nil
}
