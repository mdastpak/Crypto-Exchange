package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// ContactMatchAt is the SQL condition that finds the row c of user_contacts
// of a contact, with the three arguments of ContactArgs from $n: the type,
// the blind indexes, and the value. It finds the row by its blind index
// (#140), or by its plaintext, which the rows hold until the release after
// #140's first one, the way the index user_contacts_value_unique compares
// them: emails and usernames case-insensitively, with ASCII-only case
// folding that does not depend on the database locale. Both lookups use a
// unique index. A contact that its user removed (removed_at, #248) matches
// nothing.
func ContactMatchAt(n int) string {
	return fmt.Sprintf(`c.type = $%d::contact_type AND (c.value_bidx = ANY($%d::bytea[]) OR lower(c.value COLLATE "C") = lower($%d COLLATE "C")) AND c.removed_at IS NULL`,
		n, n+1, n+2)
}

// ContactMatch is ContactMatchAt(1).
var ContactMatch = ContactMatchAt(1)

// ContactArgs returns the arguments of ContactMatchAt for a contact: its
// type, its blind indexes with every loaded version of the blind-index key
// (none without data keys), and its value.
func ContactArgs(contactType, value string) []any {
	return []any{contactType, field.Active().Indexes(contactDomain(contactType), FoldContact(value)), value}
}

// contactDomain is the domain of the blind indexes of the contacts of one
// type.
func contactDomain(contactType string) string { return "user_contacts.value:" + contactType }

// FoldContact returns the form of a contact that its blind index is
// computed from: its ASCII letters in lower case, as lower(value COLLATE
// "C") folds them, so that the blind index and the plaintext index of
// user_contacts tell the same contacts apart.
func FoldContact(value string) string {
	b := []byte(value)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// ContactPlace is where the value of the contact id is sealed: its
// associated data.
func ContactPlace(id int64) field.Place {
	return field.Place{Table: "user_contacts", Column: "value_enc", Row: strconv.FormatInt(id, 10)}
}

// Execer is implemented by *sql.DB and *sql.Tx.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// EncryptContact writes the ciphertext and the blind index of the contact
// id, of contactType and value, with the active cipher, and empties its
// plaintext, in the transaction of the write of the plaintext (#140). The
// plaintext is written first so that user_contacts_value_unique still
// compares it with the rows that hold only plaintext, and the blind index
// then with the rows that hold only ciphertext. Without data keys, which
// only development allows, it writes nothing: the row keeps its plaintext
// only, until the rekey job encrypts it. The caller seals the row after.
func EncryptContact(ctx context.Context, q Execer, id int64, contactType, value string) error {
	if field.Active() == nil {
		return nil
	}
	sealed, idx, err := sealContact(id, contactType, value)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `UPDATE user_contacts SET value = NULL, value_enc = $2, value_bidx = $3 WHERE id = $1`, id, sealed, idx)
	return err
}

// sealContact returns the ciphertext and the blind index of the contact id,
// of contactType and value, with the active cipher.
func sealContact(id int64, contactType, value string) (sealed, idx []byte, err error) {
	c := field.Active()
	if sealed, err = c.Seal(ContactPlace(id), value); err != nil {
		return nil, nil, err
	}
	if idx, err = c.Index(contactDomain(contactType), FoldContact(value)); err != nil {
		return nil, nil, err
	}
	return sealed, idx, nil
}

// OpenContact returns the value of the contact id from its plaintext and
// its ciphertext, as they were read; see field.Cipher.Read.
func OpenContact(id int64, plain *string, sealed []byte) (string, error) {
	v, err := field.Active().Read(ContactPlace(id), plain, sealed)
	if err != nil {
		return "", err
	}
	if v == nil {
		return "", fmt.Errorf("user_contacts %d holds no value", id)
	}
	return *v, nil
}

// InsertContact adds a primary contact to a user within tx. verified says
// that the user has just proven control of it, as a correct sign-in code
// does. A contact that belongs to another user fails with a unique
// violation (SQLSTATE 23505).
func InsertContact(ctx context.Context, tx *sql.Tx, userHK, contactType, value string, verified bool) error {
	var id int64
	err := tx.QueryRowContext(ctx,
		`INSERT INTO user_contacts (user_hk, type, value, verified_at)
		 VALUES ($1, $2::contact_type, $3, CASE WHEN $4::boolean THEN NOW() END) RETURNING id`,
		userHK, contactType, value, verified,
	).Scan(&id)
	if err != nil {
		return err
	}
	if err := EncryptContact(ctx, tx, id, contactType, value); err != nil {
		return err
	}
	// The new row is the user's contact that this transaction wrote (#139).
	return rowseal.Reseal(ctx, tx, rowseal.UserContacts, "user_hk = $1 AND updated_at = NOW()", userHK)
}

// UserPlace is where a personal field of the user userHK is sealed, such
// as column first_name_enc for the first name: its associated data.
func UserPlace(userHK, column string) field.Place {
	return field.Place{Table: "users", Column: column, Row: userHK}
}

// Names are the names of a user, nil where the user set none.
type Names struct{ First, Last, Display *string }

// SealedNames are the ciphertexts of Names, nil where a name is nil, or
// without data keys.
type SealedNames struct{ First, Last, Display []byte }

// SealNames seals the names of the user userHK with the active cipher.
func SealNames(userHK string, n Names) (SealedNames, error) {
	c := field.Active()
	var s SealedNames
	var err error
	if s.First, err = c.SealOptional(UserPlace(userHK, "first_name_enc"), n.First); err != nil {
		return SealedNames{}, err
	}
	if s.Last, err = c.SealOptional(UserPlace(userHK, "last_name_enc"), n.Last); err != nil {
		return SealedNames{}, err
	}
	if s.Display, err = c.SealOptional(UserPlace(userHK, "display_name_enc"), n.Display); err != nil {
		return SealedNames{}, err
	}
	return s, nil
}

// OpenNames returns the names of the user userHK from their plaintext and
// their ciphertext, as they were read; see field.Cipher.Read.
func OpenNames(userHK string, plain Names, sealed SealedNames) (Names, error) {
	c := field.Active()
	var n Names
	var err error
	if n.First, err = c.Read(UserPlace(userHK, "first_name_enc"), plain.First, sealed.First); err != nil {
		return Names{}, err
	}
	if n.Last, err = c.Read(UserPlace(userHK, "last_name_enc"), plain.Last, sealed.Last); err != nil {
		return Names{}, err
	}
	if n.Display, err = c.Read(UserPlace(userHK, "display_name_enc"), plain.Display, sealed.Display); err != nil {
		return Names{}, err
	}
	return n, nil
}

// StoredNames returns what a write of the names n of the user userHK
// stores: their ciphertext, and no plaintext, with the active cipher (#140),
// or their plaintext without data keys, which only development allows.
func StoredNames(userHK string, n Names) (Names, SealedNames, error) {
	if field.Active() == nil {
		return n, SealedNames{}, nil
	}
	sealed, err := SealNames(userHK, n)
	return Names{}, sealed, err
}
