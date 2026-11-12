package app

import (
	"context"
	"net/http"
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
	stepUpRoute  = "/api/v1/user/step-up"
	changesRoute = "/api/v1/user/contacts/changes"
)

// refusalCode returns the code of the guard's refusal, or "" when the
// operation is allowed.
func refusalCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var refusal *userauth.Refusal
	require.ErrorAs(t, err, &refusal)
	return refusal.Code
}

// sessionOf returns the user and the session of an access token.
func sessionOf(t *testing.T, token string) (userHK, sessionID string) {
	t.Helper()
	claims, err := auth.ParseToken(token, auth.RoleUser, auth.TokenTypeAccess)
	require.NoError(t, err)
	return claims.UUID, claims.SessionID
}

// TestSecondFactor_FundsNeedAPasswordAndAFreshProof follows a user from no
// password to an elevated session (#307). There are no routes of funds yet,
// so it asks the guard that they will call.
func TestSecondFactor_FundsNeedAPasswordAndAFreshProof(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	guard := env.handlers.User.Guard
	const contact = "funds@example.com"
	token, _ := env.signedIn(t, contact)
	user, session := sessionOf(t, token)
	newMobile := map[string]string{"action": "add", "value": "+98 912 111 0001"}

	// Without a password: funds are refused until the user sets one, and
	// the other protected operations are open.
	assert.Equal(t, userauth.CodeSecondFactorRequired, refusalCode(t, guard.Check(ctx, env.db, user, session, true)))
	assert.Empty(t, refusalCode(t, guard.Check(ctx, env.db, user, session, false)))
	status, resp := env.post(t, stepUpRoute, map[string]string{"password": testPassword}, token)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "second_factor_not_set", resp.Code)

	// Setting the password proves nothing with it: the session is not
	// elevated, or a stolen session could set one and move funds.
	env.setPassword(t, token, testPassword)
	assert.Equal(t, userauth.CodeStepUpRequired, refusalCode(t, guard.Check(ctx, env.db, user, session, true)))
	status, resp = env.post(t, changesRoute, newMobile, token)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, userauth.CodeStepUpRequired, resp.Code)

	status, resp = env.post(t, stepUpRoute, map[string]string{"password": "not the password"}, token)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "password_invalid", resp.Code)
	assert.Equal(t, userauth.CodeStepUpRequired, refusalCode(t, guard.Check(ctx, env.db, user, session, true)))

	status, resp = env.post(t, stepUpRoute, map[string]string{"password": testPassword}, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.NotEmpty(t, resp.Info["elevated_until"])
	assert.Empty(t, refusalCode(t, guard.Check(ctx, env.db, user, session, true)))
	status, resp = env.post(t, changesRoute, newMobile, token)
	assert.Equal(t, http.StatusOK, status, resp.Message)

	status, body := env.sessionCall(t, http.MethodGet, "/api/v1/user/security", token)
	require.Equal(t, http.StatusOK, status, body)
	factor := body["info"].(map[string]interface{})["second_factor"].(map[string]interface{})
	assert.Equal(t, config.SecondFactorRequiredForFunds, factor["mode"])
	assert.Equal(t, true, factor["enrolled"])
	assert.NotEmpty(t, factor["elevated_until"])

	// The elevation ends on its own.
	_, err := e2eOwnerDB(t).Exec(`UPDATE session_step_ups SET elevated_until = NOW() - INTERVAL '1 second'`)
	require.NoError(t, err)
	assert.Equal(t, userauth.CodeStepUpRequired, refusalCode(t, guard.Check(ctx, env.db, user, session, true)))
	status, _ = env.post(t, changesRoute, newMobile, token)
	assert.Equal(t, http.StatusForbidden, status)

	steps := auditEvents(t, env.db, audit.ActionUserStepUp)
	require.Len(t, steps, 2)
	assert.Equal(t, audit.Failure, steps[0].Outcome)
	assert.Equal(t, audit.Success, steps[1].Outcome)
	assert.Equal(t, session, steps[1].TargetID.String)
}

