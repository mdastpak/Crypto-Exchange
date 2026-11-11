package main

import (
	"net/http"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUserPassword_ADeletedPasswordKeepsTheSecondStep deletes the password
// row of an account in SQL, as someone with direct database access would
// (#337). In enforce mode the code alone does not sign in, and a session of
// the account cannot set a first password over the deleted one; the
// deletion is a row_missing finding.
func TestUserPassword_ADeletedPasswordKeepsTheSecondStep(t *testing.T) {
	env := newE2EEnv(t)
	t.Cleanup(rowseal.Use(rowseal.New(env.db, env.handlers.DataKeys, rowseal.ModeEnforce)))
	const contact = "deleted-password@example.com"
	token, _ := env.signedIn(t, contact)
	env.setPassword(t, token, testPassword)
	var recorded bool
	require.NoError(t, env.db.QueryRow(`SELECT u.password_set_at IS NOT NULL FROM users u
		JOIN user_contacts c ON c.user_hk = u.hk WHERE c.value = $1`, contact).Scan(&recorded))
	require.True(t, recorded, "the users row records the password")

	_, err := e2eOwnerDB(t).Exec(`DELETE FROM user_passwords
		WHERE user_hk = (SELECT user_hk FROM user_contacts WHERE value = $1)`, contact)
	require.NoError(t, err)

	status, resp := env.signInUser(t, contact)
	assert.Equal(t, http.StatusForbidden, status, "the code alone signs in to no account with a recorded password: %s", resp.Message)
	assert.Equal(t, "account_unavailable", resp.Code)

	status, resp = env.put(t, passwordRoute, map[string]string{"new_password": "the attacker's password"}, token)
	assert.Equal(t, http.StatusForbidden, status, resp.Message)
	assert.Equal(t, "account_unavailable", resp.Code)

	var check string
	require.NoError(t, env.db.QueryRow(`SELECT check_failed FROM integrity_findings
		WHERE table_name = 'user_passwords' AND resolved_at IS NULL`).Scan(&check))
	assert.Equal(t, rowseal.CheckRowMissing, check)
}
