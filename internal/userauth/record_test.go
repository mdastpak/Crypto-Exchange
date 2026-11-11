package userauth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/passwordhash"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDBName is the PostgreSQL database of this package.
const testDBName = "testdb_userauth"

const recordTestPassword = "correct horse battery"

// newRecordEnv migrates the test database and seals its rows in mode, with
// throwaway data keys.
func newRecordEnv(t *testing.T, mode string) (*Passwords, *sql.DB) {
	t.Helper()
	db := pgtest.Migrated(t, testDBName)
	kek := make([]byte, 32)
	_, err := rand.Read(kek)
	require.NoError(t, err)
	provider, err := keys.NewLocalProvider(base64.StdEncoding.EncodeToString(kek))
	require.NoError(t, err)
	live, err := keys.NewLive(context.Background(), &keys.Store{DB: db, Provider: provider}, keys.LiveOptions{})
	require.NoError(t, err)
	t.Cleanup(rowseal.Use(rowseal.New(db, live, mode)))
	return NewPasswords(db, passwordhash.Argon2id{}), db
}

// newSealedUser inserts a user and seals it, as the backend writes one.
func newSealedUser(t *testing.T, db *sql.DB) string {
	t.Helper()
	hk := usertest.Insert(t, db, usertest.User{})
	require.NoError(t, rowseal.Reseal(context.Background(), db, rowseal.Users, "hk = $1", hk))
	return hk
}

func passwordRecorded(t *testing.T, db *sql.DB, hk string) bool {
	t.Helper()
	var recorded bool
	require.NoError(t, db.QueryRow(`SELECT password_set_at IS NOT NULL FROM users WHERE hk = $1`, hk).Scan(&recorded))
	return recorded
}

func setPassword(t *testing.T, p *Passwords, hk string) {
	t.Helper()
	_, err := p.Set(context.Background(), hk, "", recordTestPassword, audit.Event{ActorType: audit.ActorUser, ActorID: hk})
	require.NoError(t, err)
}

// TestPasswords_TheUsersRowRecordsThePassword checks that the first password
// of an account is recorded in its sealed users row (#337).
func TestPasswords_TheUsersRowRecordsThePassword(t *testing.T) {
	p, db := newRecordEnv(t, rowseal.ModeEnforce)
	ctx := context.Background()
	hk := newSealedUser(t, db)

	has, err := p.Has(ctx, hk)
	require.NoError(t, err)
	assert.False(t, has)
	assert.False(t, passwordRecorded(t, db, hk))

	setPassword(t, p, hk)
	assert.True(t, passwordRecorded(t, db, hk))
	has, err = p.Has(ctx, hk)
	require.NoError(t, err, "the users row, with its record, matches its seal")
	assert.True(t, has)
}

// TestPasswords_ADeletedPasswordIsFound deletes the password row of an
// account in SQL, which no seal can show (#337). The record of the users row
// finds it: a row_missing finding, and in enforce mode the account has no
// second factor to prove, no session to elevate, and no first password to
// set.
func TestPasswords_ADeletedPasswordIsFound(t *testing.T) {
	for _, mode := range []string{rowseal.ModeEnforce, rowseal.ModeReport} {
		t.Run(mode, func(t *testing.T) {
			p, db := newRecordEnv(t, mode)
			ctx := context.Background()
			hk := newSealedUser(t, db)
			setPassword(t, p, hk)
			_, err := db.Exec(`DELETE FROM user_passwords WHERE user_hk = $1`, hk)
			require.NoError(t, err)

			has, err := p.Has(ctx, hk)
			_, guardErr := (&Guard{DB: db}).State(ctx, db, hk, "00000000-0000-0000-0000-000000000000")
			_, setErr := p.Set(ctx, hk, "", "the attacker's password", audit.Event{ActorType: audit.ActorUser, ActorID: hk})
			if mode == rowseal.ModeEnforce {
				assert.ErrorIs(t, err, rowseal.ErrUnverified)
				assert.ErrorIs(t, guardErr, rowseal.ErrUnverified, "the guard does not open what the password protects")
				assert.ErrorIs(t, setErr, rowseal.ErrUnverified, "no first password over a deleted one")
			} else {
				require.NoError(t, err)
				assert.False(t, has, "report mode serves the account as one without a password")
				assert.NoError(t, guardErr)
			}

			var check string
			require.NoError(t, db.QueryRow(`SELECT check_failed FROM integrity_findings
				WHERE table_name = 'user_passwords' AND row_key = $1 AND resolved_at IS NULL`, hk).Scan(&check))
			assert.Equal(t, rowseal.CheckRowMissing, check)
		})
	}
}

// TestPasswords_AnOlderAccountRecordsItsPasswordWhenProved gives an account
// the state of the rows written before migration 12: a password, and a users
// row that does not record it, under a valid seal. Proving the password
// records it, and the users row keeps a valid seal.
func TestPasswords_AnOlderAccountRecordsItsPasswordWhenProved(t *testing.T) {
	p, db := newRecordEnv(t, rowseal.ModeEnforce)
	ctx := context.Background()
	hk := newSealedUser(t, db)
	setPassword(t, p, hk)
	_, err := db.Exec(`UPDATE users SET password_set_at = NULL WHERE hk = $1`, hk)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, db, rowseal.Users, "hk = $1", hk))

	has, err := p.Has(ctx, hk)
	require.NoError(t, err)
	assert.True(t, has, "the password row still counts")

	require.NoError(t, p.Verify(ctx, hk, recordTestPassword))
	assert.True(t, passwordRecorded(t, db, hk))
	_, err = p.Has(ctx, hk)
	assert.NoError(t, err, "the users row was sealed with its record")
}

// TestPasswords_ARemovedRecordIsAMismatch sets the record of the users row
// back to NULL in SQL, to hide the password: the users row no longer matches
// its seal.
func TestPasswords_ARemovedRecordIsAMismatch(t *testing.T) {
	p, db := newRecordEnv(t, rowseal.ModeEnforce)
	ctx := context.Background()
	hk := newSealedUser(t, db)
	setPassword(t, p, hk)
	_, err := db.Exec(`UPDATE users SET password_set_at = NULL WHERE hk = $1`, hk)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM user_passwords WHERE user_hk = $1`, hk)
	require.NoError(t, err)

	_, err = p.Has(ctx, hk)
	assert.ErrorIs(t, err, rowseal.ErrUnverified)
	var check string
	require.NoError(t, db.QueryRow(`SELECT check_failed FROM integrity_findings
		WHERE table_name = 'users' AND row_key = $1 AND resolved_at IS NULL`, hk).Scan(&check))
	assert.Equal(t, rowseal.CheckMismatch, check)
}
