package verification

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// IdentityLevelReason is the reason of the levels that the identity checks
// grant (#158), in the audit log.
const IdentityLevelReason = "identity checks: national ID and the owner of a card, IBAN, or account (#158)"

// GrantIdentityLevel raises the user from level 0 to level 1 when the
// identity checks of #158 allow it: a verified national ID, and at least
// one card, IBAN, or account in their withdrawal destinations whose owner
// the provider matched with it, not removed and not pending. It never
// lowers a level and never touches level 2, which needs more (#162).
//
// Run it in tx: it locks the user's row first, as every change of a user
// does, so call it before the transaction locks anything of the user's
// that comes later in that order. The national ID and the destination are
// checked against their seals (#139): one that fails grants nothing, and
// its error wraps rowseal.ErrUnverified. It returns the change, which
// Changed reports, and nothing when the level stays.
func GrantIdentityLevel(ctx context.Context, tx *sql.Tx, userHK string, origin audit.Event) (Change, error) {
	var tier, status string
	err := tx.QueryRowContext(ctx, `SELECT kyc_tier, kyc_status FROM users WHERE hk = $1 AND deleted_at IS NULL FOR UPDATE`,
		userHK).Scan(&tier, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return Change{}, ErrUserNotFound
	}
	if err != nil {
		return Change{}, fmt.Errorf("lock the user: %w", err)
	}
	if LevelOf(shared.KYCTier(tier), shared.KYCStatus(status)) != Level0 {
		return Change{}, nil
	}
	ready, err := identityVerified(ctx, tx, userHK)
	if err != nil || !ready {
		return Change{}, err
	}
	return SetLevel(ctx, tx, userHK, Level1, IdentityLevelReason, origin)
}

// identityVerified reports whether the user has a verified national ID and
// a matched card, IBAN, or account, each matching its seal.
func identityVerified(ctx context.Context, tx *sql.Tx, userHK string) (bool, error) {
	national := rowseal.UserNationalIDs.NewRow()
	err := tx.QueryRowContext(ctx, `SELECT `+rowseal.UserNationalIDs.Fields("")+`
		FROM user_national_ids WHERE user_hk = $1 AND verified_at IS NOT NULL`, userHK).Scan(national.Dest()...)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the national ID: %w", err)
	}
	if err := rowseal.Check(ctx, rowseal.UserNationalIDs, national); err != nil {
		return false, err
	}
	//nolint:gosec // G202: the fields are the sealed columns of the table; the values are arguments.
	rows, err := tx.QueryContext(ctx, `SELECT `+rowseal.WithdrawalDestinations.Fields("")+`
		FROM withdrawal_destinations
		WHERE user_hk = $1 AND kind IN ('card', 'iban', 'account') AND removed_at IS NULL
		  AND verified_by IS NOT NULL AND ownership_pending_since IS NULL
		ORDER BY created_at LIMIT 10`, userHK)
	if err != nil {
		return false, fmt.Errorf("read the destinations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		r := rowseal.WithdrawalDestinations.NewRow()
		if err := rows.Scan(r.Dest()...); err != nil {
			return false, fmt.Errorf("read the destinations: %w", err)
		}
		// One that fails its seal does not count; another one may.
		if rowseal.Check(ctx, rowseal.WithdrawalDestinations, r) == nil {
			return true, nil
		}
	}
	return false, rows.Err()
}
