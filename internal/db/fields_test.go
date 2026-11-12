package db

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fieldsTestDBName = "testdb_db_fields"

// fieldKeys turns on row seals in mode and field encryption with the data
// keys of a new local KEK, as a server with keys does, until the test
// ends. It returns the store of the keys, to rotate them, and the keys.
func fieldKeys(t *testing.T, conn *sql.DB, mode string) (*keys.Store, *keys.Live) {
	t.Helper()
	kek := make([]byte, 32)
	_, err := rand.Read(kek)
	require.NoError(t, err)
	provider, err := keys.NewLocalProvider(base64.StdEncoding.EncodeToString(kek))
	require.NoError(t, err)
	store := &keys.Store{DB: conn, Provider: provider}
	live, err := keys.NewLive(context.Background(), store, keys.LiveOptions{})
	require.NoError(t, err)
	sealer := rowseal.New(conn, live, mode)
	sealer.Logger = log.New(io.Discard, "", 0)
	t.Cleanup(rowseal.Use(sealer))
	t.Cleanup(audit.UseSealer(sealer))
	t.Cleanup(field.Use(field.New(live)))
	return store, live
}

// rotateKeys creates the next version of the field-enc and blind-index
// keys, active at once, and loads them.
func rotateKeys(t *testing.T, store *keys.Store, live *keys.Live) {
	t.Helper()
	for _, purpose := range []keys.Purpose{keys.PurposeFieldEnc, keys.PurposeBlindIndex} {
		_, err := store.Rotate(context.Background(), purpose)
		require.NoError(t, err)
	}
	changed, err := live.Reload(context.Background())
	require.NoError(t, err)
	require.True(t, changed)
	// The new versions are active by the clock of the database, which may
	// run a little ahead of this one.
	require.Eventually(t, func() bool {
		enc, err1 := live.Current(keys.PurposeFieldEnc)
		idx, err2 := live.Current(keys.PurposeBlindIndex)
		return err1 == nil && err2 == nil && enc.Version() == 2 && idx.Version() == 2
	}, 5*time.Second, 10*time.Millisecond)
}

// newUser creates a user and returns its hk.
func newUser(t *testing.T, conn *sql.DB) string {
	t.Helper()
	var hk string
	require.NoError(t, conn.QueryRow(`INSERT INTO users (status) VALUES ('ACTIVE') RETURNING hk::text`).Scan(&hk))
	return hk
}

// addContact adds a verified primary contact to a user, as a sign-in does.
func addContact(t *testing.T, conn *sql.DB, userHK, contactType, value string) error {
	t.Helper()
	tx, err := conn.Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	if err := InsertContact(context.Background(), tx, userHK, contactType, value, true); err != nil {
		return err
	}
	return tx.Commit()
}

// contactRow is what user_contacts holds of a contact.
type contactRow struct {
	id            int64
	plain         *string
	sealed, index []byte
}

func readContact(t *testing.T, conn *sql.DB, userHK string) contactRow {
	t.Helper()
	var r contactRow
	require.NoError(t, conn.QueryRow(`SELECT id, value, value_enc, value_bidx FROM user_contacts WHERE user_hk = $1`, userHK).
		Scan(&r.id, &r.plain, &r.sealed, &r.index))
	return r
}

