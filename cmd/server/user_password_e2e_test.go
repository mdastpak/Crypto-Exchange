package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/userauth"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	passwordRoute   = "/api/v1/user/password"
	secondStepRoute = "/api/v1/public/auth/second-step"
	testPassword    = "correct horse battery"
)

// put sends a PUT with a token and returns the status and the envelope.
func (e *e2eEnv) put(t *testing.T, path string, body interface{}, token string) (int, e2eResponse) {
	t.Helper()
	status, raw := e.call(t, http.MethodPut, path, body, token)
	var resp e2eResponse
	require.NoError(t, json.Unmarshal(raw, &resp), string(raw))
	return status, resp
}

// setPassword gives the user of token a first password.
func (e *e2eEnv) setPassword(t *testing.T, token, password string) {
	t.Helper()
	status, resp := e.put(t, passwordRoute, map[string]string{"new_password": password}, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
}

// challengeOf signs in with a code and returns the challenge of the second
// step, which the account must ask for.
func (e *e2eEnv) challengeOf(t *testing.T, contact string) string {
	t.Helper()
	status, resp := e.signInUser(t, contact)
	require.Equal(t, http.StatusAccepted, status, resp.Message)
	challenge, _ := resp.Info["challenge"].(string)
	require.NotEmpty(t, challenge)
	return challenge
}

func (e *e2eEnv) secondStep(t *testing.T, challenge, password string) (int, e2eResponse) {
	t.Helper()
	return e.post(t, secondStepRoute, map[string]string{"challenge": challenge, "password": password}, "")
}

// TestUserPassword_IsTheSecondStepOfTheSignIn sets a password and checks
// that a correct code alone no longer returns a token (#307).
func TestUserPassword_IsTheSecondStepOfTheSignIn(t *testing.T) {
	env := newE2EEnv(t)
	const contact = "password@example.com"
	token, _ := env.signedIn(t, contact)

	status, body := env.sessionCall(t, http.MethodGet, "/api/v1/user/security", token)
	require.Equal(t, http.StatusOK, status, body)
	info := body["info"].(map[string]interface{})
	assert.Equal(t, false, info["password"].(map[string]interface{})["set"])
	assert.EqualValues(t, config.DefaultPasswordMinLength, info["password_min_length"])

	// The policy.
	for password, field := range map[string]string{
		"":             "new_password",
		"short one":    "new_password",
		"aaaaaaaaaaaa": "new_password",
	} {
		status, resp := env.put(t, passwordRoute, map[string]string{"new_password": password}, token)
		require.Equal(t, http.StatusBadRequest, status, password)
		assert.Contains(t, resp.Info["errors"], field, password)
	}

	env.setPassword(t, token, testPassword)
	var hash string
	require.NoError(t, env.db.QueryRow(`SELECT password_hash FROM user_passwords`).Scan(&hash))
	assert.Contains(t, hash, "$argon2id$")
	assert.NotContains(t, hash, testPassword)
	set := auditEvents(t, env.db, audit.ActionUserPasswordSet)
	require.Len(t, set, 1)
	assert.Equal(t, audit.Success, set[0].Outcome)

	status, body = env.sessionCall(t, http.MethodGet, "/api/v1/user/security", token)
	require.Equal(t, http.StatusOK, status, body)
	password := body["info"].(map[string]interface{})["password"].(map[string]interface{})
	assert.Equal(t, true, password["set"])
	assert.NotEmpty(t, password["changed_at"])
	assert.NotContains(t, string(mustJSON(t, body)), "argon2")

	// A correct code now answers 202 with a challenge: no token, and no
	// refresh cookie.
	status, resp := env.post(t, "/api/v1/public/auth", map[string]string{"username": contact}, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	w := env.send(t, http.MethodPost, "/api/v1/public/auth/"+resp.Info["uuid"].(string), map[string]string{"otp": env.sender.last(contact)}, "", "")
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	assert.Nil(t, refreshTokenCookie(w))
	var asked e2eResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &asked))
	assert.NotContains(t, asked.Info, "access_token")
	assert.Equal(t, true, asked.Info["second_step_required"])
	assert.Equal(t, []interface{}{"password"}, asked.Info["methods"])
	challenge := asked.Info["challenge"].(string)

	// A wrong password leaves the challenge usable.
	status, resp = env.secondStep(t, challenge, "not the password")
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, "password_invalid", resp.Code)
	status, resp = env.secondStep(t, "no-such-challenge", testPassword)
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, "challenge_invalid", resp.Code)

	status, resp = env.secondStep(t, challenge, testPassword)
	require.Equal(t, http.StatusOK, status, resp.Message)
	signedIn := resp.Info["access_token"].(string)
	claims, err := auth.ParseToken(signedIn, auth.RoleUser, auth.TokenTypeAccess)
	require.NoError(t, err)
	var method string
	require.NoError(t, env.db.QueryRow(`SELECT auth_method FROM sessions WHERE id = $1`, claims.SessionID).Scan(&method))
	assert.Equal(t, "OTP+PASSWORD", method)

	// The challenge signed in once.
	status, resp = env.secondStep(t, challenge, testPassword)
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, "challenge_invalid", resp.Code)

	// The refused steps are in the audit log, without the password.
	var failures int
	require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = $1 AND reason = 'password_invalid'`,
		audit.ActionUserSignIn).Scan(&failures))
	assert.Equal(t, 1, failures)
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}

// TestUserPassword_LocksAfterWrongAttempts checks the lock, which is in
// PostgreSQL and so holds on every instance.
func TestUserPassword_LocksAfterWrongAttempts(t *testing.T) {
	env := newE2EEnv(t, func(c *config.Configuration) {
		c.Security.Password.MaxFailures = 3
		c.RateLimits.Auth.LoginAttemptsPerMinute = 1000
	})
	const contact = "locked@example.com"
	token, _ := env.signedIn(t, contact)
	env.setPassword(t, token, testPassword)
	challenge := env.challengeOf(t, contact)

	for range 3 {
		status, resp := env.secondStep(t, challenge, "not the password")
		require.Equal(t, http.StatusUnauthorized, status, resp.Message)
	}
	// Locked: the right password is not even checked.
	w := env.send(t, http.MethodPost, secondStepRoute, map[string]string{"challenge": challenge, "password": testPassword}, "", "")
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
	var resp e2eResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "password_locked", resp.Code)

	// A change of the password is locked with it.
	status, resp := env.put(t, passwordRoute, map[string]string{"current_password": testPassword, "new_password": "another good password"}, token)
	assert.Equal(t, http.StatusTooManyRequests, status)
	assert.Equal(t, "password_locked", resp.Code)

	// After the lock, the right password signs in and clears the count.
	_, err := e2eOwnerDB(t).Exec(`UPDATE user_passwords SET locked_until = NOW() - INTERVAL '1 second'`)
	require.NoError(t, err)
	status, resp = env.secondStep(t, challenge, testPassword)
	require.Equal(t, http.StatusOK, status, resp.Message)
	var failed int
	require.NoError(t, env.db.QueryRow(`SELECT failed_attempts FROM user_passwords`).Scan(&failed))
	assert.Zero(t, failed)
}

// TestUserPassword_AChangeNeedsTheCurrentOneAndEndsOtherSessions changes a
// password.
func TestUserPassword_AChangeNeedsTheCurrentOneAndEndsOtherSessions(t *testing.T) {
	env := newE2EEnv(t)
	const contact = "change@example.com"
	first, _ := env.signedIn(t, contact)
	env.setPassword(t, first, testPassword)

	// A second session, signed in with the password.
	status, resp := env.secondStep(t, env.challengeOf(t, contact), testPassword)
	require.Equal(t, http.StatusOK, status, resp.Message)
	second := resp.Info["access_token"].(string)
	require.Equal(t, http.StatusOK, env.profileStatus(t, first))

	const next = "a new and better password"
	status, resp = env.put(t, passwordRoute, map[string]string{"new_password": next}, second)
	require.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, resp.Info["errors"], "current_password")

	status, resp = env.put(t, passwordRoute, map[string]string{"current_password": "not the password", "new_password": next}, second)
	require.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "password_invalid", resp.Code)

	status, resp = env.put(t, passwordRoute, map[string]string{"current_password": testPassword, "new_password": testPassword}, second)
	require.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, resp.Info["errors"], "new_password")

	status, resp = env.put(t, passwordRoute, map[string]string{"current_password": testPassword, "new_password": next}, second)
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.EqualValues(t, 1, resp.Info["sessions_ended"])
	assert.Equal(t, http.StatusUnauthorized, env.profileStatus(t, first), "the other session is ended")
	assert.Equal(t, http.StatusOK, env.profileStatus(t, second))

	changes := auditEvents(t, env.db, audit.ActionUserPasswordChange)
	require.Len(t, changes, 2)
	assert.Equal(t, audit.Failure, changes[0].Outcome)
	assert.Equal(t, audit.Success, changes[1].Outcome)

	// Only the new password signs in.
	challenge := env.challengeOf(t, contact)
	status, _ = env.secondStep(t, challenge, testPassword)
	assert.Equal(t, http.StatusUnauthorized, status)
	status, resp = env.secondStep(t, challenge, next)
	assert.Equal(t, http.StatusOK, status, resp.Message)
}

// TestUserPassword_AHashWrittenInSQLDoesNotSignIn checks the seal of the
// password row (#139): an attacker with database access cannot give an
// account a password they know.
func TestUserPassword_AHashWrittenInSQLDoesNotSignIn(t *testing.T) {
	env := newE2EEnv(t)
	t.Cleanup(rowseal.Use(rowseal.New(env.db, env.handlers.DataKeys, rowseal.ModeEnforce)))
	const victim, attacker = "victim@example.com", "attacker@example.com"
	token, _ := env.signedIn(t, victim)
	env.setPassword(t, token, testPassword)
	other, _ := env.signedIn(t, attacker)
	env.setPassword(t, other, "the attacker's password")

	// Sealed rows sign in.
	status, resp := env.secondStep(t, env.challengeOf(t, victim), testPassword)
	require.Equal(t, http.StatusOK, status, resp.Message)

	// The attacker copies their own hash onto the victim's row.
	_, err := e2eOwnerDB(t).Exec(`UPDATE user_passwords SET password_hash = (
		SELECT p.password_hash FROM user_passwords p JOIN user_contacts c ON c.user_hk = p.user_hk WHERE c.value = $1)
		WHERE user_hk = (SELECT user_hk FROM user_contacts WHERE value = $2)`, attacker, victim)
	require.NoError(t, err)

	status, resp = env.secondStep(t, env.challengeOf(t, victim), "the attacker's password")
	assert.Equal(t, http.StatusForbidden, status, resp.Message)
	assert.Equal(t, "account_unavailable", resp.Code)

	var findings int
	require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM integrity_findings WHERE table_name = 'user_passwords'`).Scan(&findings))
	assert.Equal(t, 1, findings)
}

