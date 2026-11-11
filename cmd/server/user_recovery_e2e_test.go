package main

import (
	"net/http"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	recoveryRoute         = "/api/v1/public/auth/recovery"
	recoveryCompleteRoute = "/api/v1/public/auth/recovery/complete"
)

func (e *e2eEnv) recoverWith(t *testing.T, challenge, nationalID string) (int, e2eResponse) {
	t.Helper()
	body := map[string]string{"challenge": challenge}
	if nationalID != "" {
		body["national_id"] = nationalID
	}
	return e.post(t, recoveryRoute, body, "")
}

func (e *e2eEnv) completeRecovery(t *testing.T, challenge, password string) (int, e2eResponse) {
	t.Helper()
	return e.post(t, recoveryCompleteRoute, map[string]string{"challenge": challenge, "new_password": password}, "")
}

// endRecoveryWait moves the open recovery to the end of its wait.
func (e *e2eEnv) endRecoveryWait(t *testing.T) {
	t.Helper()
	res, err := e.db.Exec(`UPDATE account_recoveries SET ready_at = NOW() - INTERVAL '1 second' WHERE status = 'waiting'`)
	require.NoError(t, err)
	n, _ := res.RowsAffected()
	require.EqualValues(t, 1, n)
}

func loginLimit(c *config.Configuration) { c.RateLimits.Auth.LoginAttemptsPerMinute = 1000 }

// TestAccountRecovery_ProvesEveryContactAndWaits recovers an account whose
// password and recovery codes are lost (#307): every verified contact is
// proved, and the new password is set only after the wait.
func TestAccountRecovery_ProvesEveryContactAndWaits(t *testing.T) {
	env := newE2EEnv(t, loginLimit)
	const email, number = "lost@example.com", "09121110031"
	const next = "a new and better password"
	token, _ := env.signedIn(t, email)
	id, accountCode, newCode := env.startChange(t, token, map[string]string{"action": "add", "value": number}, email, number)
	status, resp := env.confirmChange(t, token, id, accountCode, newCode)
	require.Equal(t, http.StatusOK, status, resp.Message)
	env.setPassword(t, token, testPassword)

	status, resp = env.recoverWith(t, "no-such-challenge", "")
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, "challenge_invalid", resp.Code)

	// One contact of two is not enough.
	byEmail := env.challengeOf(t, email)
	status, resp = env.recoverWith(t, byEmail, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Equal(t, "standard", resp.Info["mode"])
	assert.Equal(t, "verifying", resp.Info["status"])
	assert.EqualValues(t, 2, resp.Info["contacts_required"])
	assert.EqualValues(t, 1, resp.Info["contacts_proven"])
	status, resp = env.recoverWith(t, byEmail, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.EqualValues(t, 1, resp.Info["contacts_proven"], "a contact counts once")
	status, resp = env.completeRecovery(t, byEmail, next)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "recovery_not_ready", resp.Code)

	// The second contact starts the wait.
	byMobile := env.challengeOf(t, number)
	status, resp = env.recoverWith(t, byMobile, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Equal(t, "waiting", resp.Info["status"])
	assert.NotEmpty(t, resp.Info["ready_at"])
	status, resp = env.completeRecovery(t, byMobile, next)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "recovery_not_ready", resp.Code)
	assert.NotEmpty(t, resp.Info["ready_at"])
	var hours float64
	require.NoError(t, env.db.QueryRow(`SELECT EXTRACT(EPOCH FROM ready_at - NOW()) / 3600 FROM account_recoveries`).Scan(&hours))
	assert.InDelta(t, config.DefaultRecoveryWaitHours, hours, 0.1)

	// The owner sees the recovery at a sign-in and in the settings.
	status, resp = env.signInUser(t, email)
	require.Equal(t, http.StatusAccepted, status)
	recovery, _ := resp.Info["recovery"].(map[string]interface{})
	require.NotNil(t, recovery)
	assert.Equal(t, "waiting", recovery["status"])
	status, body := env.sessionCall(t, http.MethodGet, "/api/v1/user/security", token)
	require.Equal(t, http.StatusOK, status, body)
	assert.NotNil(t, body["info"].(map[string]interface{})["recovery"])

	env.endRecoveryWait(t)
	status, resp = env.completeRecovery(t, byMobile, "short")
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, resp.Info["errors"], "new_password")
	status, resp = env.completeRecovery(t, byMobile, next)
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Len(t, resp.Info["recovery_codes"], 10)
	assert.NotEmpty(t, resp.Info["hold_until"])
	assert.Equal(t, http.StatusUnauthorized, env.profileStatus(t, token), "every session is ended")

	// The recovery is spent, and only the new password signs in.
	status, resp = env.completeRecovery(t, byEmail, "yet another password")
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, "recovery_not_found", resp.Code)
	challenge := env.challengeOf(t, email)
	status, _ = env.secondStep(t, challenge, testPassword)
	assert.Equal(t, http.StatusUnauthorized, status)
	status, resp = env.secondStep(t, challenge, next)
	require.Equal(t, http.StatusOK, status, resp.Message)

	starts := auditEvents(t, env.db, audit.ActionUserRecoveryStart)
	require.Len(t, starts, 1)
	assert.Equal(t, audit.TargetAccountRecovery, starts[0].TargetType.String)
	assert.Len(t, auditEvents(t, env.db, audit.ActionUserRecoveryComplete), 1)
	assert.Empty(t, auditEvents(t, env.db, audit.ActionUserRecoveryCancel))
}

