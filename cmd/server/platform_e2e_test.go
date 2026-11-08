package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/platform"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// platformResponse is an answer with the info of a refusal or a control.
type platformResponse struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Info   struct {
		ID      string `json:"id"`
		Kind    string `json:"kind"`
		Feature string `json:"feature"`
		Version int64  `json:"version"`
		EndsAt  string `json:"ends_at"`
		Message struct {
			FA string `json:"fa"`
			EN string `json:"en"`
		} `json:"message"`
		Mode      string   `json:"mode"`
		Lockdowns []string `json:"lockdowns"`
	} `json:"info"`
}

func (e *e2eEnv) platformCall(t *testing.T, method, path string, body interface{}, token string) (int, platformResponse) {
	t.Helper()
	status, raw := e.call(t, method, path, body, token)
	var resp platformResponse
	require.NoError(t, json.Unmarshal(raw, &resp), string(raw))
	return status, resp
}

// createControl creates a control through the admin API.
func (e *e2eEnv) createControl(t *testing.T, token string, body map[string]interface{}) platformResponse {
	t.Helper()
	status, resp := e.platformCall(t, http.MethodPost, "/api/v1/admin/platform/controls", body, token)
	require.Equal(t, http.StatusCreated, status, resp.Code)
	return resp
}

// TestPlatform_FullMaintenance puts the service under maintenance through
// the admin API, and checks what users, allowed users, and administrators
// can do then, and after the lift (#235).
func TestPlatform_FullMaintenance(t *testing.T) {
	env := newE2EEnv(t)
	enrollment, err := env.accounts.Create(context.Background(), "ops@example.com")
	require.NoError(t, err)
	admin := env.signInAdmin(t, enrollment)
	alice, _ := env.signedInUser(t, "alice@example.com")
	tester, testerID := env.signedInUser(t, "tester@example.com")

	ends := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	control := env.createControl(t, admin, map[string]interface{}{
		"kind": "maintenance", "ends_at": ends, "reason": "Database upgrade",
		"message":     map[string]string{"fa": "در حال به\u200cروزرسانی هستیم", "en": "We are upgrading the database"},
		"allow_users": []string{testerID},
	})

	t.Run("public routes answer 503 maintenance with Retry-After", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/public/currencies", nil)
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, req)
		require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		var resp platformResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, platform.CodeMaintenance, resp.Code)
		assert.Equal(t, "We are upgrading the database", resp.Info.Message.EN)
		assert.Equal(t, ends.Format(time.RFC3339), resp.Info.EndsAt)
		retry, err := strconv.Atoi(w.Header().Get("Retry-After"))
		require.NoError(t, err)
		assert.InDelta(t, 30*60, retry, 5)
	})

	t.Run("sign-in and user routes are closed, sessions are kept", func(t *testing.T) {
		status, resp := env.platformCall(t, http.MethodPost, "/api/v1/public/auth", map[string]string{"username": "new@example.com"}, "")
		assert.Equal(t, http.StatusServiceUnavailable, status)
		assert.Equal(t, platform.CodeMaintenance, resp.Code)
		status, resp = env.platformCall(t, http.MethodGet, "/api/v1/user/profile", nil, alice)
		assert.Equal(t, http.StatusServiceUnavailable, status)
		assert.Equal(t, platform.CodeMaintenance, resp.Code)
	})

	t.Run("the status endpoint and admin routes stay open", func(t *testing.T) {
		status, resp := env.platformCall(t, http.MethodGet, "/api/v1/public/status", nil, "")
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, platform.ModeMaintenance, resp.Info.Mode)
		status, _ = env.call(t, http.MethodGet, "/api/v1/admin/platform", nil, admin)
		assert.Equal(t, http.StatusOK, status)
		status, _ = env.call(t, http.MethodGet, "/api/v1/admin/currencies", nil, admin)
		assert.Equal(t, http.StatusOK, status)
	})

	t.Run("an allowed user passes", func(t *testing.T) {
		status, _ := env.call(t, http.MethodGet, "/api/v1/user/profile", nil, tester)
		assert.Equal(t, http.StatusOK, status)
	})

	t.Run("after the lift, everything opens", func(t *testing.T) {
		status, resp := env.platformCall(t, http.MethodPost, "/api/v1/admin/platform/controls/"+control.Info.ID+"/lift",
			map[string]string{"note": "Upgrade done"}, admin)
		require.Equal(t, http.StatusOK, status, resp.Code)
		status, _ = env.call(t, http.MethodGet, "/api/v1/user/profile", nil, alice)
		assert.Equal(t, http.StatusOK, status, "the session survived the maintenance")
		status, _ = env.call(t, http.MethodGet, "/api/v1/public/currencies", nil, "")
		assert.Equal(t, http.StatusOK, status)
		status, resp = env.platformCall(t, http.MethodGet, "/api/v1/public/status", nil, "")
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, platform.ModeOpen, resp.Info.Mode)
	})

	events := auditEvents(t, env.db, "platform.control_create")
	require.Len(t, events, 1)
	assert.Equal(t, enrollment.AdminID, events[0].ActorID.String)
	require.Len(t, auditEvents(t, env.db, "platform.control_lift"), 1)
}

