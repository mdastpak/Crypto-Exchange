package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAdminReseal_NeedsTwoAdministrators re-seals a row that was changed in
// SQL through the route: one administrator asks, and another approves
// (#308).
func TestAdminReseal_NeedsTwoAdministrators(t *testing.T) {
	env := newE2EEnv(t)
	// Row seals as the server turns them on, in report mode.
	t.Cleanup(rowseal.Use(rowseal.New(env.db, env.handlers.DataKeys, rowseal.ModeReport)))
	ctx := context.Background()
	asks, err := env.accounts.Create(ctx, "asks@example.com")
	require.NoError(t, err)
	approves, err := env.accounts.Create(ctx, "approves@example.com")
	require.NoError(t, err)
	changed, err := env.accounts.Create(ctx, "changed@example.com")
	require.NoError(t, err)
	asksToken, approvesToken, changedToken := env.signInAdmin(t, asks), env.signInAdmin(t, approves), env.signInAdmin(t, changed)

	// A change outside the backend, which the next request of the
	// administrator finds.
	_, err = env.db.Exec(`UPDATE users SET first_name = 'Changed' WHERE hk = $1`, changed.AdminID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, env.send(t, http.MethodGet, "/api/v1/admin/sessions", nil, changedToken, "").Code,
		"report mode still serves the row")
	var finding int64
	require.NoError(t, env.db.QueryRow(`SELECT id FROM integrity_findings
		WHERE table_name = 'users' AND row_key = $1 AND resolved_at IS NULL`, changed.AdminID).Scan(&finding))
	path := fmt.Sprintf("/api/v1/admin/integrity/findings/%d/reseal", finding)
	reseal := func(token, reason string) (int, e2eResponse) {
		return env.post(t, path, map[string]string{"reason": reason}, token)
	}

	status, resp := reseal(changedToken, "it is my row and it is fine")
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "own_row", resp.Code)

	status, resp = reseal(asksToken, "Reviewed fix of ticket 12")
	require.Equal(t, http.StatusAccepted, status, resp.Message)
	assert.Equal(t, asks.AdminID, resp.Info["reseal_requested_by"])
	assert.Nil(t, resp.Info["resolved_at"])

	status, resp = reseal(asksToken, "I approve my own request")
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "second_approval_required", resp.Code)

	status, resp = reseal(approvesToken, "Confirmed with ticket 12")
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Equal(t, "resealed", resp.Info["resolution"])
	assert.Equal(t, approves.AdminID, resp.Info["resolved_by"])

	status, resp = reseal(approvesToken, "again")
	assert.Equal(t, http.StatusConflict, status, resp.Message)
	assert.Len(t, auditEvents(t, env.db, audit.ActionIntegrityResealRequest), 1)
	reseals := auditEvents(t, env.db, audit.ActionIntegrityReseal)
	require.Len(t, reseals, 1)
	assert.Equal(t, approves.AdminID, reseals[0].ActorID.String)
}
