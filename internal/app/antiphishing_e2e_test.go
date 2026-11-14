package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/antiphishing"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/confirm"
	"github.com/mdastpak/crypto-exchange/internal/crypto/field"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lastAntiPhishing returns the anti-phishing code that the last message to
// contact showed.
func (s *capturingSender) lastAntiPhishing(contact string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.antiPhishing[contact]
}

// TestUserAntiPhishingCode walks through #375: a user sets a code, with a
// fresh proof of the second factor; the codes of sign-ins, of contact
// changes, and of confirmations to their verified email show it, an
// address that a change adds never does, and the settings show its start
// and how many emails showed it.
func TestUserAntiPhishingCode(t *testing.T) {
	env := newE2EEnv(t)
	// The server encrypts the fields with its data keys once it runs
	// (useFieldEncryption); the environment builds the handlers only.
	require.NotNil(t, env.handlers.DataKeys)
	t.Cleanup(field.Use(field.New(env.handlers.DataKeys)))
	ctx := context.Background()
	const email = "phish@example.com"
	const route = "/api/v1/user/anti-phishing-code"
	token, userID := env.signedInUser(t, email)
	status := func() antiphishing.Status {
		t.Helper()
		code, raw := env.call(t, http.MethodGet, route, nil, token)
		require.Equal(t, http.StatusOK, code, string(raw))
		var body struct {
			Info antiphishing.Status `json:"info"`
		}
		require.NoError(t, json.Unmarshal(raw, &body))
		return body.Info
	}
	assert.False(t, status().Set)

	// A user without a second factor sets it like the other security
	// settings; once they have one, a change needs a fresh proof of it.
	code, resp := env.put(t, route, map[string]string{"code": "red river"}, token)
	require.Equal(t, http.StatusOK, code, resp.Message)
	env.setPassword(t, token, testPassword)
	code, resp = env.put(t, route, map[string]string{"code": "blue falcon"}, token)
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, "step_up_required", resp.Code)
	code, _ = env.post(t, stepUpRoute, map[string]string{"password": testPassword}, token)
	require.Equal(t, http.StatusOK, code)
	code, resp = env.put(t, route, map[string]string{"code": "<script>"}, token)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "anti_phishing_code_invalid", resp.Code)
	code, resp = env.put(t, route, map[string]string{"code": "blue falcon"}, token)
	require.Equal(t, http.StatusOK, code, resp.Message)
	assert.Equal(t, "bl***", resp.Info["hint"], "the API shows only the start of the code")
	code, resp = env.put(t, route, map[string]string{"code": "blue  falcon"}, token)
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "anti_phishing_code_same", resp.Code)

	// A sign-in code to the verified email shows it.
	code, resp = env.post(t, "/api/v1/public/auth", map[string]string{"username": email}, "")
	require.Equal(t, http.StatusOK, code, resp.Message)
	assert.Equal(t, "blue falcon", env.sender.lastAntiPhishing(email))

	// A contact change: the account's email shows it, the new one never.
	const newEmail = "phish-new@example.com"
	env.startChange(t, token, map[string]string{"action": "replace", "value": newEmail}, email, newEmail)
	assert.Equal(t, "blue falcon", env.sender.lastAntiPhishing(email))
	assert.Empty(t, env.sender.lastAntiPhishing(newEmail), "a hijacked session must not learn the code")

	// A confirmation code by email shows it.
	service, ok := env.handlers.User.Confirmations.(confirm.Service)
	require.True(t, ok)
	_, err := service.Start(ctx, userID, confirm.PurposeWithdrawal, confirm.Digest(confirm.PurposeWithdrawal, "1", "USDT", "d"),
		confirm.Details{Amount: "1 USDT", Destination: "T...0000"}, "client")
	require.NoError(t, err)
	assert.Equal(t, "blue falcon", env.sender.lastAntiPhishing(email))

	st := status()
	assert.True(t, st.Set)
	assert.Equal(t, int64(3), st.ShownCount, "the sign-in, the contact change, and the confirmation")
	assert.False(t, st.ChangeSuggested)

	code, body := env.sessionCall(t, http.MethodDelete, route, token)
	require.Equal(t, http.StatusOK, code, body)
	code, body = env.sessionCall(t, http.MethodDelete, route, token)
	assert.Equal(t, http.StatusNotFound, code)
	assert.Equal(t, "anti_phishing_code_not_set", body["code"])
	code, resp = env.post(t, "/api/v1/public/auth", map[string]string{"username": email}, "")
	require.Equal(t, http.StatusOK, code, resp.Message)
	assert.Empty(t, env.sender.lastAntiPhishing(email), "a removed code is not shown")

	assert.Len(t, auditEvents(t, env.db, audit.ActionUserAntiPhishingCodeSet), 2)
	assert.Len(t, auditEvents(t, env.db, audit.ActionUserAntiPhishingCodeRemove), 1)
	var leaked int
	require.NoError(t, env.db.QueryRow(`SELECT count(*) FROM audit_events WHERE row_to_json(audit_events)::text LIKE '%falcon%'`).Scan(&leaked))
	assert.Zero(t, leaked, "the audit never holds the code")
}