// TestAccountRecovery_ThePasswordCancelsIt checks that the owner of an
// account stops a recovery that someone else started by signing in.
func TestAccountRecovery_ThePasswordCancelsIt(t *testing.T) {
	env := newE2EEnv(t, loginLimit)
	const email = "owner@example.com"
	token, _ := env.signedIn(t, email)
	codes := env.withRecoveryCodes(t, token)

	intruder := env.challengeOf(t, email)
	status, resp := env.recoverWith(t, intruder, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Equal(t, "waiting", resp.Info["status"], "the only contact is proved")

	// The owner signs in with the password.
	status, resp = env.secondStep(t, env.challengeOf(t, email), testPassword)
	require.Equal(t, http.StatusOK, status, resp.Message)
	var state, reason string
	require.NoError(t, env.db.QueryRow(`SELECT status, end_reason FROM account_recoveries`).Scan(&state, &reason))
	assert.Equal(t, "cancelled", state)
	assert.Equal(t, "password_proven", reason)
	status, resp = env.completeRecovery(t, intruder, "the password of the intruder")
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, "recovery_not_found", resp.Code)
	cancels := auditEvents(t, env.db, audit.ActionUserRecoveryCancel)
	require.Len(t, cancels, 1)
	assert.Equal(t, "password_proven", cancels[0].Reason.String)

	// A reset with a recovery code cancels one too.
	status, resp = env.recoverWith(t, env.challengeOf(t, email), "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	status, resp = env.resetPassword(t, env.challengeOf(t, email), codes[0], "a new and better password")
	require.Equal(t, http.StatusOK, status, resp.Message)
	var open int
	require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM account_recoveries WHERE status IN ('verifying', 'waiting')`).Scan(&open))
	assert.Zero(t, open)
}

// TestAccountRecovery_StrictChecksTheIdentityAgain runs the strict mode:
// the owner proves the national ID again, and the provider confirms that
// the mobile number still belongs to it.
func TestAccountRecovery_StrictChecksTheIdentityAgain(t *testing.T) {
	env := newE2EEnv(t, loginLimit, func(c *config.Configuration) {
		c.Security.Recovery.Mode = config.RecoveryStrict
		c.Identity.MobileOwnership.RecheckDays = 30
	})
	const number, nationalID, other = "09121110032", "0012345679", "9876543210"
	token, _ := env.signedIn(t, number)
	stub := env.withNationalID(t, userOf(t, token), nationalID)
	stub.SetOwner(number, nationalID)
	env.setPassword(t, token, testPassword)

	// The contact alone leaves the national ID to prove.
	challenge := env.challengeOf(t, number)
	status, resp := env.recoverWith(t, challenge, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Equal(t, "strict", resp.Info["mode"])
	assert.Equal(t, "verifying", resp.Info["status"])
	assert.Equal(t, []interface{}{"national_id"}, resp.Info["steps_remaining"])

	status, resp = env.recoverWith(t, challenge, "12345")
	assert.Equal(t, http.StatusBadRequest, status)
	status, resp = env.recoverWith(t, challenge, other)
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, "recovery_identity_mismatch", resp.Code)
	assert.EqualValues(t, 2, resp.Info["identity_attempts_left"])

	// A provider that cannot answer decides nothing.
	stub.SetUnavailable(true)
	status, resp = env.recoverWith(t, challenge, nationalID)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "recovery_unavailable", resp.Code)
	stub.SetUnavailable(false)

	status, resp = env.recoverWith(t, challenge, nationalID)
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Equal(t, "waiting", resp.Info["status"])
	assert.Empty(t, resp.Info["steps_remaining"])
	failures := auditEvents(t, env.db, audit.ActionUserRecoveryIdentity)
	require.Len(t, failures, 1)
	assert.Equal(t, audit.Failure, failures[0].Outcome)
}

// TestAccountRecovery_FailedIdentityChecksEndIt checks that the identity of
// a strict recovery cannot be guessed: the third failure ends the recovery,
// and the account starts no other for a while.
func TestAccountRecovery_FailedIdentityChecksEndIt(t *testing.T) {
	env := newE2EEnv(t, loginLimit, func(c *config.Configuration) {
		c.Security.Recovery.Mode = config.RecoveryStrict
		c.Identity.MobileOwnership.RecheckDays = 30
	})
	const number, nationalID, other = "09121110033", "0012345679", "9876543210"
	token, _ := env.signedIn(t, number)
	stub := env.withNationalID(t, userOf(t, token), nationalID)
	stub.SetOwner(number, nationalID)
	env.setPassword(t, token, testPassword)
	challenge := env.challengeOf(t, number)

	// The number was given to someone else: the right national ID does
	// not help its new holder.
	stub.SetOwner(number, other)
	for left := 2; left >= 1; left-- {
		status, resp := env.recoverWith(t, challenge, nationalID)
		require.Equal(t, http.StatusUnauthorized, status, resp.Message)
		assert.Equal(t, "recovery_identity_mismatch", resp.Code)
		assert.EqualValues(t, left, resp.Info["identity_attempts_left"])
	}
	status, resp := env.recoverWith(t, challenge, nationalID)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "recovery_failed", resp.Code)
	stub.SetOwner(number, nationalID)
	status, resp = env.recoverWith(t, challenge, nationalID)
	assert.Equal(t, http.StatusTooManyRequests, status)
	assert.Equal(t, "recovery_cooldown", resp.Code)
	var state, reason string
	require.NoError(t, env.db.QueryRow(`SELECT status, end_reason FROM account_recoveries`).Scan(&state, &reason))
	assert.Equal(t, "failed", state)
	assert.Equal(t, "identity_failed", reason)
}

// TestAccountRecovery_StrictWithoutANationalIDIsStandard checks that an
// account that never verified an identity has none to prove again.
func TestAccountRecovery_StrictWithoutANationalIDIsStandard(t *testing.T) {
	env := newE2EEnv(t, loginLimit, func(c *config.Configuration) { c.Security.Recovery.Mode = config.RecoveryStrict })
	const email = "level0@example.com"
	token, _ := env.signedIn(t, email)
	env.setPassword(t, token, testPassword)
	status, resp := env.recoverWith(t, env.challengeOf(t, email), "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Equal(t, "standard", resp.Info["mode"])
	assert.Equal(t, "waiting", resp.Info["status"])
}
