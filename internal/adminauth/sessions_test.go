package adminauth

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startTestSession starts a session of adminID that lasts an hour.
func startTestSession(t *testing.T, service *Service, adminID string, limit int) string {
	t.Helper()
	session := NewSession{ID: NewSessionID(), AdminID: adminID, IP: "203.0.113.7", UserAgent: "test-agent",
		ExpiresAt: time.Now().Add(time.Hour), Max: limit}
	event := audit.Event{ActorType: audit.ActorAdmin, ActorID: adminID, Action: audit.ActionAdminSignIn,
		Outcome: audit.Success, TargetType: audit.TargetAdminSession, TargetID: session.ID}
	require.NoError(t, service.StartSession(context.Background(), session, event))
	return session.ID
}

func sessionReason(t *testing.T, conn *sql.DB, id string) string {
	t.Helper()
	var reason sql.NullString
	require.NoError(t, conn.QueryRow(`SELECT revoked_reason FROM admin_sessions WHERE id = $1`, id).Scan(&reason))
	return reason.String
}

func TestSessions_AreListedVerifiedAndEnded(t *testing.T) {
	service, conn := newTestService(t)
	ctx := context.Background()
	enrollment, err := service.Create(ctx, "sessions@example.com")
	require.NoError(t, err)
	other, err := service.Create(ctx, "other@example.com")
	require.NoError(t, err)
	admin := enrollment.AdminID

	first := startTestSession(t, service, admin, 3)
	second := startTestSession(t, service, admin, 3)
	require.NoError(t, service.VerifySession(ctx, first, admin))
	assert.ErrorIs(t, service.VerifySession(ctx, first, other.AdminID), ErrSessionEnded, "a session belongs to its administrator")
	assert.ErrorIs(t, service.VerifySession(ctx, uuid.NewString(), admin), ErrSessionEnded)
	assert.ErrorIs(t, service.VerifySession(ctx, "", admin), ErrSessionEnded, "a token without a session")

	sessions, err := service.ListSessions(ctx, admin, second)
	require.NoError(t, err)
	require.Len(t, sessions, 2)
	assert.Equal(t, second, sessions[0].ID, "newest first")
	assert.True(t, sessions[0].Current)
	assert.False(t, sessions[1].Current)
	assert.Equal(t, "203.0.113.7", sessions[1].IP)
	assert.Equal(t, "test-agent", sessions[1].UserAgent)
	require.NotNil(t, sessions[1].LastUsedAt, "a verified session notes its use")
	assert.Nil(t, sessions[0].LastUsedAt)

	event := audit.Event{ActorType: audit.ActorAdmin, ActorID: admin, Action: audit.ActionAdminSessionRevoke,
		Outcome: audit.Success, TargetType: audit.TargetAdminSession, TargetID: first}
	assert.ErrorIs(t, service.EndSession(ctx, first, other.AdminID, SessionRevoked, event), ErrSessionEnded,
		"an administrator ends only their own sessions")
	require.NoError(t, service.EndSession(ctx, first, admin, SessionRevoked, event))
	assert.ErrorIs(t, service.VerifySession(ctx, first, admin), ErrSessionEnded)
	assert.ErrorIs(t, service.EndSession(ctx, first, admin, SessionRevoked, event), ErrSessionEnded, "it ends once")
	assert.Equal(t, SessionRevoked, sessionReason(t, conn, first))
	require.NoError(t, service.VerifySession(ctx, second, admin), "the other session stays")

	var revokes int
	require.NoError(t, conn.QueryRow(`SELECT count(*) FROM audit_events WHERE action = $1`, audit.ActionAdminSessionRevoke).Scan(&revokes))
	assert.Equal(t, 1, revokes, "only the session that ended is recorded")

	_, err = conn.Exec(`UPDATE admin_sessions SET expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, second)
	require.NoError(t, err)
	assert.ErrorIs(t, service.VerifySession(ctx, second, admin), ErrSessionEnded, "a session expires with its token")
}

func TestStartSession_EndsTheOldestPastTheCap(t *testing.T) {
	service, conn := newTestService(t)
	ctx := context.Background()
	enrollment, err := service.Create(ctx, "cap@example.com")
	require.NoError(t, err)
	admin := enrollment.AdminID

	first := startTestSession(t, service, admin, 2)
	second := startTestSession(t, service, admin, 2)
	third := startTestSession(t, service, admin, 2)

	assert.ErrorIs(t, service.VerifySession(ctx, first, admin), ErrSessionEnded)
	assert.Equal(t, SessionReplaced, sessionReason(t, conn, first))
	require.NoError(t, service.VerifySession(ctx, second, admin))
	require.NoError(t, service.VerifySession(ctx, third, admin))
}

func TestSessions_EndWithTheAccessOfTheAdministrator(t *testing.T) {
	service, conn := newTestService(t)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		change func(email string) error
		reason string
	}{
		"disable":      {func(email string) error { return service.Disable(ctx, email) }, SessionAdminDisabled},
		"reset-totp":   {func(email string) error { _, err := service.ResetTOTP(ctx, email); return err }, SessionAuthenticator},
		"end-sessions": {func(email string) error { _, err := service.EndSessions(ctx, email); return err }, SessionEndedByOperator},
	} {
		t.Run(name, func(t *testing.T) {
			email := name + "@example.com"
			enrollment, err := service.Create(ctx, email)
			require.NoError(t, err)
			session := startTestSession(t, service, enrollment.AdminID, 2)

			require.NoError(t, tc.change(email))
			assert.ErrorIs(t, service.VerifySession(ctx, session, enrollment.AdminID), ErrSessionEnded)
			assert.Equal(t, tc.reason, sessionReason(t, conn, session))
		})
	}

	ended, err := service.EndSessions(ctx, "end-sessions@example.com")
	require.NoError(t, err)
	assert.Zero(t, ended)
	_, err = service.EndSessions(ctx, "nobody@example.com")
	assert.ErrorIs(t, err, ErrNotFound)
}

// A sign-in that cannot be recorded starts no session.
func TestStartSession_NeedsItsEvent(t *testing.T) {
	service, conn := newTestService(t)
	ctx := context.Background()
	enrollment, err := service.Create(ctx, "unrecorded@example.com")
	require.NoError(t, err)

	session := NewSession{ID: NewSessionID(), AdminID: enrollment.AdminID, ExpiresAt: time.Now().Add(time.Hour), Max: 2}
	err = service.StartSession(ctx, session, audit.Event{Action: "Not An Action", Outcome: audit.Success})
	require.Error(t, err)
	var count int
	require.NoError(t, conn.QueryRow(`SELECT count(*) FROM admin_sessions`).Scan(&count))
	assert.Zero(t, count)
}

func TestStepUp_ElevatesTheSessionWithAFreshCode(t *testing.T) {
	service, conn := newTestService(t)
	ctx := context.Background()
	enrollment, err := service.Create(ctx, "elevate@example.com")
	require.NoError(t, err)
	admin := enrollment.AdminID
	session := startTestSession(t, service, admin, 2)
	event := audit.Event{ActorType: audit.ActorAdmin, ActorID: admin, Action: audit.ActionAdminStepUp,
		TargetType: audit.TargetAdminSession, TargetID: session}
	now := time.Now()

	elevated, err := service.Elevated(ctx, session, admin)
	require.NoError(t, err)
	assert.False(t, elevated, "this sign-in asked for no step-up time")
	elevated, err = service.Elevated(ctx, "", admin)
	require.NoError(t, err)
	assert.False(t, elevated)

	_, err = service.StepUp(ctx, session, admin, "000000", 5*time.Minute, event)
	assert.ErrorIs(t, err, ErrInvalidCode)
	until, err := service.StepUp(ctx, session, admin, at(t, service, now, enrollment), 5*time.Minute, event)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(5*time.Minute), until, 10*time.Second)
	elevated, err = service.Elevated(ctx, session, admin)
	require.NoError(t, err)
	assert.True(t, elevated)
	_, err = service.StepUp(ctx, session, admin, at(t, service, now, enrollment), 5*time.Minute, event)
	assert.ErrorIs(t, err, ErrInvalidCode, "a code is accepted once")

	var failures int
	require.NoError(t, conn.QueryRow(`SELECT step_up_failures FROM admin_sessions WHERE id = $1`, session).Scan(&failures))
	assert.Equal(t, 1, failures, "an accepted code starts the count again")

	for i := failures; i < MaxStepUpFailures; i++ {
		_, err = service.StepUp(ctx, session, admin, "000000", 5*time.Minute, event)
		require.ErrorIs(t, err, ErrInvalidCode)
	}
	_, err = service.StepUp(ctx, session, admin, "000000", 5*time.Minute, event)
	assert.ErrorIs(t, err, ErrSessionEnded, "the code past the limit ends the session")
	assert.Equal(t, SessionStepUpFailed, sessionReason(t, conn, session))
	_, err = service.StepUp(ctx, session, admin, at(t, service, now.Add(time.Minute), enrollment), 5*time.Minute, event)
	assert.ErrorIs(t, err, ErrSessionEnded)

	var outcomes string
	require.NoError(t, conn.QueryRow(`SELECT string_agg(outcome || ':' || COALESCE(reason, ''), ',' ORDER BY hk)
		FROM audit_events WHERE action = $1`, audit.ActionAdminStepUp).Scan(&outcomes))
	assert.Equal(t, "failure:authenticator_code_invalid,success:,failure:authenticator_code_invalid,"+
		"failure:authenticator_code_invalid,failure:authenticator_code_invalid,failure:authenticator_code_invalid,"+
		"failure:authenticator_code_invalid,failure:too_many_wrong_codes", outcomes)
}
