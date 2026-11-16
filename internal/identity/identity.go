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

	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/jackc/pgx/v5/pgconn"
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

// birthDateAAD binds a sealed date of birth to its user.
func birthDateAAD(userHK string) []byte {
	return []byte("user_national_ids.birth_date_sealed:" + userHK)
}

// NationalIDIndexDomain is the domain of the blind index of a national ID
// (#140, #158), so that the index of one is never that of another field.
const NationalIDIndexDomain = "user_national_ids.national_id"

// ErrNationalIDTaken is a national ID verified for another account (#158):
// one person has one account.
var ErrNationalIDTaken = errors.New("the national ID is verified for another account")

// NationalID returns the verified national ID of a user, and false when
// the user has none. Without sealer, a user with a national ID answers
// ErrNoKeys: the check cannot be skipped because the keys are missing. A
// row that does not match its seal (#139) is refused in enforce mode: the
// error wraps rowseal.ErrUnverified.
func NationalID(ctx context.Context, q Querier, sealer Sealer, userHK string) (string, bool, error) {
	p, has, err := PersonOf(ctx, q, sealer, userHK)
	return p.NationalID, has, err
}

// PersonOf returns the verified national ID of a user with its date of
// birth, which is zero for a national ID stored without one, and false
// when the user has none. It checks as NationalID does.
func PersonOf(ctx context.Context, q Querier, sealer Sealer, userHK string) (Person, bool, error) {
	var sealed, birth []byte
	row := rowseal.UserNationalIDs.NewRow()
	err := q.QueryRowContext(ctx, `SELECT national_id_sealed, birth_date_sealed, `+rowseal.UserNationalIDs.Fields("")+`
		FROM user_national_ids WHERE user_hk = $1 AND verified_at IS NOT NULL`, userHK).
		Scan(append([]any{&sealed, &birth}, row.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Person{}, false, nil
	}
	if err != nil {
		return Person{}, false, fmt.Errorf("failed to read the national ID: %w", err)
	}
	// A national ID swapped in SQL is never relied on (#158).
	if err := rowseal.Check(ctx, rowseal.UserNationalIDs, row); err != nil {
		return Person{}, false, err
	}
	if sealer == nil {
		return Person{}, false, ErrNoKeys
	}
	plain, err := sealer.OpenField(nationalIDAAD(userHK), sealed)
	if err != nil {
		return Person{}, false, fmt.Errorf("failed to open the national ID: %w", err)
	}
	// A national ID is checked when it is saved, and again before any
	// check relies on it, so that a malformed one never reaches a
	// provider.
	if !ValidNationalID(string(plain)) {
		return Person{}, false, ErrInvalidNationalID
	}
	p := Person{NationalID: string(plain)}
	if birth != nil {
		date, err := sealer.OpenField(birthDateAAD(userHK), birth)
		if err != nil {
			return Person{}, false, fmt.Errorf("failed to open the date of birth: %w", err)
		}
		if p.BirthDate, err = time.Parse(time.DateOnly, string(date)); err != nil {
			return Person{}, false, errors.New("the stored date of birth is not a date")
		}
	}
	return p, true, nil
}

// ErrInvalidNationalID is a stored national ID that is not well formed.
// Nothing stores one, so it marks a value written outside the backend.
var ErrInvalidNationalID = errors.New("the stored national ID is not a valid national ID")

// Verified is a national ID as a provider verified it, to be stored.
type Verified struct {
	Person
	Provider, Ref string
	At            time.Time
}

// SaveNationalID stores the verified national ID of a user, sealed,
// replacing the one before, without a date of birth, as the mobile
// ownership checks and tests store one; the identity checks use Save.
func SaveNationalID(ctx context.Context, q rowseal.Querier, sealer Sealer, userHK, nationalID, provider, ref string, verifiedAt time.Time) error {
	return Save(ctx, q, sealer, userHK, Verified{Person: Person{NationalID: nationalID}, Provider: provider, Ref: ref, At: verifiedAt})
}

// Save stores the verified national ID of a user with its date of birth,
// encrypted, and its blind index, replacing the one before, and seals the
// row (#139). A national ID that another account has verified, under any
// version of the blind-index key, is refused with ErrNationalIDTaken (#158).
// Run it in the caller's transaction, after the lock of the user's row.
// Without the field cipher, which holds the blind-index key, no index is
// stored: only tests run so.
func Save(ctx context.Context, q rowseal.Querier, sealer Sealer, userHK string, v Verified) error {
	if !ValidNationalID(v.NationalID) {
		return errors.New("not a valid national ID")
	}
	if sealer == nil {
		return ErrNoKeys
	}
	sealed, err := sealer.SealField(nationalIDAAD(userHK), []byte(v.NationalID))
	if err != nil {
		return fmt.Errorf("failed to seal the national ID: %w", err)
	}
	var birth []byte
	if !v.BirthDate.IsZero() {
		if birth, err = sealer.SealField(birthDateAAD(userHK), []byte(v.BirthDate.Format(time.DateOnly))); err != nil {
			return fmt.Errorf("failed to seal the date of birth: %w", err)
		}
	}
	var index []byte
	if cipher := field.Active(); cipher != nil {
		if index, err = cipher.Index(NationalIDIndexDomain, v.NationalID); err != nil {
			return err
		}
		var taken bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM user_national_ids
			WHERE national_id_bidx = ANY($1::bytea[]) AND user_hk <> $2)`,
			cipher.Indexes(NationalIDIndexDomain, v.NationalID), userHK).Scan(&taken); err != nil {
			return fmt.Errorf("failed to look for the national ID: %w", err)
		}
		if taken {
			return ErrNationalIDTaken
		}
	}
	_, err = q.ExecContext(ctx, `INSERT INTO user_national_ids (user_hk, national_id_sealed, verified_at, provider, provider_ref,
			national_id_bidx, birth_date_sealed)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7)
		ON CONFLICT (user_hk) DO UPDATE SET national_id_sealed = EXCLUDED.national_id_sealed,
			verified_at = EXCLUDED.verified_at, provider = EXCLUDED.provider, provider_ref = EXCLUDED.provider_ref,
			national_id_bidx = EXCLUDED.national_id_bidx, birth_date_sealed = EXCLUDED.birth_date_sealed,
			updated_at = NOW()`, userHK, sealed, v.At, v.Provider, v.Ref, index, birth)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "user_national_ids_bidx" {
		return ErrNationalIDTaken
	}
	if err != nil {
		return fmt.Errorf("failed to store the national ID: %w", err)
	}
	return rowseal.Reseal(ctx, q, rowseal.UserNationalIDs, "user_hk = $1", userHK)
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