// TestPlatform_Lockdowns closes single features: sign-up refuses only new
// accounts, read-only refuses only changes, and an active lockdown of
// withdrawals needs a second administrator to lift.
func TestPlatform_Lockdowns(t *testing.T) {
	env := newE2EEnv(t)
	first, err := env.accounts.Create(context.Background(), "first@example.com")
	require.NoError(t, err)
	second, err := env.accounts.Create(context.Background(), "second@example.com")
	require.NoError(t, err)
	admin1, admin2 := env.signInAdmin(t, first), env.signInAdmin(t, second)
	_, _ = env.signedInUser(t, "existing@example.com")

	t.Run("a sign-up lockdown refuses new accounts only", func(t *testing.T) {
		control := env.createControl(t, admin1, map[string]interface{}{"kind": "lockdown", "feature": "sign_up", "reason": "Spam wave"})
		status, resp := env.signInUser(t, "newcomer@example.com")
		assert.Equal(t, http.StatusServiceUnavailable, status)
		assert.Equal(t, platform.CodeFeatureLocked, resp.Code)
		assert.Equal(t, "sign_up", resp.Info["feature"])
		status, resp = env.signInUser(t, "existing@example.com")
		assert.Equal(t, http.StatusOK, status, resp.Message)

		status, _ = env.call(t, http.MethodPost, "/api/v1/admin/platform/controls/"+control.Info.ID+"/lift", map[string]string{"note": "Over"}, admin1)
		require.Equal(t, http.StatusOK, status)
		status, resp = env.signInUser(t, "newcomer@example.com")
		assert.Equal(t, http.StatusOK, status, resp.Message)
	})

	t.Run("read-only refuses changes, not reads", func(t *testing.T) {
		user, _ := env.signedInUser(t, "reader@example.com")
		control := env.createControl(t, admin1, map[string]interface{}{"kind": "lockdown", "feature": "read_only", "reason": "Migration"})
		status, resp := env.platformCall(t, http.MethodPatch, "/api/v1/user/profile", map[string]string{"first_name": "Reza"}, user)
		assert.Equal(t, http.StatusServiceUnavailable, status)
		assert.Equal(t, platform.CodeFeatureLocked, resp.Code)
		status, _ = env.call(t, http.MethodGet, "/api/v1/user/profile", nil, user)
		assert.Equal(t, http.StatusOK, status)
		status, _ = env.call(t, http.MethodPost, "/api/v1/admin/platform/controls/"+control.Info.ID+"/lift", map[string]string{"note": "Over"}, admin1)
		require.Equal(t, http.StatusOK, status)
	})

	t.Run("lifting a withdrawal lockdown needs a second administrator", func(t *testing.T) {
		control := env.createControl(t, admin1, map[string]interface{}{"kind": "lockdown", "feature": "withdrawals", "reason": "Hot wallet alert"})
		path := "/api/v1/admin/platform/controls/" + control.Info.ID + "/lift"
		status, resp := env.platformCall(t, http.MethodPost, path, map[string]string{"note": "Wallet checked"}, admin1)
		require.Equal(t, http.StatusAccepted, status, resp.Code)
		status, resp = env.platformCall(t, http.MethodPost, path, map[string]string{"note": "Again"}, admin1)
		assert.Equal(t, http.StatusConflict, status)
		assert.Equal(t, "second_approval_required", resp.Code)
		status, resp = env.platformCall(t, http.MethodGet, "/api/v1/public/status", nil, "")
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, []string{"withdrawals"}, resp.Info.Lockdowns, "the lockdown still applies")

		status, resp = env.platformCall(t, http.MethodPost, path, map[string]string{"note": "Approved"}, admin2)
		require.Equal(t, http.StatusOK, status, resp.Code)
		status, resp = env.platformCall(t, http.MethodGet, "/api/v1/public/status", nil, "")
		require.Equal(t, http.StatusOK, status)
		assert.Empty(t, resp.Info.Lockdowns)
	})

	t.Run("invalid controls are refused with field errors", func(t *testing.T) {
		status, raw := env.call(t, http.MethodPost, "/api/v1/admin/platform/controls",
			map[string]interface{}{"kind": "lockdown", "feature": "everything", "reason": ""}, admin1)
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		assert.Contains(t, string(raw), "validation_failed")
		assert.Contains(t, string(raw), `"feature"`)
		assert.Contains(t, string(raw), `"reason"`)
	})
}
