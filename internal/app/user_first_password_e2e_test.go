package app

import (
	"net/http"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// passwordCount counts the account passwords.
func (e *e2eEnv) passwordCount(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, e.db.QueryRow(`SELECT COUNT(*) FROM user_passwords`).Scan(&n))
	return n
}

// TestUserPassword_TheFirstNeedsAContactCode checks that a session alone
// cannot give an account its first password (#401): the first password
// needs a code that was sent to a verified contact, to this session.
func TestUserPassword_TheFirstNeedsAContactCode(t *testing.T) {
	env := newE2EEnv(t, loginLimit)
	const email = "first-password@example.com"
	owner, _ := env.signedIn(t, email)
	stolen, _ := env.signedIn(t, email)

	// The session alone is refused, and the owner keeps the account.
	status, resp := env.put(t, passwordRoute, map[string]string{"new_password": testPassword}, stolen)
	assert.Equal(t, http.StatusForbidden, status, resp.Message)
	assert.Equal(t, "contact_proof_required", resp.Code)
	assert.Zero(t, env.passwordCount(t))
	assert.Equal(t, http.StatusOK, env.profileStatus(t, owner), "the owner's session survives")

	// The code goes to the owner's contact. A guess is refused.
	status, resp = env.post(t, passwordCodeRoute, map[string]string{}, stolen)
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.NotEqual(t, email, resp.Info["code_sent_to"], "the contact is masked")
	sent := env.sender.last(email)
	require.NotEmpty(t, sent)
	guess := "000000"
	if sent == guess {
		guess = "111111"
	}
	status, resp = env.put(t, passwordRoute, map[string]string{"new_password": testPassword, "contact_code": guess}, stolen)
	assert.Equal(t, http.StatusForbidden, status, resp.Message)
	assert.Equal(t, "contact_code_invalid", resp.Code)

	// The code works only in the session that asked for it.
	status, resp = env.put(t, passwordRoute, map[string]string{"new_password": testPassword, "contact_code": sent}, owner)
	assert.Equal(t, http.StatusForbidden, status, resp.Message)
	assert.Equal(t, "contact_proof_required", resp.Code)
	assert.Zero(t, env.passwordCount(t))
	assert.Equal(t, http.StatusOK, env.profileStatus(t, owner))

	// A refused policy does not use up the code.
	code := env.passwordCode(t, owner)
	status, resp = env.put(t, passwordRoute, map[string]string{"new_password": "short", "contact_code": code}, owner)
	require.Equal(t, http.StatusBadRequest, status, resp.Message)
	assert.Contains(t, resp.Info["errors"], "new_password")

	// With the code of a verified contact, the owner sets it, and the
	// other session ends.
	status, resp = env.put(t, passwordRoute, map[string]string{"new_password": testPassword, "contact_code": code}, owner)
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.EqualValues(t, 1, resp.Info["sessions_ended"])
	assert.Len(t, resp.Info["recovery_codes"], 10)
	assert.Equal(t, http.StatusUnauthorized, env.profileStatus(t, stolen))

	// The code is used up, and an account with a password gets no code.
	status, resp = env.post(t, passwordCodeRoute, map[string]string{}, owner)
	assert.Equal(t, http.StatusConflict, status, resp.Message)
	assert.Equal(t, "password_set", resp.Code)

	refused := auditEvents(t, env.db, audit.ActionUserPasswordChange)
	require.Len(t, refused, 3, "each refused proof is recorded")
	for _, event := range refused {
		assert.Equal(t, audit.Denied, event.Outcome)
		assert.Equal(t, "contact_proof_required", event.Reason.String)
	}
	assert.Len(t, auditEvents(t, env.db, audit.ActionUserPasswordSet), 1)
}

// TestUserPassword_CodeRequestIsValidated checks the contact that a code of
// the first password goes to.
func TestUserPassword_CodeRequestIsValidated(t *testing.T) {
	env := newE2EEnv(t, loginLimit)
	const email = "first-password-contact@example.com"
	token, _ := env.signedIn(t, email)

	status, resp := env.post(t, passwordCodeRoute, map[string]string{"send_to": "fax"}, token)
	assert.Equal(t, http.StatusBadRequest, status, resp.Message)
	assert.Contains(t, resp.Info["errors"], "send_to")

	status, resp = env.post(t, passwordCodeRoute, map[string]string{"send_to": "mobile"}, token)
	assert.Equal(t, http.StatusConflict, status, resp.Message)
	assert.Equal(t, "contact_not_found", resp.Code, "the account has no verified mobile number")

	status, resp = env.post(t, passwordCodeRoute, map[string]string{"send_to": "email"}, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.NotEmpty(t, env.sender.last(email))
}

// TestAccountRecovery_AnOlderSessionCannotCancelIt checks that a proof of
// the password in a session that began before a recovery leaves the
// recovery open (#401): whoever holds a stolen session cannot cancel every
// recovery the owner opens. A full sign-in still cancels it.
func TestAccountRecovery_AnOlderSessionCannotCancelIt(t *testing.T) {
	env := newE2EEnv(t, loginLimit)
	const email = "older-session@example.com"
	token, _ := env.signedIn(t, email)
	env.setPassword(t, token, testPassword)

	status, resp := env.recoverWith(t, env.challengeOf(t, email), "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	require.Equal(t, "waiting", resp.Info["status"])

	// A step-up and a password change in the older session.
	env.stepUpWithPassword(t, token)
	const next = "a new and better password"
	status, resp = env.put(t, passwordRoute, map[string]string{"current_password": testPassword, "new_password": next}, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	var open int
	require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM account_recoveries WHERE status = 'waiting'`).Scan(&open))
	assert.Equal(t, 1, open, "the recovery stays open")
	assert.Empty(t, auditEvents(t, env.db, audit.ActionUserRecoveryCancel))

	status, body := env.sessionCall(t, http.MethodGet, "/api/v1/user/security", token)
	require.Equal(t, http.StatusOK, status, body)
	assert.NotNil(t, body["info"].(map[string]interface{})["recovery"], "the session sees the recovery")

	// A sign-in with the password cancels it.
	status, resp = env.secondStep(t, env.challengeOf(t, email), next)
	require.Equal(t, http.StatusOK, status, resp.Message)
	require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM account_recoveries WHERE status = 'waiting'`).Scan(&open))
	assert.Zero(t, open)
	cancels := auditEvents(t, env.db, audit.ActionUserRecoveryCancel)
	require.Len(t, cancels, 1)
	assert.Equal(t, "password_proven", cancels[0].Reason.String)

}