// lookUp returns the id of the contact that ContactMatch finds, or 0.
func lookUp(t *testing.T, conn *sql.DB, contactType, value string) int64 {
	t.Helper()
	var id int64
	err := conn.QueryRow(`SELECT c.id FROM user_contacts c WHERE `+ContactMatch, ContactArgs(contactType, value)...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	require.NoError(t, err)
	return id
}

// sealMatches reports whether the row of t with key matches its seal.
func sealMatches(t *testing.T, conn *sql.DB, live *keys.Live, table *rowseal.Table, key string) bool {
	t.Helper()
	r := table.NewRow()
	require.NoError(t, conn.QueryRow(`SELECT `+table.Fields("")+` FROM `+table.Name+` WHERE `+table.Key+`::text = $1`, key).Scan(r.Dest()...))
	enforcing := rowseal.New(conn, live, rowseal.ModeEnforce)
	enforcing.Logger = log.New(io.Discard, "", 0)
	return enforcing.Check(context.Background(), table, r) == nil
}

func TestContacts_EncryptedAndFoundByTheirBlindIndex(t *testing.T) {
	conn := pgtest.Migrated(t, fieldsTestDBName)
	_, live := fieldKeys(t, conn, rowseal.ModeEnforce)
	owner := newUser(t, conn)
	require.NoError(t, addContact(t, conn, owner, "email", "Someone@Example.com"))

	r := readContact(t, conn, owner)
	assert.Nil(t, r.plain, "with data keys, no plaintext is stored")
	assert.False(t, bytes.Contains(bytes.ToLower(r.sealed), []byte("someone")), "the ciphertext holds no plaintext")
	opened, err := OpenContact(r.id, nil, r.sealed)
	require.NoError(t, err)
	assert.Equal(t, "Someone@Example.com", opened)
	index, err := field.Active().Index("user_contacts.value:email", "someone@example.com")
	require.NoError(t, err)
	assert.Equal(t, index, r.index, "the index is of the folded value")
	assert.True(t, sealMatches(t, conn, live, rowseal.UserContacts, formatID(r.id)), "the seal covers the new columns")

	assert.Equal(t, r.id, lookUp(t, conn, "email", "SOMEONE@example.COM"))
	assert.Zero(t, lookUp(t, conn, "username", "someone@example.com"), "a type has indexes of its own")

	t.Run("the blind index keeps contacts unique", func(t *testing.T) {
		err := addContact(t, conn, newUser(t, conn), "email", "someone@EXAMPLE.com")
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "23505", pgErr.Code)
		assert.Equal(t, "user_contacts_value_bidx_unique", pgErr.ConstraintName)
	})

	t.Run("against a contact still in plaintext", func(t *testing.T) {
		// Written without keys, as before the job reached it.
		restore := field.Use(nil)
		require.NoError(t, addContact(t, conn, newUser(t, conn), "mobile", "09121112233"))
		restore()
		err := addContact(t, conn, newUser(t, conn), "mobile", "09121112233")
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "user_contacts_value_unique", pgErr.ConstraintName, "the plaintext is written first, then emptied")
	})

	t.Run("a removed contact matches nothing", func(t *testing.T) {
		_, err := conn.Exec(`UPDATE user_contacts SET removed_at = NOW() WHERE id = $1`, r.id)
		require.NoError(t, err)
		assert.Zero(t, lookUp(t, conn, "email", "someone@example.com"))
	})
}

// formatID is the key of the seal row of the contact id.
func formatID(id int64) string { return ContactPlace(id).Row }

func TestContacts_WithoutDataKeysStayInPlaintext(t *testing.T) {
	conn := pgtest.Migrated(t, fieldsTestDBName)
	t.Cleanup(field.Use(nil))
	owner := newUser(t, conn)
	require.NoError(t, addContact(t, conn, owner, "mobile", "09123456789"))
	r := readContact(t, conn, owner)
	assert.Nil(t, r.sealed)
	assert.Nil(t, r.index)
	assert.Equal(t, r.id, lookUp(t, conn, "mobile", "09123456789"), "the plaintext finds it")
	value, err := OpenContact(r.id, r.plain, r.sealed)
	require.NoError(t, err)
	assert.Equal(t, "09123456789", value)

	_, err = RekeyFields(context.Background(), conn)
	require.ErrorIs(t, err, field.ErrNoCipher)
}

