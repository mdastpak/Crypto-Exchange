package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// fieldsBatchSize is how many rows one transaction of RekeyFields and
// DecryptFields reads.
const fieldsBatchSize = 200

// FieldsReport counts what a run of RekeyFields or DecryptFields did.
type FieldsReport struct {
	// Contacts and Users are the rows that it wrote.
	Contacts, Users int
	// Skipped are the rows that it left as they are: their seal does not
	// match in enforce mode, or their ciphertext does not open. The seal
	// verifier and the integrity findings report them.
	Skipped int
}

// fieldsPass is what a run of RekeyFields or DecryptFields selects and
// writes: the SQL conditions of the rows of user_contacts c and of users u
// that it writes, given the current versions of the field-enc and the
// blind-index keys.
type fieldsPass struct {
	contacts, users func(sealing, indexing int) string
	// plaintext writes the plaintext too.
	plaintext bool
}

// stale is the SQL condition of a column of sealed values, or of blind
// indexes, that holds a value of another version than version. The
// version is a number of the keyring, never input.
func stale(column string, version int) string {
	prefix := string(field.Prefix(version))
	return fmt.Sprintf("substring(%s FROM 1 FOR %d) <> '%s'::bytea", column, len(prefix), prefix)
}

// nameColumns are the columns of the names of users, which are encrypted.
var nameColumns = []string{"first_name", "last_name", "display_name"}

// anyName joins with OR the condition that cond makes of each name column.
func anyName(cond func(column string) string) string {
	joined := ""
	for _, column := range nameColumns {
		if joined != "" {
			joined += " OR "
		}
		joined += cond(column)
	}
	return joined
}

// The two passes.
var (
	rekeyPass = fieldsPass{
		contacts: func(sealing, indexing int) string {
			// A row without ciphertext has plaintext, by a check.
			return `c.value IS NOT NULL OR ` + stale("c.value_enc", sealing) + ` OR ` + stale("c.value_bidx", indexing)
		},
		users: func(sealing, _ int) string {
			return anyName(func(column string) string {
				return fmt.Sprintf("u.%[1]s IS NOT NULL OR %[2]s", column, stale("u."+column+"_enc", sealing))
			})
		},
	}
	decryptPass = fieldsPass{
		contacts: func(int, int) string { return `c.value IS NULL` },
		users: func(int, int) string {
			return anyName(func(column string) string {
				return fmt.Sprintf("(u.%[1]s IS NULL AND u.%[1]s_enc IS NOT NULL)", column)
			})
		},
		plaintext: true,
	}
)

// RekeyFields encrypts the personal fields that are still stored in
// plaintext (#140), and empties the plaintext, which the first release of
// #140 kept next to the ciphertext. It also encrypts again the fields sealed
// with an older field-enc key, and indexes again the contacts indexed with
// an older blind-index key, so that the older versions can be retired. It
// reads the rows in batches, each in a transaction of its own, checks their
// seals before it writes, and seals them again after.
//
// Rows that a run on another instance holds are skipped; that run handles
// them. Without data keys it returns field.ErrNoCipher.
func RekeyFields(ctx context.Context, conn *sql.DB) (FieldsReport, error) {
	return runFields(ctx, conn, rekeyPass)
}

// DecryptFields writes the plaintext of the personal fields that are
// stored only encrypted, as a later release of #140 stores them, and
// encrypts them again with the current keys. It is the step before the
// down migration of 16_encrypt_personal_fields, which refuses to drop the
// ciphertext while a value has no plaintext. Stop the servers first: their
// job empties the plaintext again. Without data keys it returns
// field.ErrNoCipher.
func DecryptFields(ctx context.Context, conn *sql.DB) (FieldsReport, error) {
	return runFields(ctx, conn, decryptPass)
}

func runFields(ctx context.Context, conn *sql.DB, pass fieldsPass) (FieldsReport, error) {
	sealing, indexing, err := field.Active().CurrentVersions()
	if err != nil {
		return FieldsReport{}, err
	}
	contactsWhere, usersWhere := pass.contacts(sealing, indexing), pass.users(sealing, indexing)
	var report FieldsReport
	for after := int64(0); ; {
		last, done, err := contactsBatch(ctx, conn, contactsWhere, pass.plaintext, after, &report)
		if err != nil {
			return report, fmt.Errorf("the contacts: %w", err)
		}
		if done {
			break
		}
		after = last
	}
	for after := ""; ; {
		last, done, err := usersBatch(ctx, conn, usersWhere, pass.plaintext, after, &report)
		if err != nil {
			return report, fmt.Errorf("the names: %w", err)
		}
		if done {
			break
		}
		after = last
	}
	return report, nil
}

// storedContact is a row of user_contacts that a pass writes.
type storedContact struct {
	row    *rowseal.Row
	id     int64
	typ    string
	plain  *string
	sealed []byte
}

