// Package identity checks a user's identity with a KYC provider. For now it
// answers one question (#253): does an Iranian mobile number belong to a
// national ID? The Shahkar service of the Communications Regulatory
// Authority holds the answer, and KYC providers such as Jibit offer it.
//
// A code sent to a number only shows that someone controls the line. A
// match with the national ID shows that the line is registered to the
// owner of the account, so a number that was recycled to another
// subscriber, or a SIM card in someone else's name, does not pass.
//
// National IDs and mobile numbers are personal data. They are never logged
// and never put in errors.
package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MobileOwnership checks that a mobile number belongs to a national ID.
// Implementations are safe for concurrent use.
type MobileOwnership interface {
	// Provider names the provider, as the contacts record it.
	Provider() string
	// Match reports whether mobile, in the national form 09xxxxxxxxx, is
	// registered to nationalID. An error means that the provider could not
	// answer; it wraps ErrUnavailable when trying again later may work.
	Match(ctx context.Context, nationalID, mobile string) (Match, error)
}

// Match is the answer of a check.
type Match struct {
	// Matched is whether the number belongs to the national ID.
	Matched bool
	// Ref traces the check at the provider, for disputes.
	Ref string
}

// ErrUnavailable reports a provider that cannot answer now: a timeout, a
// network error, a server error, a rate limit, or an answer that cannot be
// read. Nothing is decided on it.
var ErrUnavailable = errors.New("the identity provider is unavailable")

// ErrNoKeys reports a national ID that cannot be read because the
// data-protection keys are not loaded.
var ErrNoKeys = errors.New("the data-protection keys are not loaded")

// Sealer encrypts and decrypts fields with the data-protection keys
// (#138). *keys.Live implements it.
type Sealer interface {
	SealField(aad, plaintext []byte) ([]byte, error)
	OpenField(aad, sealed []byte) ([]byte, error)
}

// Querier is a *sql.DB or a *sql.Tx.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// nationalIDAAD binds a sealed national ID to its user, so that a sealed
// value copied to another user's row does not decrypt.
func nationalIDAAD(userHK string) []byte {
	return []byte("user_national_ids.national_id_sealed:" + userHK)
}

// NationalID returns the verified national ID of a user, and false when
// the user has none. Without sealer, a user with a national ID answers
// ErrNoKeys: the check cannot be skipped because the keys are missing.
func NationalID(ctx context.Context, q Querier, sealer Sealer, userHK string) (string, bool, error) {
	var sealed []byte
	err := q.QueryRowContext(ctx, `SELECT national_id_sealed FROM user_national_ids
		WHERE user_hk = $1 AND verified_at IS NOT NULL`, userHK).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("failed to read the national ID: %w", err)
	}
	if sealer == nil {
		return "", false, ErrNoKeys
	}
	plain, err := sealer.OpenField(nationalIDAAD(userHK), sealed)
	if err != nil {
		return "", false, fmt.Errorf("failed to open the national ID: %w", err)
	}
	// A national ID is checked when it is saved, and again before any
	// check relies on it, so that a malformed one never reaches a
	// provider.
	if !ValidNationalID(string(plain)) {
		return "", false, ErrInvalidNationalID
	}
	return string(plain), true, nil
}

// ErrInvalidNationalID is a stored national ID that is not well formed.
// Nothing stores one, so it marks a value written outside the backend.
var ErrInvalidNationalID = errors.New("the stored national ID is not a valid national ID")

// Execer is a *sql.DB or a *sql.Tx.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// SaveNationalID stores the verified national ID of a user, sealed,
// replacing the one before. The identity verification of #158 calls it,
// with the provider and its reference of the check.
func SaveNationalID(ctx context.Context, q Execer, sealer Sealer, userHK, nationalID, provider, ref string, verifiedAt time.Time) error {
	if !ValidNationalID(nationalID) {
		return errors.New("not a valid national ID")
	}
	if sealer == nil {
		return ErrNoKeys
	}
	sealed, err := sealer.SealField(nationalIDAAD(userHK), []byte(nationalID))
	if err != nil {
		return fmt.Errorf("failed to seal the national ID: %w", err)
	}
	_, err = q.ExecContext(ctx, `INSERT INTO user_national_ids (user_hk, national_id_sealed, verified_at, provider, provider_ref)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''))
		ON CONFLICT (user_hk) DO UPDATE SET national_id_sealed = EXCLUDED.national_id_sealed,
			verified_at = EXCLUDED.verified_at, provider = EXCLUDED.provider, provider_ref = EXCLUDED.provider_ref,
			updated_at = NOW()`, userHK, sealed, verifiedAt, provider, ref)
	if err != nil {
		return fmt.Errorf("failed to store the national ID: %w", err)
	}
	return nil
}

// ValidNationalID reports whether s is a well-formed Iranian national ID:
// ten digits whose last one is the check digit of the others, and not one
// digit repeated.
func ValidNationalID(s string) bool {
	if len(s) != 10 {
		return false
	}
	same := true
	sum := 0
	for i := 0; i < 10; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
		if s[i] != s[0] {
			same = false
		}
		if i < 9 {
			sum += int(s[i]-'0') * (10 - i)
		}
	}
	if same {
		return false
	}
	check := int(s[9] - '0')
	r := sum % 11
	if r < 2 {
		return check == r
	}
	return check == 11-r
}
