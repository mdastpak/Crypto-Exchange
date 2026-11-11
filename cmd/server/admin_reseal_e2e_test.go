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
	// Row seals and audit seals as the server turns them on, in report mode.
	sealer := rowseal.New(env.db, env.handlers.DataKeys, rowseal.ModeReport)
	t.Cleanup(rowseal.Use(sealer))
	t.Cleanup(audit.UseSealer(sealer))
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

// TestAdminReseal_ARequestWrittenInSQLApprovesNothing writes a re-seal
// request in SQL, in the name of another administrator, so that one
// administrator could approve alone (#337). Without the sealed audit event
// of the request, the approval is a new request, and nothing is sealed.
func TestAdminReseal_ARequestWrittenInSQLApprovesNothing(t *testing.T) {
	env := newE2EEnv(t)
	sealer := rowseal.New(env.db, env.handlers.DataKeys, rowseal.ModeEnforce)
	t.Cleanup(rowseal.Use(sealer))
	t.Cleanup(audit.UseSealer(sealer))
	ctx := context.Background()
	named, err := env.accounts.Create(ctx, "named@example.com")
	require.NoError(t, err)
	alone, err := env.accounts.Create(ctx, "alone@example.com")
	require.NoError(t, err)
	changed, err := env.accounts.Create(ctx, "victim@example.com")
	require.NoError(t, err)
	aloneToken := env.signInAdmin(t, alone)

	_, err = env.db.Exec(`UPDATE users SET first_name = 'Changed' WHERE hk = $1`, changed.AdminID)
	require.NoError(t, err)
	row := rowseal.Users.NewRow()
	require.NoError(t, env.db.QueryRow(`SELECT `+rowseal.Users.Fields("")+` FROM users WHERE hk = $1`, changed.AdminID).Scan(row.Dest()...))
	require.ErrorIs(t, rowseal.Check(ctx, rowseal.Users, row), rowseal.ErrUnverified)
	var finding int64
	require.NoError(t, env.db.QueryRow(`SELECT id FROM integrity_findings
		WHERE table_name = 'users' AND row_key = $1 AND resolved_at IS NULL`, changed.AdminID).Scan(&finding))

	// The request, and an audit event for it that nobody sealed.
	_, err = env.db.Exec(`UPDATE integrity_findings SET reseal_requested_by = $2, reseal_requested_at = NOW(),
		reseal_request_reason = 'Reviewed' WHERE id = $1`, finding, named.AdminID)
	require.NoError(t, err)
	_, err = env.db.Exec(`INSERT INTO audit_events (hk, actor_type, actor_id, action, target_type, target_id, outcome, changes, occurred_at)
		SELECT gen_random_uuid(), 'admin', $2, $3, 'users', $4, 'success',
			jsonb_build_object('after', jsonb_build_object('finding', $1::bigint, 'reason', 'Reviewed')), reseal_requested_at
		FROM integrity_findings WHERE id = $1`, finding, named.AdminID, audit.ActionIntegrityResealRequest, changed.AdminID)
	require.NoError(t, err)

	status, resp := env.post(t, fmt.Sprintf("/api/v1/admin/integrity/findings/%d/reseal", finding),
		map[string]string{"reason": "Approved alone"}, aloneToken)
	require.Equal(t, http.StatusAccepted, status, resp.Message)
	assert.Equal(t, alone.AdminID, resp.Info["reseal_requested_by"], "the call asks again, in its own name")
	assert.Nil(t, resp.Info["resolved_at"])
	row = rowseal.Users.NewRow()
	require.NoError(t, env.db.QueryRow(`SELECT `+rowseal.Users.Fields("")+` FROM users WHERE hk = $1`, changed.AdminID).Scan(row.Dest()...))
	require.ErrorIs(t, rowseal.Check(ctx, rowseal.Users, row), rowseal.ErrUnverified, "nothing is sealed")
	assert.Empty(t, auditEvents(t, env.db, audit.ActionIntegrityReseal))
}
