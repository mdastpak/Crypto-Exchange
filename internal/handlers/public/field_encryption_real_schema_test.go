package public

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// encryptWith encrypts the personal fields of env's database with live, as
// a server with data keys does (#140).
func (e *sessionTestEnv) encryptWith(t *testing.T, live *keys.Live) {
	t.Helper()
	t.Cleanup(field.Use(field.New(live)))
}

// storedContact reads the row of user_contacts of an email address, found
// by its blind index, and checks that it holds no plaintext.
func (e *sessionTestEnv) storedContact(t *testing.T, email string) (id int64, userHK string, sealed, index []byte) {
	t.Helper()
	var plain *string
	require.NoError(t, e.conn.QueryRow(`SELECT c.id, c.user_hk, c.value, c.value_enc, c.value_bidx FROM user_contacts c WHERE `+db.ContactMatch,
		db.ContactArgs("email", email)...).Scan(&id, &userHK, &plain, &sealed, &index))
	assert.Nil(t, plain, "with data keys, no plaintext is stored")
	return id, userHK, sealed, index
}

func TestFieldEncryption_RealSchema_SignInFindsTheEncryptedContact(t *testing.T) {
	env := newSessionTestEnv(t)
	live := env.dataKeys(t)
	sealer := env.sealWith(t, live, rowseal.ModeEnforce)
	env.encryptWith(t, live)
	const email = "encrypted@example.com"

	env.signIn(t, email)
	id, owner, sealed, index := env.storedContact(t, email)
	require.NotNil(t, sealed, "the contact is encrypted")
	assert.False(t, bytes.Contains(sealed, []byte("encrypted@")), "the ciphertext holds no plaintext")
	opened, err := db.OpenContact(id, nil, sealed)
	require.NoError(t, err)
	assert.Equal(t, email, opened)
	want, err := field.Active().Index("user_contacts.value:email", email)
	require.NoError(t, err)
	assert.Equal(t, want, index)

	// Signing in again, in other letters, finds the same account by the
	// blind index.
	env.signIn(t, "Encrypted@Example.com")
	var accounts int
	require.NoError(t, env.conn.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'user'`).Scan(&accounts))
	assert.Equal(t, 1, accounts, "no second account")
	var contacts int
	require.NoError(t, env.conn.QueryRow(`SELECT COUNT(*) FROM user_contacts WHERE user_hk = $1`, owner).Scan(&contacts))
	assert.Equal(t, 1, contacts)

	report, err := sealer.VerifyAll(context.Background())
	require.NoError(t, err)
	assert.Zero(t, report.Findings, "the seals cover the ciphertext and the index: %+v", env.openFindings(t))
}

func TestFieldEncryption_RealSchema_ABlindIndexCopiedToAnotherAccountSignsInNowhere(t *testing.T) {
	env := newSessionTestEnv(t)
	// Without seals, so that only the ciphertext stands in the way.
	t.Cleanup(rowseal.Use(nil))
	env.encryptWith(t, env.dataKeys(t))
	env.signIn(t, "bidx-victim@example.com")
	env.signIn(t, "bidx-attacker@example.com")
	victim, _, _, _ := env.storedContact(t, "bidx-victim@example.com")
	attacker, _, _, attackerIndex := env.storedContact(t, "bidx-attacker@example.com")

	// The attacker's address would now find the victim's account.
	_, err := env.conn.Exec(`UPDATE user_contacts SET removed_at = NOW() WHERE id = $1`, attacker)
	require.NoError(t, err)
	_, err = env.conn.Exec(`UPDATE user_contacts SET value_bidx = $2 WHERE id = $1`, victim, attackerIndex)
	require.NoError(t, err)

	signIn, code := env.requestCode(t, "bidx-attacker@example.com")
	w := env.send(http.MethodPost, "/public/auth/"+signIn, map[string]string{"otp": code})
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "access_token")
}
