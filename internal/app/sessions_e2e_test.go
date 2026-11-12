package app

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/auth"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sessionCall sends a request without a body, with an access token, and returns
// the status and the envelope.
func (e *e2eEnv) sessionCall(t *testing.T, method, path, token string) (int, map[string]interface{}) {
	t.Helper()
	status, raw := e.call(t, method, path, nil, token)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &body), string(raw))
	return status, body
}

// signedIn signs a user in and returns the access token and its session.
func (e *e2eEnv) signedIn(t *testing.T, contact string) (token, session string) {
	t.Helper()
	status, resp := e.signInUser(t, contact)
	require.Equal(t, http.StatusOK, status, resp.Message)
	token = resp.Info["access_token"].(string)
	claims, err := auth.ParseToken(token, auth.RoleUser, auth.TokenTypeAccess)
	require.NoError(t, err)
	return token, claims.SessionID
}

// listSessions returns the IDs of the sessions that the list shows, and the
// ID of the one marked current.
func (e *e2eEnv) listSessions(t *testing.T, token, query string) (ids []string, current string, next string) {
	t.Helper()
	status, body := e.sessionCall(t, http.MethodGet, "/api/v1/user/sessions"+query, token)
	require.Equal(t, http.StatusOK, status, body)
	info := body["info"].(map[string]interface{})
	for _, item := range info["items"].([]interface{}) {
		s := item.(map[string]interface{})
		ids = append(ids, s["id"].(string))
		if s["current"] == true {
			current = s["id"].(string)
		}
		assert.NotEmpty(t, s["last_used_at"])
		assert.NotContains(t, s, "refresh_token_hash")
	}
	next, _ = info["next_cursor"].(string)
	return ids, current, next
}

func TestUserSessions_ListAndRevoke(t *testing.T) {
	env := newE2EEnv(t)
	const contact, stranger = "sessions@example.com", "stranger@example.com"
	first, firstSession := env.signedIn(t, contact)
	second, secondSession := env.signedIn(t, contact)
	third, thirdSession := env.signedIn(t, contact)
	other, otherSession := env.signedIn(t, stranger)

	ids, current, _ := env.listSessions(t, first, "")
	assert.Equal(t, []string{thirdSession, secondSession, firstSession}, ids, "newest first, only the user's own")
	assert.Equal(t, firstSession, current)

	t.Run("pages", func(t *testing.T) {
		page1, _, next := env.listSessions(t, first, "?limit=2")
		require.NotEmpty(t, next)
		page2, _, last := env.listSessions(t, first, "?limit=2&cursor="+next)
		assert.Equal(t, ids, append(page1, page2...))
		assert.Empty(t, last)
	})

	t.Run("a user cannot revoke the session of another", func(t *testing.T) {
		status, _ := env.sessionCall(t, http.MethodDelete, "/api/v1/user/sessions/"+otherSession, first)
		assert.Equal(t, http.StatusNotFound, status)
		status, _ = env.sessionCall(t, http.MethodGet, "/api/v1/user/profile", other)
		assert.Equal(t, http.StatusOK, status, "the other user's session still works")
		status, _ = env.sessionCall(t, http.MethodDelete, "/api/v1/user/sessions/"+uuid.NewString(), first)
		assert.Equal(t, http.StatusNotFound, status)
		status, _ = env.sessionCall(t, http.MethodDelete, "/api/v1/user/sessions/not-a-uuid", first)
		assert.Equal(t, http.StatusBadRequest, status)
	})

	t.Run("a revoked session stops working at once", func(t *testing.T) {
		status, _ := env.sessionCall(t, http.MethodDelete, "/api/v1/user/sessions/"+secondSession, first)
		require.Equal(t, http.StatusOK, status)
		status, body := env.sessionCall(t, http.MethodGet, "/api/v1/user/profile", second)
		assert.Equal(t, http.StatusUnauthorized, status)
		assert.Equal(t, "session_inactive", body["code"])
		status, _ = env.sessionCall(t, http.MethodDelete, "/api/v1/user/sessions/"+secondSession, first)
		assert.Equal(t, http.StatusNotFound, status, "an ended session is not listed or revoked again")
	})

	t.Run("revoke the others", func(t *testing.T) {
		status, body := env.sessionCall(t, http.MethodDelete, "/api/v1/user/sessions", first)
		require.Equal(t, http.StatusOK, status, body)
		assert.Equal(t, float64(1), body["info"].(map[string]interface{})["revoked"])
		status, _ = env.sessionCall(t, http.MethodGet, "/api/v1/user/profile", third)
		assert.Equal(t, http.StatusUnauthorized, status)
		ids, _, _ := env.listSessions(t, first, "")
		assert.Equal(t, []string{firstSession}, ids)
		status, _ = env.sessionCall(t, http.MethodGet, "/api/v1/user/profile", other)
		assert.Equal(t, http.StatusOK, status, "the sessions of other users are untouched")
	})

	t.Run("each revocation is audited", func(t *testing.T) {
		rows, err := env.db.Query(`SELECT target_id FROM audit_events WHERE action = 'user.session_revoke' AND outcome = 'success' ORDER BY occurred_at`)
		require.NoError(t, err)
		defer rows.Close()
		var targets []string
		for rows.Next() {
			var id string
			require.NoError(t, rows.Scan(&id))
			targets = append(targets, id)
		}
		assert.Equal(t, []string{secondSession, thirdSession}, targets)
	})

	t.Run("revoking the current session signs out", func(t *testing.T) {
		status, _ := env.sessionCall(t, http.MethodDelete, "/api/v1/user/sessions/"+firstSession, first)
		require.Equal(t, http.StatusOK, status)
		status, _ = env.sessionCall(t, http.MethodGet, "/api/v1/user/sessions", first)
		assert.Equal(t, http.StatusUnauthorized, status)
	})
}
