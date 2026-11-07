package main

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// auditRow is an audit event, as these tests read it.
type auditRow struct {
	Action, ActorType, Outcome string
	ActorID, Reason, RequestID sql.NullString
}

// auditEvents returns the audit events of action, oldest first.
func auditEvents(t *testing.T, db *sql.DB, action string) []auditRow {
	t.Helper()
	rows, err := db.Query(`SELECT action, actor_type, outcome, actor_id::text, reason, request_id
		FROM audit_events WHERE action = $1 ORDER BY hk`, action)
	require.NoError(t, err)
	defer rows.Close()
	var events []auditRow
	for rows.Next() {
		var e auditRow
		require.NoError(t, rows.Scan(&e.Action, &e.ActorType, &e.Outcome, &e.ActorID, &e.Reason, &e.RequestID))
		events = append(events, e)
	}
	require.NoError(t, rows.Err())
	return events
}

// TestAudit_AdminSignIn signs an administrator in and out through the
// router, with a wrong authenticator code and an unknown address in between,
// and expects one audit event for each attempt (#51). The answers do not tell
// the factors apart; the audit log does.
func TestAudit_AdminSignIn(t *testing.T) {
	env := newE2EEnv(t)
	enrollment, err := env.accounts.Create(context.Background(), "audited@example.com")
	require.NoError(t, err)

	sessionID, code := env.requestAdminCode(t, enrollment.Email)
	status, _ := env.post(t, "/api/v1/admin/auth/"+sessionID, map[string]string{"otp": code, "totp": "000000"}, "")
	require.Equal(t, http.StatusUnauthorized, status)
	status, _ = env.post(t, "/api/v1/admin/auth", map[string]string{"email": "nobody@example.com"}, "")
	require.Equal(t, http.StatusOK, status, "an unknown address gets the same answer")
	token := env.signInAdmin(t, enrollment)
	status, _ = env.post(t, "/api/v1/admin/logout", nil, token)
	require.Equal(t, http.StatusOK, status)

	codes := auditEvents(t, env.db, audit.ActionAdminSignInCode)
	require.Len(t, codes, 3, "two code requests of the administrator and one of an unknown address")
	assert.Equal(t, []string{audit.Success, audit.Failure, audit.Success}, []string{codes[0].Outcome, codes[1].Outcome, codes[2].Outcome})
	assert.Equal(t, audit.ActorAnonymous, codes[1].ActorType, "an unknown address has no actor")
	assert.False(t, codes[1].ActorID.Valid)
	assert.Equal(t, "unknown_account", codes[1].Reason.String)

	signIns := auditEvents(t, env.db, audit.ActionAdminSignIn)
	require.Len(t, signIns, 2)
	assert.Equal(t, audit.Failure, signIns[0].Outcome)
	assert.Equal(t, "authenticator_code_invalid", signIns[0].Reason.String, "the log names the factor that was wrong")
	assert.Equal(t, enrollment.AdminID, signIns[0].ActorID.String)
	assert.Equal(t, audit.Success, signIns[1].Outcome)
	assert.Equal(t, enrollment.AdminID, signIns[1].ActorID.String)
	assert.NotEmpty(t, signIns[1].RequestID.String, "events carry the request ID")

	logouts := auditEvents(t, env.db, audit.ActionAdminLogout)
	require.Len(t, logouts, 1)
	assert.Equal(t, enrollment.AdminID, logouts[0].ActorID.String)
}

// TestAudit_CurrencyChangesThroughTheRouter checks that an admin change made
// through the real router, with the admin middleware, records the
// administrator of the token.
func TestAudit_CurrencyChangesThroughTheRouter(t *testing.T) {
	env := newE2EEnv(t)
	enrollment, err := env.accounts.Create(context.Background(), "changes@example.com")
	require.NoError(t, err)
	token := env.signInAdmin(t, enrollment)

	status, resp := env.post(t, "/api/v1/admin/currencies", testCurrency("AUDT"), token)
	require.Equal(t, http.StatusCreated, status, resp.Message)
	creates := auditEvents(t, env.db, audit.ActionCurrencyCreate)
	require.Len(t, creates, 1)
	assert.Equal(t, audit.ActorAdmin, creates[0].ActorType)
	assert.Equal(t, enrollment.AdminID, creates[0].ActorID.String)
}
