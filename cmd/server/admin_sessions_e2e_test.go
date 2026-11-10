package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/adminauth"
	"github.com/mdastpak/crypto-exchange/internal/audit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signInAdminAgain signs an administrator in once more within the time step
// of their last authenticator code, which the server accepts only once.
func (e *e2eEnv) signInAdminAgain(t *testing.T, enrollment adminauth.Enrollment) string {
	t.Helper()
	_, err := e.db.Exec(`UPDATE users SET totp_last_step = NULL WHERE hk = $1`, enrollment.AdminID)
	require.NoError(t, err)
	return e.signInAdmin(t, enrollment)
}

type adminSession struct {
	ID        string `json:"id"`
	IP        string `json:"ip"`
	UserAgent string `json:"user_agent"`
	Current   bool   `json:"current"`
}

func (e *e2eEnv) adminSessions(t *testing.T, token string) []adminSession {
	t.Helper()
	w := e.send(t, http.MethodGet, "/api/v1/admin/sessions", nil, token, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body struct {
		Info []adminSession `json:"info"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	return body.Info
}

// TestAdminSessions_EndToEnd signs an administrator in on several devices,
// and lists and ends the sessions through the routes (#308).
func TestAdminSessions_EndToEnd(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	enrollment, err := env.accounts.Create(ctx, "sessions@example.com")
	require.NoError(t, err)
	listCurrencies := func(token string) int {
		return env.send(t, http.MethodGet, "/api/v1/admin/currencies", nil, token, "").Code
	}

	first := env.signInAdmin(t, enrollment)
	second := env.signInAdminAgain(t, enrollment)

	sessions := env.adminSessions(t, second)
	require.Len(t, sessions, 2)
	assert.True(t, sessions[0].Current, "the newest session is the one that asks")
	assert.False(t, sessions[1].Current)
	assert.NotEmpty(t, sessions[1].IP)

	t.Run("ending another session rejects its token at once", func(t *testing.T) {
		w := env.send(t, http.MethodDelete, "/api/v1/admin/sessions/"+sessions[1].ID, nil, second, "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, http.StatusUnauthorized, listCurrencies(first))
		assert.Equal(t, http.StatusOK, listCurrencies(second))
		require.Len(t, env.adminSessions(t, second), 1)

		w = env.send(t, http.MethodDelete, "/api/v1/admin/sessions/"+sessions[1].ID, nil, second, "")
		assert.Equal(t, http.StatusNotFound, w.Code, "an ended session is gone")
		revokes := auditEvents(t, env.db, audit.ActionAdminSessionRevoke)
		require.Len(t, revokes, 1)
		assert.Equal(t, "admin_session/"+sessions[1].ID, revokes[0].TargetType.String+"/"+revokes[0].TargetID.String)
	})

	t.Run("an administrator cannot end the session of another", func(t *testing.T) {
		other, err := env.accounts.Create(ctx, "other-sessions@example.com")
		require.NoError(t, err)
		otherToken := env.signInAdmin(t, other)
		w := env.send(t, http.MethodDelete, "/api/v1/admin/sessions/"+sessions[0].ID, nil, otherToken, "")
		assert.Equal(t, http.StatusNotFound, w.Code)
		assert.Equal(t, http.StatusOK, listCurrencies(second))
	})

	t.Run("a sign-in past the cap ends the oldest session", func(t *testing.T) {
		third := env.signInAdminAgain(t, enrollment)
		fourth := env.signInAdminAgain(t, enrollment)
		assert.Equal(t, http.StatusUnauthorized, listCurrencies(second))
		assert.Equal(t, http.StatusOK, listCurrencies(third))
		assert.Equal(t, http.StatusOK, listCurrencies(fourth))
	})

	t.Run("a logout holds without the blacklist of Redis", func(t *testing.T) {
		token := env.signInAdminAgain(t, enrollment)
		status, resp := env.post(t, "/api/v1/admin/logout", nil, token)
		require.Equal(t, http.StatusOK, status, resp.Message)

		// What an eviction or a lost Redis would do (#305).
		require.NoError(t, env.handlers.TokenBlacklist.RedisClient.FlushDB(ctx).Err())
		w := env.send(t, http.MethodGet, "/api/v1/admin/currencies", nil, token, "")
		require.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Contains(t, w.Body.String(), "session_inactive")
	})

	t.Run("the operator signs an administrator out everywhere", func(t *testing.T) {
		token := env.signInAdminAgain(t, enrollment)
		ended, err := env.accounts.EndSessions(ctx, enrollment.Email)
		require.NoError(t, err)
		assert.EqualValues(t, 2, ended)
		assert.Equal(t, http.StatusUnauthorized, listCurrencies(token))
	})
}
