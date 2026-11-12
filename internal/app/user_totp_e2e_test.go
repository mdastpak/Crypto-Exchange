package app

import (
	"encoding/base32"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/totp"
	"github.com/mdastpak/crypto-exchange/internal/userauth"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	totpRoute        = "/api/v1/user/totp"
	totpConfirmRoute = "/api/v1/user/totp/confirm"
)

// totpClock is the clock of the authenticator checks of a test. Each code
// that a test makes comes from a later time step, as a user waits for the
// next code of the app.
type totpClock struct{ now time.Time }

func (c *totpClock) next(secret []byte) string {
	c.now = c.now.Add(2 * totp.Period)
	return totp.Code(secret, totp.Step(c.now))
}

// withTOTP gives the user of token, who has a password and a fresh proof of
// it, an authenticator, and returns its secret and clock.
func (e *e2eEnv) withTOTP(t *testing.T, token string) ([]byte, *totpClock) {
	t.Helper()
	clock := &totpClock{now: time.Now()}
	e.handlers.TOTP.Now = func() time.Time { return clock.now }
	status, resp := e.post(t, totpRoute, nil, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Contains(t, resp.Info["uri"], "otpauth://totp/")
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(resp.Info["secret"].(string))
	require.NoError(t, err)
	status, resp = e.post(t, totpConfirmRoute, map[string]string{"code": clock.next(secret)}, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	require.NotEmpty(t, resp.Info["security_hold_until"])
	return secret, clock
}

func (e *e2eEnv) del(t *testing.T, path, token string) (int, e2eResponse) {
	t.Helper()
	status, raw := e.call(t, http.MethodDelete, path, nil, token)
	var resp e2eResponse
	require.NoError(t, json.Unmarshal(raw, &resp), string(raw))
	return status, resp
}

// stepUpWithPassword proves the password in the session of token.
func (e *e2eEnv) stepUpWithPassword(t *testing.T, token string) {
	t.Helper()
	status, resp := e.post(t, stepUpRoute, map[string]string{"password": testPassword}, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
}

// TestUserTOTP_IsASecondStepInsteadOfThePassword enrolls an authenticator
// app (#307) and uses it where the password is asked for.
func TestUserTOTP_IsASecondStepInsteadOfThePassword(t *testing.T) {
	env := newE2EEnv(t, loginLimit)
	const email = "authenticator@example.com"
	token, _ := env.signedIn(t, email)

	// An authenticator comes after the password, and needs a fresh proof.
	status, resp := env.post(t, totpRoute, nil, token)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "second_factor_not_set", resp.Code)
	env.setPassword(t, token, testPassword)
	status, resp = env.post(t, totpRoute, nil, token)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "step_up_required", resp.Code)
	env.stepUpWithPassword(t, token)

	// A secret does nothing until its first code is right.
	clock := &totpClock{now: time.Now()}
	env.handlers.TOTP.Now = func() time.Time { return clock.now }
	status, resp = env.post(t, totpRoute, nil, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	status, resp = env.post(t, totpConfirmRoute, map[string]string{"code": "000000"}, token)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "totp_invalid", resp.Code)
	status, resp = env.signInUser(t, email)
	require.Equal(t, http.StatusAccepted, status)
	assert.Equal(t, []interface{}{"password"}, resp.Info["methods"])
	var stored []byte
	require.NoError(t, env.db.QueryRow(`SELECT secret_sealed FROM user_totp`).Scan(&stored))

	secret, clock := env.withTOTP(t, token)
	var sealed []byte
	require.NoError(t, env.db.QueryRow(`SELECT secret_sealed FROM user_totp`).Scan(&sealed))
	assert.NotContains(t, string(sealed), string(secret), "the secret is sealed")
	assert.NotEqual(t, stored, sealed, "a new enrollment replaces a secret that was not confirmed")
	status, resp = env.post(t, totpRoute, nil, token)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "totp_enabled", resp.Code)
	status, body := env.sessionCall(t, http.MethodGet, "/api/v1/user/security", token)
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, true, body["info"].(map[string]interface{})["totp_enabled"])

	// The sign-in offers both, and a code of the app finishes it.
	status, resp = env.signInUser(t, email)
	require.Equal(t, http.StatusAccepted, status)
	assert.Equal(t, []interface{}{"password", "totp"}, resp.Info["methods"])
	assert.Equal(t, []interface{}{"recovery_code", "totp"}, resp.Info["password_reset"])
	challenge := resp.Info["challenge"].(string)
	status, resp = env.post(t, secondStepRoute, map[string]string{"challenge": challenge, "totp_code": "000000"}, "")
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, "totp_invalid", resp.Code)
	status, resp = env.post(t, secondStepRoute, map[string]string{"challenge": challenge, "totp_code": "123", "password": "x"}, "")
	assert.Equal(t, http.StatusBadRequest, status)
	code := clock.next(secret)
	status, resp = env.post(t, secondStepRoute, map[string]string{"challenge": challenge, "totp_code": code}, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	signedIn := resp.Info["access_token"].(string)
	var method string
	require.NoError(t, env.db.QueryRow(`SELECT auth_method FROM sessions ORDER BY created_at DESC LIMIT 1`).Scan(&method))
	assert.Equal(t, "OTP+TOTP", method)

	// A code is used once, also for another challenge.
	status, resp = env.post(t, secondStepRoute, map[string]string{"challenge": env.challengeOf(t, email), "totp_code": code}, "")
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, "totp_invalid", resp.Code)

	// The sign-in with the app is a fresh proof, and a step-up takes a
	// code too.
	userHK, sessionID := sessionOf(t, signedIn)
	assert.NoError(t, env.handlers.User.Guard.(*userauth.Guard).Check(t.Context(), env.db, userHK, sessionID, true))
	_, err := env.db.Exec(`UPDATE session_step_ups SET elevated_until = NOW() - INTERVAL '1 second'`)
	require.NoError(t, err)
	status, resp = env.post(t, stepUpRoute, map[string]string{"totp_code": clock.next(secret)}, signedIn)
	require.Equal(t, http.StatusOK, status, resp.Message)

	// Turning it off leaves the password.
	status, resp = env.del(t, totpRoute, signedIn)
	require.Equal(t, http.StatusOK, status, resp.Message)
	status, resp = env.signInUser(t, email)
	require.Equal(t, http.StatusAccepted, status)
	assert.Equal(t, []interface{}{"password"}, resp.Info["methods"])
	status, resp = env.post(t, secondStepRoute, map[string]string{"challenge": resp.Info["challenge"].(string), "totp_code": clock.next(secret)}, "")
	assert.Equal(t, http.StatusUnauthorized, status)
	status, resp = env.del(t, totpRoute, signedIn)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "totp_not_enabled", resp.Code)

	enables := auditEvents(t, env.db, audit.ActionUserTOTPEnable)
	require.Len(t, enables, 2)
	assert.Equal(t, audit.Failure, enables[0].Outcome)
	assert.Equal(t, audit.Success, enables[1].Outcome)
	assert.Len(t, auditEvents(t, env.db, audit.ActionUserTOTPDisable), 1)
}