func TestRekeyFields_EncryptsOldRowsAndMovesThemToNewKeys(t *testing.T) {
	ctx := context.Background()
	conn := pgtest.Migrated(t, fieldsTestDBName)

	// Rows written without keys, as development and the releases before
	// #140 write them.
	restore := field.Use(nil)
	owner := newUser(t, conn)
	require.NoError(t, addContact(t, conn, owner, "email", "old@example.com"))
	_, err := conn.Exec(`UPDATE users SET first_name = 'Sara', display_name = 'sara' WHERE hk = $1`, owner)
	require.NoError(t, err)
	restore()

	store, live := fieldKeys(t, conn, rowseal.ModeReport)
	// A row as the first release of #140 wrote it: in plaintext and
	// encrypted.
	both := newUser(t, conn)
	require.NoError(t, addContact(t, conn, both, "email", "both@example.com"))
	bothID := readContact(t, conn, both).id
	resealed, err := field.Active().Seal(ContactPlace(bothID), "both@example.com")
	require.NoError(t, err)
	_, err = conn.Exec(`UPDATE user_contacts SET value = 'both@example.com', value_enc = $2 WHERE id = $1`, bothID, resealed)
	require.NoError(t, err)

	status, err := ReadFieldsStatus(ctx, conn)
	require.NoError(t, err)
	assert.Equal(t, FieldsStatus{ContactsToRekey: 2, UsersToRekey: 1}, status)
	column, err := KeyInUse(ctx, conn, keys.PurposeFieldEnc, 1)
	require.NoError(t, err)
	assert.Equal(t, "user_contacts.value_enc", column)

	report, err := RekeyFields(ctx, conn)
	require.NoError(t, err)
	assert.Equal(t, FieldsReport{Contacts: 2, Users: 1}, report)
	r := readContact(t, conn, owner)
	require.NotNil(t, r.sealed)
	assert.Nil(t, r.plain, "the plaintext is emptied")
	assert.Nil(t, readContact(t, conn, both).plain, "also next to a ciphertext")
	assert.Equal(t, r.id, lookUp(t, conn, "email", "old@example.com"))
	assert.Equal(t, bothID, lookUp(t, conn, "email", "both@example.com"))
	var plainNames int
	require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM users WHERE first_name IS NOT NULL OR last_name IS NOT NULL OR display_name IS NOT NULL`).
		Scan(&plainNames))
	assert.Zero(t, plainNames)
	names := readNames(t, conn, owner)
	require.NotNil(t, names.First)
	assert.Equal(t, "Sara", *names.First)
	assert.Nil(t, names.Last, "a name the user did not set stays NULL")
	assert.Equal(t, "sara", *names.Display)
	assert.True(t, sealMatches(t, conn, live, rowseal.UserContacts, formatID(r.id)))
	assert.True(t, sealMatches(t, conn, live, rowseal.Users, owner))
	status, err = ReadFieldsStatus(ctx, conn)
	require.NoError(t, err)
	assert.Equal(t, FieldsStatus{ContactsEncryptedOnly: 2, UsersEncryptedOnly: 1}, status)
	report, err = RekeyFields(ctx, conn)
	require.NoError(t, err)
	assert.Zero(t, report, "a second run has nothing to do")

	column, err = KeyInUse(ctx, conn, keys.PurposeBlindIndex, 1)
	require.NoError(t, err)
	assert.Equal(t, "user_contacts.value_bidx", column)

	t.Run("after a rotation", func(t *testing.T) {
		rotateKeys(t, store, live)
		assert.Equal(t, r.id, lookUp(t, conn, "email", "old@example.com"), "the older index still finds it")
		status, err := ReadFieldsStatus(ctx, conn)
		require.NoError(t, err)
		assert.Equal(t, FieldsStatus{ContactsToRekey: 2, UsersToRekey: 1, ContactsEncryptedOnly: 2, UsersEncryptedOnly: 1}, status)

		report, err := RekeyFields(ctx, conn)
		require.NoError(t, err)
		assert.Equal(t, FieldsReport{Contacts: 2, Users: 1}, report)
		after := readContact(t, conn, owner)
		for name, b := range map[string][]byte{"ciphertext": after.sealed, "index": after.index} {
			version, ok := field.Version(b)
			require.True(t, ok, name)
			assert.Equal(t, 2, version, name)
		}
		assert.Equal(t, r.id, lookUp(t, conn, "email", "old@example.com"))
		for _, purpose := range []keys.Purpose{keys.PurposeFieldEnc, keys.PurposeBlindIndex} {
			column, err := KeyInUse(ctx, conn, purpose, 1)
			require.NoError(t, err)
			assert.Empty(t, column, "%s/v1 can be retired", purpose)
		}
		assert.True(t, sealMatches(t, conn, live, rowseal.UserContacts, formatID(r.id)))
		assert.True(t, sealMatches(t, conn, live, rowseal.Users, owner))
	})
}

func TestRekeyFields_SkipsRowsThatFailTheirSealInEnforceMode(t *testing.T) {
	ctx := context.Background()
	conn := pgtest.Migrated(t, fieldsTestDBName)
	_, _ = fieldKeys(t, conn, rowseal.ModeEnforce)
	owner := newUser(t, conn)
	require.NoError(t, addContact(t, conn, owner, "email", "sealed@example.com"))
	// Changed outside the backend, and in plaintext only: encrypting and
	// sealing it again would hide the change.
	_, err := conn.Exec(`UPDATE user_contacts SET value = 'changed@example.com' WHERE user_hk = $1`, owner)
	require.NoError(t, err)

	report, err := RekeyFields(ctx, conn)
	require.NoError(t, err)
	assert.Equal(t, FieldsReport{Skipped: 1}, report)
	assert.Nil(t, readContact(t, conn, owner).sealed)
}

// readNames reads the names of a user, opened.
func readNames(t *testing.T, conn *sql.DB, userHK string) Names {
	t.Helper()
	var plain Names
	var sealed SealedNames
	require.NoError(t, conn.QueryRow(`SELECT first_name, last_name, display_name, first_name_enc, last_name_enc, display_name_enc
		FROM users WHERE hk = $1`, userHK).Scan(&plain.First, &plain.Last, &plain.Display, &sealed.First, &sealed.Last, &sealed.Display))
	if plain.First != nil {
		require.NotNil(t, sealed.First, "a first name is encrypted")
	}
	names, err := OpenNames(userHK, Names{}, sealed)
	require.NoError(t, err)
	return names
}

func TestPlaintextWrittenAlone_DropsItsCiphertext(t *testing.T) {
	conn := pgtest.Migrated(t, fieldsTestDBName)
	_, _ = fieldKeys(t, conn, rowseal.ModeReport)
	owner := newUser(t, conn)
	require.NoError(t, addContact(t, conn, owner, "email", "before@example.com"))
	sealed, err := SealNames(owner, Names{First: ptr("Ali"), Last: ptr("Ahmadi")})
	require.NoError(t, err)
	_, err = conn.Exec(`UPDATE users SET first_name = 'Ali', last_name = 'Ahmadi', first_name_enc = $2, last_name_enc = $3 WHERE hk = $1`,
		owner, sealed.First, sealed.Last)
	require.NoError(t, err)

	// An instance of a release before #140 writes the plaintext alone.
	_, err = conn.Exec(`UPDATE user_contacts SET value = 'after@example.com' WHERE user_hk = $1`, owner)
	require.NoError(t, err)
	_, err = conn.Exec(`UPDATE users SET first_name = 'Reza' WHERE hk = $1`, owner)
	require.NoError(t, err)

	r := readContact(t, conn, owner)
	assert.Nil(t, r.sealed, "the ciphertext of the old value is gone")
	assert.Nil(t, r.index, "and its index, which would find the old address")
	assert.Zero(t, lookUp(t, conn, "email", "before@example.com"))
	assert.Equal(t, r.id, lookUp(t, conn, "email", "after@example.com"))
	var first, last []byte
	require.NoError(t, conn.QueryRow(`SELECT first_name_enc, last_name_enc FROM users WHERE hk = $1`, owner).Scan(&first, &last))
	assert.Nil(t, first)
	assert.NotNil(t, last, "the name that did not change keeps its ciphertext")

	// The job encrypts the new values.
	_, err = RekeyFields(context.Background(), conn)
	require.NoError(t, err)
	names := readNames(t, conn, owner)
	assert.Equal(t, "Reza", *names.First)
	assert.Equal(t, "Ahmadi", *names.Last)
	value, err := OpenContact(r.id, nil, readContact(t, conn, owner).sealed)
	require.NoError(t, err)
	assert.Equal(t, "after@example.com", value)
}

func ptr(s string) *string { return &s }

func TestDecryptFields_LetsTheDownMigrationKeepTheValues(t *testing.T) {
	ctx := context.Background()
	m := newMigrationsTestMigrator(t)
	require.NoError(t, m.Up())
	conn, err := sql.Open("pgx", migrationsTestDBConnStr())
	require.NoError(t, err)
	defer conn.Close()
	_, _ = fieldKeys(t, conn, rowseal.ModeReport)

	owner := newUser(t, conn)
	require.NoError(t, addContact(t, conn, owner, "email", "kept@example.com"))
	id := readContact(t, conn, owner).id
	// Stored encrypted only, as the next release stores them.
	sealedContact, err := field.Active().Seal(ContactPlace(id), "kept@example.com")
	require.NoError(t, err)
	_, err = conn.Exec(`UPDATE user_contacts SET value = NULL, value_enc = $2 WHERE id = $1`, id, sealedContact)
	require.NoError(t, err)
	sealedNames, err := SealNames(owner, Names{First: ptr("Mina")})
	require.NoError(t, err)
	_, err = conn.Exec(`UPDATE users SET first_name = NULL, first_name_enc = $2 WHERE hk = $1`, owner, sealedNames.First)
	require.NoError(t, err)

	// The migrations after 16 go down first; 16 itself refuses.
	const fieldsMigration = 16
	err = m.Migrate(fieldsMigration - 1)
	require.Error(t, err, "the down migration refuses to lose the values")
	assert.Contains(t, err.Error(), "admin fields decrypt")
	var stillThere bool
	require.NoError(t, conn.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name = 'user_contacts' AND column_name = 'value_enc')`).Scan(&stillThere))
	require.True(t, stillThere, "the refused migration changed nothing")
	require.NoError(t, m.Force(fieldsMigration))

	status, err := ReadFieldsStatus(ctx, conn)
	require.NoError(t, err)
	assert.Equal(t, int64(1), status.ContactsEncryptedOnly)
	assert.Equal(t, int64(1), status.UsersEncryptedOnly)
	report, err := DecryptFields(ctx, conn)
	require.NoError(t, err)
	assert.Equal(t, FieldsReport{Contacts: 1, Users: 1}, report)

	require.NoError(t, m.Migrate(fieldsMigration-1))
	var value, first string
	require.NoError(t, conn.QueryRow(`SELECT value FROM user_contacts WHERE id = $1`, id).Scan(&value))
	require.NoError(t, conn.QueryRow(`SELECT first_name FROM users WHERE hk = $1`, owner).Scan(&first))
	assert.Equal(t, "kept@example.com", value)
	assert.Equal(t, "Mina", first)
}