// countingHasher counts the slow hashes.
type countingHasher struct {
	userauth.Hasher
	verified atomic.Int64
}

func (h *countingHasher) Verify(ctx context.Context, encoded, password string) (bool, bool, error) {
	h.verified.Add(1)
	return h.Hasher.Verify(ctx, encoded, password)
}

// TestUserPassword_NothingIsHashedWithoutAChallenge checks that the slow
// hash cannot be reached by anyone: not without a challenge of a correct
// code, and not while the password is locked (#16, #307).
func TestUserPassword_NothingIsHashedWithoutAChallenge(t *testing.T) {
	env := newE2EEnv(t, func(c *config.Configuration) {
		c.Security.Password.MaxFailures = 2
		c.RateLimits.Auth.LoginAttemptsPerMinute = 1000
	})
	const contact = "hashing@example.com"
	token, _ := env.signedIn(t, contact)
	env.setPassword(t, token, testPassword)

	counter := &countingHasher{Hasher: env.handlers.Public.Passwords.Hasher}
	env.handlers.Public.Passwords.Hasher = counter

	for _, body := range []map[string]string{
		{"challenge": "no-such-challenge", "password": testPassword},
		{"challenge": "", "password": testPassword},
		{"password": testPassword},
		{"challenge": "no-such-challenge", "password": strings.Repeat("x", 1000)},
	} {
		status, _ := env.post(t, secondStepRoute, body, "")
		assert.Contains(t, []int{http.StatusBadRequest, http.StatusUnauthorized}, status)
	}
	assert.Zero(t, counter.verified.Load(), "no hash without a challenge")

	challenge := env.challengeOf(t, contact)
	for range 2 {
		status, _ := env.secondStep(t, challenge, "not the password")
		require.Equal(t, http.StatusUnauthorized, status)
	}
	require.EqualValues(t, 2, counter.verified.Load())
	for range 5 {
		status, _ := env.secondStep(t, challenge, testPassword)
		require.Equal(t, http.StatusTooManyRequests, status)
	}
	assert.EqualValues(t, 2, counter.verified.Load(), "no hash while the password is locked")
}
