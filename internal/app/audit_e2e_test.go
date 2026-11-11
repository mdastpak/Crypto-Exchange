package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// auditRow is an audit event, as these tests read it.
type auditRow struct {
	Action, ActorType, Outcome string
	ActorID, Reason, RequestID sql.NullString
	TargetType, TargetID       sql.NullString
}

// auditEvents returns the audit events of action, oldest first.
func auditEvents(t *testing.T, db *sql.DB, action string) []auditRow {
	t.Helper()
	rows, err := db.Query(`SELECT action, actor_type, outcome, actor_id::text, reason, request_id, target_type, target_id
		FROM audit_events WHERE action = $1 ORDER BY hk`, action)
	require.NoError(t, err)
	defer rows.Close()
	var events []auditRow
	for rows.Next() {
		var e auditRow
		require.NoError(t, rows.Scan(&e.Action, &e.ActorType, &e.Outcome, &e.ActorID, &e.Reason, &e.RequestID, &e.TargetType, &e.TargetID))
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

// auditPage is the info of a page of the audit query.
type auditPage struct {
	Info struct {
		Items []struct {
			Action    string `json:"action"`
			ActorID   string `json:"actor_id"`
			Integrity string `json:"integrity"`
		} `json:"items"`
		NextCursor string `json:"next_cursor"`
	} `json:"info"`
}

// TestAudit_SealedWithTheRowSeals turns row seals on as the server does
// (useRowSeals), and expects the events written since to be sealed and
// reported as verified by the admin query (#337). An event changed in SQL,
// with the append-only trigger turned off, is then reported as a mismatch.
func TestAudit_SealedWithTheRowSeals(t *testing.T) {
	env := newE2EEnv(t)
	require.NotNil(t, env.handlers.DataKeys)
	saved := config.Config.Keys.RowSeals
	t.Cleanup(func() { config.Config.Keys.RowSeals = saved })
	config.Config.Keys.RowSeals.VerifyIntervalMinutes = config.DefaultRowSealsVerifyIntervalMinutes
	verifier := useRowSeals(&models.InfraServices{Postgres: &db.PostgresService{DB: env.db}}, env.handlers.DataKeys)
	t.Cleanup(func() {
		require.NoError(t, verifier.Stop(context.Background()))
		rowseal.Use(nil)
		audit.UseSealer(nil)
	})

	enrollment, err := env.accounts.Create(context.Background(), "sealed@example.com")
	require.NoError(t, err)
	token := env.signInAdmin(t, enrollment)
	status, resp := env.post(t, "/api/v1/admin/currencies", testCurrency("SLD"), token)
	require.Equal(t, http.StatusCreated, status, resp.Message)

	integrity := func() string {
		t.Helper()
		status, body := env.call(t, http.MethodGet, "/api/v1/admin/audit?action=currency.create", nil, token)
		require.Equal(t, http.StatusOK, status, string(body))
		var page auditPage
		require.NoError(t, json.Unmarshal(body, &page))
		require.Len(t, page.Info.Items, 1)
		return page.Info.Items[0].Integrity
	}
	assert.Equal(t, audit.IntegrityVerified, integrity(), "the event of the change, with its changes, is sealed")

	// The owner of the table, as someone with direct database access.
	tx, err := e2eOwnerDB(t).Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	for _, statement := range []string{
		`ALTER TABLE audit_events DISABLE TRIGGER audit_events_no_update_or_delete`,
		`UPDATE audit_events SET actor_id = gen_random_uuid() WHERE action = 'currency.create'`,
		`ALTER TABLE audit_events ENABLE TRIGGER audit_events_no_update_or_delete`,
	} {
		_, err := tx.Exec(statement)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	assert.Equal(t, audit.IntegrityMismatch, integrity(), "a change of the actor is detected")
}

// TestAudit_AdminQuery queries the audit log through the router as an
// administrator, pages through it, and checks that a user cannot read it
// and that invalid filters are reported together.
func TestAudit_AdminQuery(t *testing.T) {
	env := newE2EEnv(t)
	enrollment, err := env.accounts.Create(context.Background(), "reader@example.com")
	require.NoError(t, err)
	token := env.signInAdmin(t, enrollment)
	for _, symbol := range []string{"QRA", "QRB"} {
		status, resp := env.post(t, "/api/v1/admin/currencies", testCurrency(symbol), token)
		require.Equal(t, http.StatusCreated, status, resp.Message)
	}

	status, body := env.call(t, http.MethodGet, "/api/v1/admin/audit?action=currency.create&limit=1", nil, token)
	require.Equal(t, http.StatusOK, status, string(body))
	var first auditPage
	require.NoError(t, json.Unmarshal(body, &first))
	require.Len(t, first.Info.Items, 1)
	assert.Equal(t, audit.ActionCurrencyCreate, first.Info.Items[0].Action)
	assert.Equal(t, enrollment.AdminID, first.Info.Items[0].ActorID)
	require.NotEmpty(t, first.Info.NextCursor, "a second event remains")

	status, body = env.call(t, http.MethodGet, "/api/v1/admin/audit?action=currency.create&limit=1&cursor="+first.Info.NextCursor, nil, token)
	require.Equal(t, http.StatusOK, status, string(body))
	var second auditPage
	require.NoError(t, json.Unmarshal(body, &second))
	require.Len(t, second.Info.Items, 1)
	assert.Empty(t, second.Info.NextCursor, "the last page")

	user, _ := env.signedInUser(t, "audit-user@example.com")
	status, _ = env.call(t, http.MethodGet, "/api/v1/admin/audit", nil, user)
	assert.Equal(t, http.StatusUnauthorized, status, "a user token cannot read the audit log")

	status, body = env.call(t, http.MethodGet, "/api/v1/admin/audit?actor_id=x&outcome=maybe&limit=0&since=yesterday", nil, token)
	require.Equal(t, http.StatusBadRequest, status)
	var invalid struct {
		Code string `json:"code"`
		Info struct {
			Errors map[string]string `json:"errors"`
		} `json:"info"`
	}
	require.NoError(t, json.Unmarshal(body, &invalid))
	assert.Equal(t, "validation_failed", invalid.Code)
	assert.ElementsMatch(t, []string{"actor_id", "outcome", "limit", "since"}, mapKeys(invalid.Info.Errors))
}

// TestAudit_UserSignIn takes a new user through the router: a wrong code,
// the sign-in, a refresh, a replayed refresh token, a refresh of the revoked
// session, a second sign-in, and a logout. It expects the events of each step,
// linked by their targets, and none that holds the contact (#51).
func TestAudit_UserSignIn(t *testing.T) {
	env := newE2EEnv(t)
	const contact = "audited-user@example.com"

	status, resp := env.post(t, "/api/v1/public/auth", map[string]string{"username": contact}, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	signIn := resp.Info["uuid"].(string)
	code := env.sender.last(contact)
	wrong := strings.Map(func(r rune) rune { return '0' + (r-'0'+1)%10 }, code)
	status, _ = env.post(t, "/api/v1/public/auth/"+signIn, map[string]string{"otp": wrong}, "")
	require.Equal(t, http.StatusUnauthorized, status)
	w := env.send(t, http.MethodPost, "/api/v1/public/auth/"+signIn, map[string]string{"otp": code}, "", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	first := userSession{access: accessTokenOf(t, w), refresh: refreshTokenCookie(w).Value}
	claims, err := auth.ParseToken(first.access, auth.RoleUser, auth.TokenTypeAccess)
	require.NoError(t, err)
	first.userID, first.sessionID = claims.UUID, claims.SessionID

	second := env.refreshSession(t, first)
	env.endRefreshGrace(t)
	w = env.send(t, http.MethodPost, "/api/v1/public/auth/refresh", nil, "", first.refresh)
	require.Equal(t, http.StatusUnauthorized, w.Code, "a replayed refresh token revokes the session")
	w = env.send(t, http.MethodPost, "/api/v1/public/auth/refresh", nil, "", second.refresh)
	require.Equal(t, http.StatusUnauthorized, w.Code, "the session has ended")
	third := env.startSession(t, contact)
	w = env.send(t, http.MethodPost, "/api/v1/user/logout", nil, third.access, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	user := first.userID
	target := func(e auditRow) string { return e.TargetType.String + "/" + e.TargetID.String }
	actor := func(e auditRow) string { return e.ActorType + "/" + e.ActorID.String }
	outcome := func(e auditRow) string { return e.Outcome + "/" + e.Reason.String }

	codes := auditEvents(t, env.db, audit.ActionUserSignInCode)
	require.Len(t, codes, 2)
	assert.Equal(t, "anonymous/", actor(codes[0]), "the contact had no account yet")
	assert.Equal(t, "sign_in/"+signIn, target(codes[0]))
	assert.Equal(t, "user/"+user, actor(codes[1]), "the second request is for the user's contact")
	assert.Equal(t, []string{"success/", "success/"}, []string{outcome(codes[0]), outcome(codes[1])})

	created := auditEvents(t, env.db, audit.ActionUserCreate)
	require.Len(t, created, 1)
	assert.Equal(t, "user/"+user, actor(created[0]))
	assert.Equal(t, "user/"+user, target(created[0]))

	signIns := auditEvents(t, env.db, audit.ActionUserSignIn)
	require.Len(t, signIns, 3)
	assert.Equal(t, "failure/code_invalid", outcome(signIns[0]))
	assert.Equal(t, "anonymous/", actor(signIns[0]))
	assert.Equal(t, "sign_in/"+signIn, target(signIns[0]), "the failure links to its code request")
	assert.Equal(t, "success/", outcome(signIns[1]))
	assert.Equal(t, "user/"+user, actor(signIns[1]))
	assert.Equal(t, "session/"+first.sessionID, target(signIns[1]))
	assert.Equal(t, "session/"+third.sessionID, target(signIns[2]))

	refreshes := auditEvents(t, env.db, audit.ActionUserRefresh)
	require.Len(t, refreshes, 3)
	assert.Equal(t, []string{"success/", "failure/refresh_token_reused", "failure/session_inactive"},
		[]string{outcome(refreshes[0]), outcome(refreshes[1]), outcome(refreshes[2])})
	for _, e := range refreshes {
		assert.Equal(t, "user/"+user, actor(e))
		assert.Equal(t, "session/"+first.sessionID, target(e))
	}

	logouts := auditEvents(t, env.db, audit.ActionUserLogout)
	require.Len(t, logouts, 1)
	assert.Equal(t, "user/"+user, actor(logouts[0]))
	assert.Equal(t, "session/"+third.sessionID, target(logouts[0]))
	assert.NotEmpty(t, logouts[0].RequestID.String, "events carry the request ID")

	var leaked int
	require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE strpos(row_to_json(audit_events)::text, $1) > 0`,
		"audited-user").Scan(&leaked))
	assert.Zero(t, leaked, "no event holds the contact")
}

func mapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