func TestContactMatch_UsesTheUniqueIndexes(t *testing.T) {
	ctx := context.Background()
	conn := pgtest.Migrated(t, fieldsTestDBName)
	_, _ = fieldKeys(t, conn, rowseal.ModeReport)
	// Enough contacts, encrypted and not, for the planner to prefer the
	// indexes to a scan.
	_, err := conn.ExecContext(ctx, `WITH u AS (INSERT INTO users (status) SELECT 'ACTIVE' FROM generate_series(1, 5000) RETURNING hk)
		INSERT INTO user_contacts (user_hk, type, value, value_enc, value_bidx)
		SELECT hk, 'email', 'user' || n || '@example.com',
			CASE WHEN n % 2 = 0 THEN '\x01'::bytea END, CASE WHEN n % 2 = 0 THEN gen_random_bytes(34) END
		FROM (SELECT hk, row_number() OVER () AS n FROM u) numbered`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `ANALYZE user_contacts`)
	require.NoError(t, err)
	rows, err := conn.QueryContext(ctx, `EXPLAIN SELECT c.id FROM user_contacts c WHERE `+ContactMatch, ContactArgs("email", "someone@example.com")...)
	require.NoError(t, err)
	defer rows.Close()
	var plan string
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		plan += line + "\n"
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, plan, "user_contacts_value_bidx_unique", plan)
	assert.Contains(t, plan, "user_contacts_value_unique", plan)
	assert.NotContains(t, plan, "Seq Scan", plan)
}