// TestSecondFactor_ASignInWithThePasswordElevatesItsSessionOnly checks that
// the proof belongs to one session.
func TestSecondFactor_ASignInWithThePasswordElevatesItsSessionOnly(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	guard := env.handlers.User.Guard
	const contact = "elevated@example.com"
	first, _ := env.signedIn(t, contact)
	env.setPassword(t, first, testPassword)

	status, resp := env.secondStep(t, env.challengeOf(t, contact), testPassword)
	require.Equal(t, http.StatusOK, status, resp.Message)
	second := resp.Info["access_token"].(string)
	user, session := sessionOf(t, second)
	assert.Empty(t, refusalCode(t, guard.Check(ctx, env.db, user, session, true)), "the sign-in proved the password")

	// A third session, of another user, cannot be elevated for this one.
	other, _ := env.signedIn(t, "someone.else@example.com")
	_, otherSession := sessionOf(t, other)
	_, err := guard.Elevate(ctx, user, otherSession)
	assert.Error(t, err)
	otherUser, _ := sessionOf(t, other)
	assert.Equal(t, userauth.CodeSecondFactorRequired, refusalCode(t, guard.Check(ctx, env.db, otherUser, otherSession, true)))

	// An ended session cannot be elevated.
	require.Equal(t, http.StatusOK, env.send(t, http.MethodPost, "/api/v1/user/logout", nil, second, "").Code)
	_, err = guard.Elevate(ctx, user, session)
	assert.Error(t, err)
}

// TestSecondFactor_OptionalModeLeavesFundsOpenWithoutAPassword checks the
// mode optional: no password is asked for, and one that is set still needs
// its proof.
func TestSecondFactor_OptionalModeLeavesFundsOpenWithoutAPassword(t *testing.T) {
	env := newE2EEnv(t, func(c *config.Configuration) { c.Security.SecondFactor.Mode = config.SecondFactorOptional })
	ctx := context.Background()
	guard := env.handlers.User.Guard
	token, _ := env.signedIn(t, "optional@example.com")
	user, session := sessionOf(t, token)

	assert.Empty(t, refusalCode(t, guard.Check(ctx, env.db, user, session, true)))
	env.setPassword(t, token, testPassword)
	assert.Equal(t, userauth.CodeStepUpRequired, refusalCode(t, guard.Check(ctx, env.db, user, session, true)))
}

// TestSecondFactor_AStepUpLengthenedInSQLLetsNothingRun lengthens the
// step-up of a session in SQL (#337). In enforce mode the session is no
// longer elevated, the change is a finding, and proving the factor again
// elevates it, with a new seal.
func TestSecondFactor_AStepUpLengthenedInSQLLetsNothingRun(t *testing.T) {
	env := newE2EEnv(t)
	t.Cleanup(rowseal.Use(rowseal.New(env.db, env.handlers.DataKeys, rowseal.ModeEnforce)))
	ctx := context.Background()
	guard := env.handlers.User.Guard
	token, _ := env.signedIn(t, "lengthened@example.com")
	user, session := sessionOf(t, token)
	env.setPassword(t, token, testPassword)
	status, resp := env.post(t, stepUpRoute, map[string]string{"password": testPassword}, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	require.Empty(t, refusalCode(t, guard.Check(ctx, env.db, user, session, false)), "the step-up is sealed")

	_, err := e2eOwnerDB(t).Exec(`UPDATE session_step_ups SET elevated_until = NOW() + INTERVAL '30 days' WHERE session_id = $1`, session)
	require.NoError(t, err)
	assert.Equal(t, userauth.CodeStepUpRequired, refusalCode(t, guard.Check(ctx, env.db, user, session, false)))
	var findings int
	require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM integrity_findings
		WHERE table_name = 'session_step_ups' AND row_key = $1 AND resolved_at IS NULL`, session).Scan(&findings))
	assert.Equal(t, 1, findings)

	status, resp = env.post(t, stepUpRoute, map[string]string{"password": testPassword}, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Empty(t, refusalCode(t, guard.Check(ctx, env.db, user, session, false)), "a new proof seals a new step-up")
}
