package restrictions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// The security hold (#251). A change of a security setting of a user, such
// as their email address, mobile number, or second factor, holds the
// operations that a second factor protects until users.security_hold_until.
// If someone else made the change, they cannot use it at once to move funds
// out, and the owner has time to notice and undo it.
//
// The hold is a column of the user's row, which every instance reads, so it
// applies on every instance at once. StartSecurityHold writes it in the
// transaction of the change, and Check reads it in the transaction of an
// operation after Lock: the share lock of the row waits for a change in
// flight, as it waits for a new restriction.
//
// Changes of contacts are not held: the owner must be able to undo an
// attacker's change at once (#250 keeps a new contact from changing the
// older ones).

// CodeSecurityHold is the code of a refusal by the security hold.
const CodeSecurityHold = "security_hold"

// messageSecurityHold is the message of a refusal by the security hold.
var messageSecurityHold = i18n.M("SECURITY_HOLD")

// HeldOperations are the operations that the security hold blocks. Every
// path that moves funds out calls Check with one of them, so it cannot miss
// the hold. Other operations that a second factor protects, such as
// registering a withdrawal destination (#160) or turning off the second
// factor (#161), call CheckSecurityHold.
var HeldOperations = []Operation{OpWithdraw}

// StartSecurityHold holds the user's operations that a second factor
// protects for d from now, within tx, the transaction of the security
// change. A hold that ends later is kept: a change never shortens it. It
// returns when the hold ends.
func StartSecurityHold(ctx context.Context, tx *sql.Tx, userHK string, d time.Duration) (time.Time, error) {
	if d <= 0 {
		return time.Time{}, fmt.Errorf("a security hold of %s", d)
	}
	var until time.Time
	err := tx.QueryRowContext(ctx, `UPDATE users
		SET security_hold_until = GREATEST(COALESCE(security_hold_until, NOW()), NOW() + make_interval(secs => $2))
		WHERE hk = $1 RETURNING security_hold_until`, userHK, d.Seconds()).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrUserNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to start the security hold: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.Users, "hk = $1", userHK); err != nil {
		return time.Time{}, err
	}
	return until, nil
}

// SecurityHold returns when the security hold of the user ends, or nil when
// none applies now. The time is compared with the database clock.
func SecurityHold(ctx context.Context, q Querier, userHK string) (*time.Time, error) {
	var until sql.NullTime
	err := q.QueryRowContext(ctx, `SELECT security_hold_until FROM users
		WHERE hk = $1 AND security_hold_until > NOW()`, userHK).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !until.Valid) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read the security hold: %w", err)
	}
	return &until.Time, nil
}

// CheckSecurityHold returns a *Refusal with CodeSecurityHold while the
// security hold of the user applies. op names the operation in the refusal.
func CheckSecurityHold(ctx context.Context, q Querier, userHK string, op Operation) error {
	until, err := SecurityHold(ctx, q, userHK)
	if err != nil || until == nil {
		return err
	}
	return &Refusal{Code: CodeSecurityHold, Operation: op, Until: until}
}

// held reports whether the security hold blocks op.
func held(op Operation) bool { return slices.Contains(HeldOperations, op) }
