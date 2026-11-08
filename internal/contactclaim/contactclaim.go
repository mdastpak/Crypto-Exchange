// Package contactclaim takes a contact off an account that no longer owns
// it (#252).
//
// Mobile numbers in Iran are recycled: an operator gives a number that was
// not used for a while to a new subscriber, and emails can be recycled
// too. A contact belongs to one account at a time, and a code sent to it
// signs in to that account, so a stale contact would let its new holder
// into the previous owner's account and its balances. A contact therefore
// moves off an account when:
//
//   - another user proves control of it with a code and adds it, and, for
//     a mobile number of an account with a verified national ID, the
//     provider no longer matches the number to that ID (#253); or
//   - a sign-in with a mobile number finds that the provider no longer
//     matches it to the national ID of its account. The sign-in then
//     continues as for an unknown number, with a new, empty account.
//
// The account that loses the contact keeps its data. Every one of its
// sessions ends, since whoever holds the contact now must not keep a way
// in, and a security hold (#251) starts. The row of the contact is marked
// removed (#248), so the record stays.
package contactclaim

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/contact"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
)

// Reasons of a claim, as the audit event records them.
const (
	// ReasonClaimed is a contact that another user proved control of.
	ReasonClaimed = "claimed_by_another_account"
	// ReasonOwnershipMismatch is a mobile number that the provider no
	// longer matches to the national ID of its account.
	ReasonOwnershipMismatch = "ownership_mismatch"
	// ReasonContactClaimed is the audit reason of a session that a claim
	// ended.
	ReasonContactClaimed = "contact_claimed"
)

// Querier is a *sql.DB or a *sql.Tx.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Owner is the account that holds a contact now.
type Owner struct {
	UserHK string
	// OwnershipCheckedAt is the last successful check that the mobile
	// number belongs to the national ID of the account, if any.
	OwnershipCheckedAt *time.Time
}

// FindOwner returns the account that holds a contact, and false when none
// does.
func FindOwner(ctx context.Context, q Querier, contactType, value string) (Owner, bool, error) {
	var o Owner
	var checked sql.NullTime
	err := q.QueryRowContext(ctx, `SELECT c.user_hk, c.ownership_checked_at FROM user_contacts c WHERE `+db.ContactMatch,
		contactType, value).Scan(&o.UserHK, &checked)
	if errors.Is(err, sql.ErrNoRows) {
		return Owner{}, false, nil
	}
	if err != nil {
		return Owner{}, false, fmt.Errorf("failed to find the owner of a contact: %w", err)
	}
	if checked.Valid {
		o.OwnershipCheckedAt = &checked.Time
	}
	return o, true, nil
}

// Verdict is what the provider says about a mobile number of an account.
type Verdict int

const (
	// NoNationalID is an account without a verified national ID: at level
	// 0 (#157), it holds no funds, and its number cannot be checked.
	NoNationalID Verdict = iota
	// Owned is a number that belongs to the national ID of the account.
	Owned
	// NotOwned is a number that no longer does.
	NotOwned
)

// Check asks the provider whether mobile still belongs to the national ID
// of the account ownerHK. It returns the provider's reference of the check.
// An error wraps identity.ErrUnavailable when the provider cannot answer,
// or none is configured, for an account that has a national ID.
func Check(ctx context.Context, ownership identity.MobileOwnership, sealer identity.Sealer, q Querier, ownerHK, mobile string) (Verdict, string, error) {
	nationalID, has, err := identity.NationalID(ctx, q, sealer, ownerHK)
	if errors.Is(err, identity.ErrNoKeys) {
		return NoNationalID, "", fmt.Errorf("%w: %w", identity.ErrUnavailable, err)
	}
	if err != nil {
		return NoNationalID, "", err
	}
	if !has {
		return NoNationalID, "", nil
	}
	if ownership == nil {
		return NoNationalID, "", fmt.Errorf("%w: no provider is configured", identity.ErrUnavailable)
	}
	match, err := ownership.Match(ctx, nationalID, mobile)
	if err != nil {
		return NoNationalID, "", err
	}
	if match.Matched {
		return Owned, match.Ref, nil
	}
	return NotOwned, match.Ref, nil
}

// RecordCheck stores a successful check of the mobile number of an
// account, so that a sign-in does not ask again for a while.
func RecordCheck(ctx context.Context, q interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}, ownerHK, mobile, provider, ref string) error {
	_, err := q.ExecContext(ctx, `UPDATE user_contacts c SET ownership_checked_at = NOW(), ownership_provider = $3, ownership_ref = NULLIF($4, '')
		WHERE c.user_hk = $5 AND `+db.ContactMatch, string(contactMobile), mobile, provider, ref, ownerHK)
	if err != nil {
		return fmt.Errorf("failed to record the ownership check: %w", err)
	}
	return nil
}

const contactMobile = "mobile"

// LockUsers takes the exclusive lock of the rows of the users, in the
// order of their hk, so that two claims between the same accounts in
// opposite directions wait for each other instead of deadlocking.
func LockUsers(ctx context.Context, tx *sql.Tx, userHKs ...string) error {
	sorted := slices.Clone(userHKs)
	slices.Sort(sorted)
	for _, hk := range slices.Compact(sorted) {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE hk = $1 FOR UPDATE`, hk).Scan(&one); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("failed to lock the users: %w", err)
		}
	}
	return nil
}

// Detach takes the contact off the account ownerHK within tx, whose caller
// holds the lock of that account's row: it marks the row removed, ends
// every session of the account, starts a security hold of hold, and
// records it all with origin. It returns how many sessions it ended, and
// false when the account no longer holds the contact.
func Detach(ctx context.Context, tx *sql.Tx, ownerHK, contactType, value, reason string, origin audit.Event, hold time.Duration) (int, bool, error) {
	//nolint:gosec // db.ContactMatch is a constant; the values are parameters.
	res, err := tx.ExecContext(ctx, `UPDATE user_contacts c SET removed_at = NOW(), updated_at = NOW()
		WHERE c.user_hk = $3 AND `+db.ContactMatch, contactType, value, ownerHK)
	if err != nil {
		return 0, false, fmt.Errorf("failed to take the contact off its account: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return 0, false, err
	}

	rows, err := tx.QueryContext(ctx, `UPDATE sessions SET status = 'REVOKED', revoked_at = NOW()
		WHERE user_id = $1 AND status = 'ACTIVE' RETURNING id`, ownerHK)
	if err != nil {
		return 0, false, fmt.Errorf("failed to end the sessions: %w", err)
	}
	var ended []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, false, fmt.Errorf("failed to end the sessions: %w", err)
		}
		ended = append(ended, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("failed to end the sessions: %w", err)
	}

	until, err := restrictions.StartSecurityHold(ctx, tx, ownerHK, hold)
	if err != nil {
		return 0, false, err
	}
	_, masked := contact.Identify(value)
	e := audit.Of(origin, audit.ActionUserContactClaimed, audit.TargetUser, ownerHK)
	e.Reason = reason
	e.Changes = &audit.Changes{
		Before: map[string]interface{}{contactType: masked},
		After:  map[string]interface{}{"ended_sessions": len(ended), "security_hold_until": until.UTC().Format(time.RFC3339)},
	}
	if err := audit.Record(ctx, tx, e); err != nil {
		return 0, false, err
	}
	for _, id := range ended {
		se := audit.Of(origin, audit.ActionUserSessionRevoke, audit.TargetSession, id)
		se.Reason = ReasonContactClaimed
		if err := audit.Record(ctx, tx, se); err != nil {
			return 0, false, err
		}
	}
	return len(ended), true, nil
}
