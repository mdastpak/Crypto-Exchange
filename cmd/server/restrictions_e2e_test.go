package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUserRestrictions_FreezeAndLift walks through #220: an administrator
// holds a currency of a user, blocks withdrawals, and freezes the account,
// which ends the user's tokens at once and refuses sign-in, then lifts the
// freeze.
func TestUserRestrictions_FreezeAndLift(t *testing.T) {
	env := newE2EEnv(t)
	admin, err := env.accounts.Create(context.Background(), "compliance@example.com")
	require.NoError(t, err)
	adminToken := env.signInAdmin(t, admin)
	const email = "subject@example.com"
	token, userID := env.signedInUser(t, email)
	status, resp := env.post(t, "/api/v1/admin/currencies", testCurrency("HLD"), adminToken)
	require.Equal(t, http.StatusCreated, status, resp.Message)
	currency, _ := resp.Info["hk"].(string)
	require.NotEmpty(t, currency)
	base := "/api/v1/admin/users/" + userID + "/restrictions"

	// Invalid restrictions.
	for name, body := range map[string]map[string]interface{}{
		"an unknown scope":           {"scope": "planet", "reason": "fraud"},
		"a freeze with operations":   {"scope": "account", "reason": "fraud", "operations": []string{"withdraw"}},
		"a hold without a currency":  {"scope": "asset", "reason": "fraud", "operations": []string{"withdraw"}},
		"an unknown operation":       {"scope": "operation", "reason": "fraud", "operations": []string{"fly"}},
		"an unknown reason":          {"scope": "operation", "reason": "curiosity", "operations": []string{"withdraw"}},
		"an end in the past":         {"scope": "operation", "reason": "fraud", "operations": []string{"withdraw"}, "ends_at": "2020-01-01T00:00:00Z"},
		"a currency for an op block": {"scope": "operation", "reason": "fraud", "operations": []string{"withdraw"}, "currency_hk": currency},
	} {
		status, resp := env.post(t, base, body, adminToken)
		assert.Equal(t, http.StatusBadRequest, status, name)
		assert.Equal(t, "validation_failed", resp.Code, name)
	}
	status, _ = env.post(t, base, map[string]interface{}{"scope": "account", "reason": "fraud"}, token)
	assert.Equal(t, http.StatusUnauthorized, status, "a user token cannot restrict")

	// A hold and a block, which the user sees without the reason.
	status, resp = env.post(t, base, map[string]interface{}{
		"scope": "asset", "currency_hk": currency, "operations": []string{"withdraw", "convert"},
		"reason": "aml_review", "case_ref": "AML-1", "note": "large inflow",
	}, adminToken)
	require.Equal(t, http.StatusCreated, status, resp.Message)
	status, resp = env.post(t, base, map[string]interface{}{
		"scope": "operation", "operations": []string{"trade"}, "reason": "user_request", "ends_at": "2099-01-01T00:00:00Z",
	}, adminToken)
	require.Equal(t, http.StatusCreated, status, resp.Message)

	status, raw := env.call(t, http.MethodGet, "/api/v1/user/restrictions", nil, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.NotContains(t, string(raw), "aml_review")
	assert.NotContains(t, string(raw), "AML-1")
	assert.NotContains(t, string(raw), "large inflow")
	var mine struct {
		Info struct {
			Items []map[string]interface{} `json:"items"`
		} `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &mine))
	require.Len(t, mine.Info.Items, 2)
	assert.Equal(t, "operation", mine.Info.Items[0]["scope"])
	assert.Equal(t, "2099-01-01T00:00:00Z", mine.Info.Items[0]["until"], "a user request shows its end")
	assert.Equal(t, currency, mine.Info.Items[1]["currency_hk"])

	// A freeze ends the user's tokens at once and refuses sign-in.
	status, resp = env.post(t, base, map[string]interface{}{"scope": "account", "reason": "court_order", "case_ref": "CO-7"}, adminToken)
	require.Equal(t, http.StatusCreated, status, resp.Message)
	assert.Equal(t, float64(1), resp.Info["ended_sessions"])
	freeze := resp.Info["restriction"].(map[string]interface{})["id"].(string)

	status, raw = env.call(t, http.MethodGet, "/api/v1/user/profile", nil, token)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Contains(t, string(raw), `"code":"account_frozen"`)
	status, resp = env.signInUser(t, email)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "account_frozen", resp.Code)

	// The list holds the reasons for administrators.
	status, raw = env.call(t, http.MethodGet, base+"?active=true", nil, adminToken)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Contains(t, string(raw), "court_order")
	assert.Contains(t, string(raw), "CO-7")

	// Lifting needs a note, and works once.
	status, _ = env.post(t, base+"/"+freeze+"/lift", map[string]string{"note": ""}, adminToken)
	assert.Equal(t, http.StatusBadRequest, status)
	status, resp = env.post(t, base+"/"+freeze+"/lift?version=1", map[string]string{"note": "order revoked"}, adminToken)
	require.Equal(t, http.StatusOK, status, resp.Message)
	status, resp = env.post(t, base+"/"+freeze+"/lift", map[string]string{"note": "again"}, adminToken)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "already_lifted", resp.Code)

	status, resp = env.signInUser(t, email)
	require.Equal(t, http.StatusOK, status, "a lifted freeze lets the user sign in: %s", resp.Message)
	status, _ = env.call(t, http.MethodGet, "/api/v1/user/profile", nil, token)
	assert.Equal(t, http.StatusUnauthorized, status, "the sessions that the freeze ended stay ended")

	var actions int
	require.NoError(t, env.db.QueryRow(`SELECT count(*) FROM audit_events
		WHERE target_id = $1 AND action IN ('user.restriction_add', 'user.restriction_lift') AND actor_id = $2`,
		userID, admin.AdminID).Scan(&actions))
	assert.Equal(t, 4, actions)
}