// contactsBatch writes one batch of the contacts after the id after, and
// returns the last id it read and whether there are no more.
func contactsBatch(ctx context.Context, conn *sql.DB, where string, plaintext bool, after int64, report *FieldsReport) (int64, bool, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	//nolint:gosec // G202: the conditions are built by this package from key versions, never from input; the values are arguments.
	rows, err := tx.QueryContext(ctx, `SELECT `+rowseal.UserContacts.Fields("c")+`, c.id, c.type::text, c.value, c.value_enc
		FROM user_contacts c
		WHERE c.id > $1 AND (`+where+`)
		ORDER BY c.id LIMIT $2 FOR UPDATE SKIP LOCKED`, after, fieldsBatchSize)
	if err != nil {
		return 0, false, err
	}
	var batch []storedContact
	for rows.Next() {
		r := storedContact{row: rowseal.UserContacts.NewRow()}
		if err := rows.Scan(append(r.row.Dest(), &r.id, &r.typ, &r.plain, &r.sealed)...); err != nil {
			rows.Close()
			return 0, false, err
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	for _, r := range batch {
		if !checked(ctx, rowseal.UserContacts, r.row, report) {
			continue
		}
		value, err := OpenContact(r.id, r.plain, r.sealed)
		if err != nil {
			log.Printf("SECURITY: user_contacts %d is left as it is: %v", r.id, err)
			report.Skipped++
			continue
		}
		if plaintext {
			// The plaintext, with a new ciphertext, so that the trigger of
			// a plaintext written alone keeps them both.
			sealed, idx, err := sealContact(r.id, r.typ, value)
			if err != nil {
				return 0, false, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE user_contacts SET value = $2, value_enc = $3, value_bidx = $4 WHERE id = $1`,
				r.id, value, sealed, idx); err != nil {
				return 0, false, err
			}
		} else if err := EncryptContact(ctx, tx, r.id, r.typ, value); err != nil {
			return 0, false, err
		}
		if err := rowseal.Reseal(ctx, tx, rowseal.UserContacts, "id = $1", r.id); err != nil {
			return 0, false, err
		}
		report.Contacts++
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	if len(batch) < fieldsBatchSize {
		return 0, true, nil
	}
	return batch[len(batch)-1].id, false, nil
}

// storedNames is a row of users that a pass writes.
type storedNames struct {
	row    *rowseal.Row
	hk     string
	plain  Names
	sealed SealedNames
}

// usersBatch writes the names of one batch of users after the hk after,
// and returns the last hk it read and whether there are no more.
func usersBatch(ctx context.Context, conn *sql.DB, where string, plaintext bool, after string, report *FieldsReport) (string, bool, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	//nolint:gosec // G202: the conditions are built by this package from key versions, never from input; the values are arguments.
	rows, err := tx.QueryContext(ctx, `SELECT `+rowseal.Users.Fields("u")+`, u.hk::text,
			u.first_name, u.last_name, u.display_name, u.first_name_enc, u.last_name_enc, u.display_name_enc
		FROM users u
		WHERE ($1 = '' OR u.hk > $1::uuid) AND (`+where+`)
		ORDER BY u.hk LIMIT $2 FOR UPDATE SKIP LOCKED`, after, fieldsBatchSize)
	if err != nil {
		return "", false, err
	}
	var batch []storedNames
	for rows.Next() {
		r := storedNames{row: rowseal.Users.NewRow()}
		dest := append(r.row.Dest(), &r.hk, &r.plain.First, &r.plain.Last, &r.plain.Display,
			&r.sealed.First, &r.sealed.Last, &r.sealed.Display)
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return "", false, err
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	for _, r := range batch {
		if !checked(ctx, rowseal.Users, r.row, report) {
			continue
		}
		names, err := OpenNames(r.hk, r.plain, r.sealed)
		if err != nil {
			log.Printf("SECURITY: the names of users %s are left as they are: %v", r.hk, err)
			report.Skipped++
			continue
		}
		sealed, err := SealNames(r.hk, names)
		if err != nil {
			return "", false, err
		}
		if plaintext {
			_, err = tx.ExecContext(ctx, `UPDATE users SET first_name = $2, last_name = $3, display_name = $4,
				first_name_enc = $5, last_name_enc = $6, display_name_enc = $7 WHERE hk = $1`,
				r.hk, names.First, names.Last, names.Display, sealed.First, sealed.Last, sealed.Display)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE users SET first_name = NULL, last_name = NULL, display_name = NULL,
				first_name_enc = $2, last_name_enc = $3, display_name_enc = $4 WHERE hk = $1`,
				r.hk, sealed.First, sealed.Last, sealed.Display)
		}
		if err != nil {
			return "", false, err
		}
		if err := rowseal.Reseal(ctx, tx, rowseal.Users, "hk = $1", r.hk); err != nil {
			return "", false, err
		}
		report.Users++
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	if len(batch) < fieldsBatchSize {
		return "", true, nil
	}
	return batch[len(batch)-1].hk, false, nil
}

// checked checks the seal of a row before a pass writes it: sealing it
// again would hide a change made outside the backend. In enforce mode a row
// that does not match is skipped; report mode records the finding and
// writes it, as the verifier does.
func checked(ctx context.Context, t *rowseal.Table, r *rowseal.Row, report *FieldsReport) bool {
	err := rowseal.Check(ctx, t, r)
	if errors.Is(err, rowseal.ErrUnverified) {
		report.Skipped++
		return false
	}
	return err == nil
}

// FieldsStatus counts, for `admin fields status`, the rows whose personal
// fields RekeyFields would still write, and the rows whose plaintext is
// gone.
type FieldsStatus struct {
	// ContactsToRekey and UsersToRekey still hold plaintext, or a value of
	// an older key version.
	ContactsToRekey, UsersToRekey int64
	// ContactsEncryptedOnly and UsersEncryptedOnly have no plaintext.
	ContactsEncryptedOnly, UsersEncryptedOnly int64
}

// ReadFieldsStatus counts the rows of FieldsStatus, with the current key
// versions of the active cipher.
func ReadFieldsStatus(ctx context.Context, conn *sql.DB) (FieldsStatus, error) {
	sealing, indexing, err := field.Active().CurrentVersions()
	if err != nil {
		return FieldsStatus{}, err
	}
	var s FieldsStatus
	err = conn.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM user_contacts c WHERE `+rekeyPass.contacts(sealing, indexing)+`),
			(SELECT COUNT(*) FROM users u WHERE `+rekeyPass.users(sealing, indexing)+`),
			(SELECT COUNT(*) FROM user_contacts c WHERE `+decryptPass.contacts(sealing, indexing)+`),
			(SELECT COUNT(*) FROM users u WHERE `+decryptPass.users(sealing, indexing)+`)`,
	).Scan(&s.ContactsToRekey, &s.UsersToRekey, &s.ContactsEncryptedOnly, &s.UsersEncryptedOnly)
	return s, err
}

// sealedColumn is a column that holds values sealed with a data key, or
// blind indexes: expr yields them as BYTEA, in the format of
// keys.SealField, which starts with the version of the key.
type sealedColumn struct {
	table, column, expr string
}

// sealedColumns are the columns of each purpose whose values need their
// key version to stay readable, or findable.
var sealedColumns = map[keys.Purpose][]sealedColumn{
	keys.PurposeFieldEnc: {
		{"user_contacts", "value_enc", "value_enc"},
		{"users", "first_name_enc", "first_name_enc"},
		{"users", "last_name_enc", "last_name_enc"},
		{"users", "display_name_enc", "display_name_enc"},
		{"users", "last_login_ip_enc", "last_login_ip_enc"},
		// The authenticator secret of an administrator is "v1:" and the
		// sealed value in base64.
		{"users", "totp_secret", "CASE WHEN totp_secret LIKE 'v1:%' THEN decode(substring(totp_secret FROM 4), 'base64') END"},
		{"user_totp", "secret_sealed", "secret_sealed"},
		{"user_national_ids", "national_id_sealed", "national_id_sealed"},
		{"api_keys", "secret_sealed", "secret_sealed"},
		{"withdrawal_destinations", "value_enc", "value_enc"},
		{"withdrawal_destinations", "memo_enc", "memo_enc"},
		{"user_anti_phishing_codes", "phrase_enc", "phrase_enc"},
	},
	keys.PurposeBlindIndex: {
		{"user_contacts", "value_bidx", "value_bidx"},
		{"withdrawal_destinations", "value_bidx", "value_bidx"},
	},
}

// KeyInUse returns the column, as "table.column", that still holds a value
// sealed with version of field-enc, or a blind index of version of
// blind-index, or "" when none does. Retiring that version would leave the
// value unreadable, or the contact unfindable: RekeyFields moves the
// personal fields to the current versions first. The other purposes have no
// such columns.
func KeyInUse(ctx context.Context, q *sql.DB, purpose keys.Purpose, version int) (string, error) {
	prefix := string(field.Prefix(version))
	for _, c := range sealedColumns[purpose] {
		var used bool
		err := q.QueryRowContext(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE substring(%s FROM 1 FOR %d) = '%s'::bytea)`,
			c.table, c.expr, len(prefix), prefix)).Scan(&used)
		if err != nil {
			return "", fmt.Errorf("reading %s.%s: %w", c.table, c.column, err)
		}
		if used {
			return c.table + "." + c.column, nil
		}
	}
	return "", nil
}
