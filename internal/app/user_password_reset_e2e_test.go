package app

import (
	"net/http"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	resetRoute         = "/api/v1/public/auth/password-reset"
	recoveryCodesRoute = "/api/v1/user/recovery-codes"
)

// withRecoveryCodes gives the user of token a first password and returns
// the recovery codes that came with it.
func (e *e2eEnv) withRecoveryCodes(t *testing.T, token string) []string {
	t.Helper()
	status, resp := e.put(t, passwordRoute, map[string]string{"new_password": testPassword}, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	raw, _ := resp.Info["recovery_codes"].([]interface{})
	require.Len(t, raw, 10)
	codes := make([]string, len(raw))
	for i, code := range raw {
		codes[i] = code.(string)
		require.Regexp(t, `^[A-Z2-9]{4}(-[A-Z2-9]{4}){3}$`, codes[i])
	}
	return codes
}

func (e *e2eEnv) resetPassword(t *testing.T, challenge, code, password string) (int, e2eResponse) {
	t.Helper()
	return e.post(t, resetRoute, map[string]string{"challenge": challenge, "recovery_code": code, "new_password": password}, "")
}

func (e *e2eEnv) recoveryCodesLeft(t *testing.T, token string) float64 {
	t.Helper()
	status, body := e.sessionCall(t, http.MethodGet, "/api/v1/user/security", token)
	require.Equal(t, http.StatusOK, status, body)
	return body["info"].(map[string]interface{})["recovery_codes_left"].(float64)
}

// TestPasswordReset_NeedsACodeAndARecoveryCode resets a forgotten password
// (#307): the code of a contact alone never does.
func TestPasswordReset_NeedsACodeAndARecoveryCode(t *testing.T) {
	env := newE2EEnv(t, func(c *config.Configuration) { c.RateLimits.Auth.LoginAttemptsPerMinute = 1000 })
	const contact = "forgot@example.com"
	const next = "a new and better password"
	token, _ := env.signedIn(t, contact)
	codes := env.withRecoveryCodes(t, token)
	assert.EqualValues(t, 10, env.recoveryCodesLeft(t, token))
	var stored string
	require.NoError(t, env.db.QueryRow(`SELECT string_agg(code_hash, ',') FROM user_recovery_codes`).Scan(&stored))
	assert.NotContains(t, stored, codes[0][:4], "only hashes are stored")

	// The correct code of the contact says that a reset is possible.
	status, resp := env.signInUser(t, contact)
	require.Equal(t, http.StatusAccepted, status, resp.Message)
	assert.Equal(t, []interface{}{"recovery_code"}, resp.Info["password_reset"])
	challenge := resp.Info["challenge"].(string)

	status, resp = env.resetPassword(t, challenge, "AAAA-BBBB-CCCC-DDDD", next)
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, "recovery_code_invalid", resp.Code)
	status, resp = env.resetPassword(t, "no-such-challenge", codes[0], next)
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, "challenge_invalid", resp.Code)
	status, resp = env.resetPassword(t, challenge, codes[0], "short")
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, resp.Info["errors"], "new_password")
	assert.EqualValues(t, 10, env.recoveryCodesLeft(t, token), "a refused reset uses no code")

	// A code as a user types it: lower case, with spaces.
	typed := " " + lower(codes[0]) + " "
	status, resp = env.resetPassword(t, challenge, typed, next)
	require.Equal(t, http.StatusOK, status, resp.Message)
	signedIn := resp.Info["access_token"].(string)
	assert.Equal(t, http.StatusUnauthorized, env.profileStatus(t, token), "every older session is ended")
	assert.Equal(t, http.StatusOK, env.profileStatus(t, signedIn))
	assert.EqualValues(t, 9, env.recoveryCodesLeft(t, signedIn))

	resets := auditEvents(t, env.db, audit.ActionUserPasswordReset)
	require.Len(t, resets, 2)
	assert.Equal(t, "recovery_code_invalid", resets[0].Reason.String)
	assert.Equal(t, audit.Success, resets[1].Outcome)
	var held bool
	require.NoError(t, env.db.QueryRow(`SELECT security_hold_until > NOW() FROM users WHERE security_hold_until IS NOT NULL`).Scan(&held))
	assert.True(t, held, "a reset holds withdrawals")

	// The challenge and the code are spent, and only the new password
	// signs in.
	status, _ = env.resetPassword(t, challenge, codes[1], "yet another password")
	assert.Equal(t, http.StatusUnauthorized, status)
	challenge = env.challengeOf(t, contact)
	status, resp = env.resetPassword(t, challenge, codes[0], "yet another password")
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, "recovery_code_invalid", resp.Code)
	status, _ = env.secondStep(t, challenge, testPassword)
	assert.Equal(t, http.StatusUnauthorized, status)
	status, resp = env.secondStep(t, challenge, next)
	assert.Equal(t, http.StatusOK, status, resp.Message)
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// TestPasswordReset_WrongRecoveryCodesLockThePassword checks that the
// recovery codes cannot be guessed past the limit of the password.
func TestPasswordReset_WrongRecoveryCodesLockThePassword(t *testing.T) {
	env := newE2EEnv(t, func(c *config.Configuration) {
		c.Security.Password.MaxFailures = 2
		c.RateLimits.Auth.LoginAttemptsPerMinute = 1000
	})
	const contact = "guessing@example.com"
	token, _ := env.signedIn(t, contact)
	codes := env.withRecoveryCodes(t, token)
	challenge := env.challengeOf(t, contact)

	for range 2 {
		status, _ := env.resetPassword(t, challenge, "AAAA-BBBB-CCCC-DDDD", "a new and better password")
		require.Equal(t, http.StatusUnauthorized, status)
	}
	status, resp := env.resetPassword(t, challenge, codes[0], "a new and better password")
	assert.Equal(t, http.StatusTooManyRequests, status)
	assert.Equal(t, "password_locked", resp.Code)
	assert.EqualValues(t, 10, env.recoveryCodesLeft(t, token))
}

// TestRecoveryCodes_NewOnesReplaceTheOld generates new codes, which needs a
// fresh proof of the password.
func TestRecoveryCodes_NewOnesReplaceTheOld(t *testing.T) {
	env := newE2EEnv(t)
	const contact = "codes@example.com"
	token, _ := env.signedIn(t, contact)

	status, resp := env.post(t, recoveryCodesRoute, nil, token)
	assert.Equal(t, http.StatusConflict, status, "no password yet")
	assert.Equal(t, "second_factor_not_set", resp.Code)

	old := env.withRecoveryCodes(t, token)
	status, resp = env.post(t, recoveryCodesRoute, nil, token)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "step_up_required", resp.Code)

	status, resp = env.post(t, stepUpRoute, map[string]string{"password": testPassword}, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	status, resp = env.post(t, recoveryCodesRoute, nil, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	fresh := resp.Info["recovery_codes"].([]interface{})
	require.Len(t, fresh, 10)
	assert.NotContains(t, fresh, old[0])
	assert.EqualValues(t, 10, env.recoveryCodesLeft(t, token))
	require.Len(t, auditEvents(t, env.db, audit.ActionUserRecoveryCodesGenerate), 1)

	// An old code no longer resets; a new one does.
	challenge := env.challengeOf(t, contact)
	status, _ = env.resetPassword(t, challenge, old[0], "a new and better password")
	assert.Equal(t, http.StatusUnauthorized, status)
	status, resp = env.resetPassword(t, challenge, fresh[0].(string), "a new and better password")
	assert.Equal(t, http.StatusOK, status, resp.Message)
}
