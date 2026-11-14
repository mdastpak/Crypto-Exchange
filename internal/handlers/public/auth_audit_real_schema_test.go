package public

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sessionFixture is a stored session of a new user, with the store that
// changes it.
type sessionFixture struct {
	store   *auth.PostgresSessionStore
	conn    *sql.DB
	userID  string
	session auth.Session
}

func newSessionFixture(t *testing.T) sessionFixture {
	t.Helper()
	handler, conn, _ := setupRealSchemaHandler(t)
	hk, err := handler.checkOrCreateUser(context.Background(), audit.Event{}, "email", "audited-session@example.com", signUpInfo{})
	require.NoError(t, err)
	f := sessionFixture{store: auth.NewPostgresSessionStore(conn), conn: conn, userID: hk.String()}
	f.session = auth.Session{
		ID: uuid.NewString(), UserID: f.userID, AuthMethod: "OTP",
		AccessTokenHash: auth.HashToken("access-1"), RefreshTokenHash: auth.HashToken("refresh-1"),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	return f
}

// event returns a valid event of action on the fixture's session.
func (f sessionFixture) event(action string) audit.Event {
	return audit.Event{ActorType: audit.ActorUser, ActorID: f.userID, Action: action, Outcome: audit.Success,
		TargetType: audit.TargetSession, TargetID: f.session.ID}
}

// unwritable returns an event of action that the audit_events checks refuse:
// a user actor without its hk.
func (f sessionFixture) unwritable(action string) audit.Event {
	e := f.event(action)
	e.ActorID = ""
	return e
}

func (f sessionFixture) count(t *testing.T, query string, args ...interface{}) int {
	t.Helper()
	var n int
	require.NoError(t, f.conn.QueryRow(query, args...).Scan(&n))
	return n
}

func (f sessionFixture) events(t *testing.T, action string) int {
	return f.count(t, `SELECT COUNT(*) FROM audit_events WHERE action = $1 AND target_id = $2`, action, f.session.ID)
}

func (f sessionFixture) status(t *testing.T) string {
	t.Helper()
	var status string
	require.NoError(t, f.conn.QueryRow(`SELECT status FROM sessions WHERE id = $1`, f.session.ID).Scan(&status))
	return status
}

func (f sessionFixture) rotation(presented, next string) auth.SessionRotation {
	return auth.SessionRotation{SessionID: f.session.ID, UserID: f.userID,
		PresentedRefreshHash: auth.HashToken(presented), NewRefreshHash: auth.HashToken(next),
		NewAccessHash: auth.HashToken("access-" + next), ExpiresAt: time.Now().Add(time.Hour)}
}

// TestSessionStore_RealSchema_GrantsCommitOnlyWithTheirEvent checks that a
// new session and new tokens are not stored when their audit event cannot be
// written, and are stored with it otherwise (#51).
func TestSessionStore_RealSchema_GrantsCommitOnlyWithTheirEvent(t *testing.T) {
	f := newSessionFixture(t)
	ctx := context.Background()

	require.Error(t, f.store.Create(ctx, f.session, f.unwritable(audit.ActionUserSignIn)))
	assert.Zero(t, f.count(t, `SELECT COUNT(*) FROM sessions WHERE id = $1`, f.session.ID), "no session without its event")

	require.NoError(t, f.store.Create(ctx, f.session, f.event(audit.ActionUserSignIn)))
	assert.Equal(t, 1, f.events(t, audit.ActionUserSignIn))

	require.Error(t, f.store.Rotate(ctx, f.rotation("refresh-1", "refresh-2"), f.unwritable(audit.ActionUserRefresh)))
	var hash string
	require.NoError(t, f.conn.QueryRow(`SELECT refresh_token_hash FROM sessions WHERE id = $1`, f.session.ID).Scan(&hash))
	assert.Equal(t, auth.HashToken("refresh-1"), hash, "no new tokens without their event")

	require.NoError(t, f.store.Rotate(ctx, f.rotation("refresh-1", "refresh-2"), f.event(audit.ActionUserRefresh)))
	assert.Equal(t, 1, f.events(t, audit.ActionUserRefresh))
}

// TestSessionStore_RealSchema_ReuseRecordsTheRevocation checks that a
// replayed refresh token revokes the session and records the failure in the
// same transaction.
func TestSessionStore_RealSchema_ReuseRecordsTheRevocation(t *testing.T) {
	f := newSessionFixture(t)
	ctx := context.Background()
	require.NoError(t, f.store.Create(ctx, f.session, f.event(audit.ActionUserSignIn)))
	require.NoError(t, f.store.Rotate(ctx, f.rotation("refresh-1", "refresh-2"), f.event(audit.ActionUserRefresh)))

	err := f.store.Rotate(ctx, f.rotation("refresh-1", "refresh-3"), f.event(audit.ActionUserRefresh))
	require.ErrorIs(t, err, auth.ErrRefreshTokenReused)
	assert.Equal(t, "REVOKED", f.status(t))
	var outcome, reason string
	require.NoError(t, f.conn.QueryRow(`SELECT outcome, reason FROM audit_events
		WHERE action = $1 AND target_id = $2 ORDER BY hk DESC LIMIT 1`, audit.ActionUserRefresh, f.session.ID).Scan(&outcome, &reason))
	assert.Equal(t, audit.Failure, outcome)
	assert.Equal(t, auth.ReasonRefreshTokenReused, reason)

	err = f.store.Rotate(ctx, f.rotation("refresh-2", "refresh-4"), f.event(audit.ActionUserRefresh))
	require.ErrorIs(t, err, auth.ErrSessionInvalid)
	assert.NotErrorIs(t, err, auth.ErrRefreshTokenReused)
	assert.Equal(t, 2, f.events(t, audit.ActionUserRefresh), "a session that has ended records nothing in the store")
}

// TestSessionStore_RealSchema_RevocationsDoNotWaitOnTheirEvent checks that a
// logout, and a revocation for reuse, end the session even when their audit
// event cannot be written, so that an unavailable audit log never keeps a
// session alive.
func TestSessionStore_RealSchema_RevocationsDoNotWaitOnTheirEvent(t *testing.T) {
	t.Run("logout", func(t *testing.T) {
		f := newSessionFixture(t)
		ctx := context.Background()
		require.NoError(t, f.store.Create(ctx, f.session, f.event(audit.ActionUserSignIn)))

		require.NoError(t, f.store.Revoke(ctx, f.session.ID, f.userID, f.unwritable(audit.ActionUserLogout)))
		assert.Equal(t, "REVOKED", f.status(t))
		assert.Zero(t, f.events(t, audit.ActionUserLogout))
	})

	t.Run("reuse", func(t *testing.T) {
		f := newSessionFixture(t)
		ctx := context.Background()
		require.NoError(t, f.store.Create(ctx, f.session, f.event(audit.ActionUserSignIn)))
		require.NoError(t, f.store.Rotate(ctx, f.rotation("refresh-1", "refresh-2"), f.event(audit.ActionUserRefresh)))

		err := f.store.Rotate(ctx, f.rotation("refresh-1", "refresh-3"), f.unwritable(audit.ActionUserRefresh))
		require.ErrorIs(t, err, auth.ErrRefreshTokenReused)
		assert.Equal(t, "REVOKED", f.status(t))
		assert.Equal(t, 1, f.events(t, audit.ActionUserRefresh), "only the earlier rotation is recorded")
	})
}

// TestCreateUser_RealSchema_RecordsTheNewUser checks that a first sign-in
// records the new user, in the transaction that creates it, and that a
// returning user is not recorded again.
func TestCreateUser_RealSchema_RecordsTheNewUser(t *testing.T) {
	handler, conn, _ := setupRealSchemaHandler(t)
	ctx := context.Background()
	origin := audit.Event{RequestID: "req-1", IP: "203.0.113.9"}

	hk, err := handler.checkOrCreateUser(ctx, origin, "email", "recorded@example.com", signUpInfo{})
	require.NoError(t, err)
	_, err = handler.checkOrCreateUser(ctx, origin, "email", "recorded@example.com", signUpInfo{})
	require.NoError(t, err)

	var actorType, actorID, targetType, targetID, requestID, ip string
	require.NoError(t, conn.QueryRow(`SELECT actor_type, actor_id::text, target_type, target_id, request_id, host(ip)
		FROM audit_events WHERE action = $1`, audit.ActionUserCreate).Scan(&actorType, &actorID, &targetType, &targetID, &requestID, &ip))
	assert.Equal(t, audit.ActorUser, actorType)
	assert.Equal(t, hk.String(), actorID)
	assert.Equal(t, audit.TargetUser, targetType)
	assert.Equal(t, hk.String(), targetID)
	assert.Equal(t, "req-1", requestID)
	assert.Equal(t, "203.0.113.9", ip)
}

// TestSessionStore_RealSchema_ConcurrentRefreshWithinTheGrace checks that the
// previous refresh token, within the grace period, gets a new access token
// without a rotation, and records it (#194); and that it is a reuse after
// the grace period, or without one.
func TestSessionStore_RealSchema_ConcurrentRefreshWithinTheGrace(t *testing.T) {
	f := newSessionFixture(t)
	ctx := context.Background()
	require.NoError(t, f.store.Create(ctx, f.session, f.event(audit.ActionUserSignIn)))
	withGrace := func(r auth.SessionRotation) auth.SessionRotation {
		r.Grace = 10 * time.Second
		return r
	}
	require.NoError(t, f.store.Rotate(ctx, withGrace(f.rotation("refresh-1", "refresh-2")), f.event(audit.ActionUserRefresh)))

	err := f.store.Rotate(ctx, withGrace(f.rotation("refresh-1", "refresh-3")), f.event(audit.ActionUserRefresh))
	require.ErrorIs(t, err, auth.ErrConcurrentRefresh)
	assert.Equal(t, "ACTIVE", f.status(t))
	var reason string
	require.NoError(t, f.conn.QueryRow(`SELECT reason FROM audit_events WHERE action = $1 AND target_id = $2 AND outcome = 'success'
		ORDER BY hk DESC LIMIT 1`, audit.ActionUserRefresh, f.session.ID).Scan(&reason))
	assert.Equal(t, auth.ReasonConcurrentRefresh, reason)
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM sessions WHERE id = $1 AND refresh_token_hash = $2`, f.session.ID, auth.HashToken("refresh-2")),
		"the current refresh token stays")

	_, err = f.conn.Exec(`UPDATE sessions SET rotated_at = NOW() - INTERVAL '1 minute' WHERE id = $1`, f.session.ID)
	require.NoError(t, err)
	err = f.store.Rotate(ctx, withGrace(f.rotation("refresh-1", "refresh-4")), f.event(audit.ActionUserRefresh))
	require.ErrorIs(t, err, auth.ErrRefreshTokenReused, "after the grace period")
	assert.Equal(t, "REVOKED", f.status(t))
}

// Without a grace period, the previous refresh token is a reuse at once.
func TestSessionStore_RealSchema_NoGraceWithoutAGracePeriod(t *testing.T) {
	f := newSessionFixture(t)
	ctx := context.Background()
	require.NoError(t, f.store.Create(ctx, f.session, f.event(audit.ActionUserSignIn)))
	require.NoError(t, f.store.Rotate(ctx, f.rotation("refresh-1", "refresh-2"), f.event(audit.ActionUserRefresh)))
	assert.ErrorIs(t, f.store.Rotate(ctx, f.rotation("refresh-1", "refresh-3"), f.event(audit.ActionUserRefresh)), auth.ErrRefreshTokenReused)
}
