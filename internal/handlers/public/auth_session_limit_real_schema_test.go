package public

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The session limit of #212, on a database built by the real migrations.

// requestCode asks for a sign-in code and returns the sign-in id and code.
func (e *sessionTestEnv) requestCode(t *testing.T, contact string) (string, string) {
	t.Helper()
	w := e.send(http.MethodPost, "/public/auth", map[string]string{"username": contact})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp models.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.Info.(map[string]interface{})["uuid"].(string), lastSentCode(t, e.sender)
}

type limitAnswer struct {
	Code string `json:"code"`
	Info struct {
		Challenge   string `json:"challenge"`
		ExpiresIn   int    `json:"expires_in"`
		MaxSessions int    `json:"max_sessions"`
		Sessions    []struct {
			ID string `json:"id"`
		} `json:"sessions"`
		AccessToken string `json:"access_token"`
	} `json:"info"`
}

func decodeLimit(t *testing.T, w *httptest.ResponseRecorder) limitAnswer {
	t.Helper()
	var a limitAnswer
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &a), w.Body.String())
	return a
}

func (e *sessionTestEnv) activeSessions(t *testing.T, email string) int {
	t.Helper()
	var n int
	require.NoError(t, e.conn.QueryRow(`
		SELECT count(*) FROM sessions s JOIN user_contacts c ON c.user_hk = s.user_id
		WHERE c.type = 'email' AND c.value = $1 AND s.status = 'ACTIVE' AND s.expires_at > NOW()`, email).Scan(&n))
	return n
}

func TestSessionLimit_RealSchema_RefusesPastTheLimitAndReplaces(t *testing.T) {
	env := newSessionTestEnv(t)
	config.Config.JWT.Sessions.MaxPerUser = 2
	const email = "limit@example.com"
	_, firstCookie, _ := env.signIn(t, email)
	env.signIn(t, email)

	id, code := env.requestCode(t, email)
	w := env.send(http.MethodPost, "/public/auth/"+id, map[string]string{"otp": code})
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	refusal := decodeLimit(t, w)
	assert.Equal(t, codeSessionLimit, refusal.Code)
	assert.Equal(t, 2, refusal.Info.MaxSessions)
	assert.Equal(t, 300, refusal.Info.ExpiresIn)
	require.Len(t, refusal.Info.Sessions, 2)
	require.NotEmpty(t, refusal.Info.Challenge)
	assert.Nil(t, findRefreshCookie(w), "no session past the limit")
	assert.Equal(t, 2, env.activeSessions(t, email))

	// A session that is not the user's leaves the challenge usable.
	w = env.send(http.MethodPost, "/public/auth/sessions/replace", map[string]string{
		"challenge": refusal.Info.Challenge, "session_id": "00000000-0000-4000-8000-000000000000"})
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Equal(t, "session_not_found", decodeLimit(t, w).Code)

	// The oldest session is the last one listed: the first sign-in.
	oldest := refusal.Info.Sessions[1].ID
	w = env.send(http.MethodPost, "/public/auth/sessions/replace", map[string]string{
		"challenge": refusal.Info.Challenge, "session_id": oldest})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotEmpty(t, decodeLimit(t, w).Info.AccessToken)
	assert.NotNil(t, findRefreshCookie(w))
	assert.Equal(t, 2, env.activeSessions(t, email))

	// The ended session can no longer refresh, and the challenge works once.
	assert.Equal(t, http.StatusUnauthorized, env.send(http.MethodPost, "/public/auth/refresh", nil, withRefreshCookie(firstCookie.Value)).Code)
	w = env.send(http.MethodPost, "/public/auth/sessions/replace", map[string]string{
		"challenge": refusal.Info.Challenge, "session_id": refusal.Info.Sessions[0].ID})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, "challenge_invalid", decodeLimit(t, w).Code)

	var reason string
	require.NoError(t, env.conn.QueryRow(`SELECT reason FROM audit_events
		WHERE action = $1 AND target_id = $2`, audit.ActionUserSessionRevoke, oldest).Scan(&reason))
	assert.Equal(t, "replaced_at_sign_in", reason)
	var refused int
	require.NoError(t, env.conn.QueryRow(`SELECT count(*) FROM audit_events
		WHERE action = $1 AND outcome = 'denied' AND reason = $2`, audit.ActionUserSignIn, codeSessionLimit).Scan(&refused))
	assert.Equal(t, 1, refused)
}

func TestSessionLimit_RealSchema_EndedSessionsFreeASlot(t *testing.T) {
	env := newSessionTestEnv(t)
	config.Config.JWT.Sessions.MaxPerUser = 1
	const email = "limit-logout@example.com"
	token, _, _ := env.signIn(t, email)
	require.Equal(t, http.StatusOK, env.send(http.MethodPost, "/public/auth/logout", nil, withAuthorization(token)).Code)
	env.signIn(t, email)
}

func TestSessionLimit_RealSchema_InvalidRequests(t *testing.T) {
	env := newSessionTestEnv(t)
	for name, body := range map[string]map[string]string{
		"no challenge":     {"session_id": "00000000-0000-4000-8000-000000000000"},
		"a bad session id": {"challenge": "abc", "session_id": "nope"},
	} {
		w := env.send(http.MethodPost, "/public/auth/sessions/replace", body)
		assert.Equal(t, http.StatusBadRequest, w.Code, name)
	}
	w := env.send(http.MethodPost, "/public/auth/sessions/replace", map[string]string{
		"challenge": "never-issued", "session_id": "00000000-0000-4000-8000-000000000000"})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// TestSessionLimit_RealSchema_ConcurrentSignInsStayWithinTheLimit verifies
// two codes at once, as two instances would: the lock of the user's row lets
// one of them take the last slot.
func TestSessionLimit_RealSchema_ConcurrentSignInsStayWithinTheLimit(t *testing.T) {
	env := newSessionTestEnv(t)
	config.Config.JWT.Sessions.MaxPerUser = 2
	const email = "limit-race@example.com"
	env.signIn(t, email)
	firstID, firstCode := env.requestCode(t, email)
	secondID, secondCode := env.requestCode(t, email)

	statuses := make([]int, 2)
	var wg sync.WaitGroup
	for i, attempt := range [][2]string{{firstID, firstCode}, {secondID, secondCode}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = env.send(http.MethodPost, "/public/auth/"+attempt[0], map[string]string{"otp": attempt[1]}).Code
		}()
	}
	wg.Wait()
	assert.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, statuses)
	assert.Equal(t, 2, env.activeSessions(t, email))
}