// TestUserTOTP_ResetsAForgottenPasswordAndLocks checks the authenticator as
// the second proof of a password reset, and its own lock.
func TestUserTOTP_ResetsAForgottenPasswordAndLocks(t *testing.T) {
	env := newE2EEnv(t, loginLimit, func(c *config.Configuration) { c.Security.Password.MaxFailures = 3 })
	const email = "forgot-with-app@example.com"
	const next = "a new and better password"
	token, _ := env.signedIn(t, email)
	env.setPassword(t, token, testPassword)
	env.stepUpWithPassword(t, token)
	secret, clock := env.withTOTP(t, token)

	challenge := env.challengeOf(t, email)
	reset := func(code, password string) (int, e2eResponse) {
		return env.post(t, resetRoute, map[string]string{"challenge": challenge, "totp_code": code, "new_password": password}, "")
	}
	status, resp := reset(clock.next(secret), "short")
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, resp.Info["errors"], "new_password")
	status, resp = reset("000000", next)
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, "totp_invalid", resp.Code)
	status, resp = reset(clock.next(secret), next)
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Equal(t, http.StatusUnauthorized, env.profileStatus(t, token), "every older session is ended")
	assert.EqualValues(t, 10, env.recoveryCodesLeft(t, resp.Info["access_token"].(string)), "no recovery code was used")
	status, _ = env.secondStep(t, env.challengeOf(t, email), next)
	assert.Equal(t, http.StatusOK, status)

	// Wrong codes lock the authenticator, and the password still works.
	challenge = env.challengeOf(t, email)
	for range 3 {
		status, _ = env.post(t, secondStepRoute, map[string]string{"challenge": challenge, "totp_code": "000000"}, "")
		require.Equal(t, http.StatusUnauthorized, status)
	}
	status, resp = env.post(t, secondStepRoute, map[string]string{"challenge": challenge, "totp_code": clock.next(secret)}, "")
	assert.Equal(t, http.StatusTooManyRequests, status)
	assert.Equal(t, "totp_locked", resp.Code)
	status, _ = env.secondStep(t, challenge, next)
	assert.Equal(t, http.StatusOK, status)
}

// TestUserTOTP_ACodeCancelsARecovery checks that the authenticator stops an
// account recovery as the password does.
func TestUserTOTP_ACodeCancelsARecovery(t *testing.T) {
	env := newE2EEnv(t, loginLimit)
	const email = "app-owner@example.com"
	token, _ := env.signedIn(t, email)
	env.setPassword(t, token, testPassword)
	env.stepUpWithPassword(t, token)
	secret, clock := env.withTOTP(t, token)

	status, resp := env.recoverWith(t, env.challengeOf(t, email), "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	status, resp = env.post(t, secondStepRoute, map[string]string{"challenge": env.challengeOf(t, email), "totp_code": clock.next(secret)}, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	var state, reason string
	require.NoError(t, env.db.QueryRow(`SELECT status, end_reason FROM account_recoveries`).Scan(&state, &reason))
	assert.Equal(t, "cancelled", state)
	assert.Equal(t, "second_factor_proven", reason)
}
