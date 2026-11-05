package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/auth"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// userSession holds the tokens of one sign-in session.
type userSession struct {
	userID, sessionID string
	access, refresh   string
}

// send sends a request with an optional JSON body, access token, and refresh
// token cookie, and returns the recorder, so that cookies can be read.
func (e *e2eEnv) send(t *testing.T, method, path string, body interface{}, token, refresh string) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if refresh != "" {
		req.AddCookie(&http.Cookie{Name: auth.RefreshTokenCookie, Value: refresh})
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func refreshTokenCookie(w *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == auth.RefreshTokenCookie {
			return cookie
		}
	}
	return nil
}

func accessTokenOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var resp e2eResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	token, _ := resp.Info["access_token"].(string)
	require.NotEmpty(t, token, w.Body.String())
	return token
}

// startSession signs a user in and returns the tokens of the new session.
func (e *e2eEnv) startSession(t *testing.T, contact string) userSession {
	t.Helper()
	status, resp := e.post(t, "/public/auth", map[string]string{"username": contact}, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	w := e.send(t, http.MethodPost, "/public/auth/"+resp.Info["uuid"].(string), map[string]string{"otp": e.sender.last(contact)}, "", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	cookie := refreshTokenCookie(w)
	require.NotNil(t, cookie)
	session := userSession{access: accessTokenOf(t, w), refresh: cookie.Value}
	claims, err := auth.ParseToken(session.access, auth.RoleUser, auth.TokenTypeAccess)
	require.NoError(t, err)
	session.userID, session.sessionID = claims.UUID, claims.SessionID
	return session
}

// refreshSession exchanges the session's refresh token and returns the
// session with the new tokens.
func (e *e2eEnv) refreshSession(t *testing.T, s userSession) userSession {
	t.Helper()
	w := e.send(t, http.MethodPost, "/public/auth/refresh", nil, "", s.refresh)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	cookie := refreshTokenCookie(w)
	require.NotNil(t, cookie)
	s.access, s.refresh = accessTokenOf(t, w), cookie.Value
	return s
}

// profileStatus returns the status of GET /user/profile with token.
func (e *e2eEnv) profileStatus(t *testing.T, token string) int {
	t.Helper()
	return e.send(t, http.MethodGet, "/user/profile", nil, token, "").Code
}

func (e *e2eEnv) sessionStatus(t *testing.T, sessionID string) string {
	t.Helper()
	var status string
	require.NoError(t, e.db.QueryRow(`SELECT status FROM sessions WHERE id = $1`, sessionID).Scan(&status))
	return status
}

// TestUserSessions_EndToEnd checks that ending a session or disabling its
// user rejects every token of the session on the next request, not when the
// tokens expire (#120).
func TestUserSessions_EndToEnd(t *testing.T) {
	env := newE2EEnv(t)

	t.Run("logout rejects every access token of the session", func(t *testing.T) {
		first := env.startSession(t, "logout@example.com")
		other := env.startSession(t, "logout@example.com")
		second := env.refreshSession(t, first)
		require.Equal(t, http.StatusOK, env.profileStatus(t, first.access), "the access token issued before the refresh is still valid")
		require.Equal(t, http.StatusOK, env.profileStatus(t, second.access))

		w := env.send(t, http.MethodPost, "/user/logout", nil, second.access, "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		w = env.send(t, http.MethodGet, "/user/profile", nil, first.access, "")
		assert.Equal(t, http.StatusUnauthorized, w.Code, "an earlier access token of the session is rejected")
		assert.Contains(t, w.Body.String(), "Session is not active")
		assert.Equal(t, http.StatusUnauthorized, env.profileStatus(t, second.access))
		assert.Equal(t, http.StatusUnauthorized, env.send(t, http.MethodPost, "/public/auth/refresh", nil, "", second.refresh).Code)
		assert.Equal(t, http.StatusOK, env.profileStatus(t, other.access), "another session of the same user is not affected")
	})

	t.Run("refresh token reuse rejects the access tokens of the session", func(t *testing.T) {
		first := env.startSession(t, "reuse@example.com")
		second := env.refreshSession(t, first)

		// Replaying the rotated refresh token is treated as theft.
		w := env.send(t, http.MethodPost, "/public/auth/refresh", nil, "", first.refresh)
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		require.Equal(t, "REVOKED", env.sessionStatus(t, first.sessionID))

		assert.Equal(t, http.StatusUnauthorized, env.profileStatus(t, first.access), "the attacker's access token is rejected")
		assert.Equal(t, http.StatusUnauthorized, env.profileStatus(t, second.access))
		assert.Equal(t, http.StatusUnauthorized, env.send(t, http.MethodPost, "/public/auth/refresh", nil, "", second.refresh).Code)
	})

	for name, change := range map[string]string{
		"disabled":  `UPDATE users SET status = 'INACTIVE' WHERE hk = $1`,
		"suspended": `UPDATE users SET status = 'SUSPENDED' WHERE hk = $1`,
		"banned":    `UPDATE users SET status = 'BANNED' WHERE hk = $1`,
		"deleted":   `UPDATE users SET deleted_at = NOW() WHERE hk = $1`,
	} {
		t.Run("a "+name+" user is rejected on the next request", func(t *testing.T) {
			session := env.startSession(t, name+"@example.com")
			require.Equal(t, http.StatusOK, env.profileStatus(t, session.access))

			_, err := env.db.Exec(change, session.userID)
			require.NoError(t, err)

			for _, method := range []string{http.MethodGet, http.MethodPatch} {
				w := env.send(t, method, "/user/profile", map[string]string{"first_name": "Changed"}, session.access, "")
				assert.Equal(t, http.StatusForbidden, w.Code, method)
				assert.Contains(t, w.Body.String(), "This account is not available. Please contact support.")
			}
			w := env.send(t, http.MethodPost, "/public/auth/refresh", nil, "", session.refresh)
			assert.Equal(t, http.StatusForbidden, w.Code, "no new tokens for a user who can no longer sign in")
			assert.NotContains(t, w.Body.String(), "access_token")
			if cookie := refreshTokenCookie(w); assert.NotNil(t, cookie) {
				assert.Negative(t, cookie.MaxAge, "the refresh token cookie is cleared")
			}

			// The user can still end the session, and it stays ended when
			// the account is restored.
			w = env.send(t, http.MethodPost, "/user/logout", nil, session.access, "")
			assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, "REVOKED", env.sessionStatus(t, session.sessionID))
			_, err = env.db.Exec(`UPDATE users SET status = 'ACTIVE', deleted_at = NULL WHERE hk = $1`, session.userID)
			require.NoError(t, err)
			assert.Equal(t, http.StatusUnauthorized, env.profileStatus(t, session.access))
		})
	}
}
