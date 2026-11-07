package public

import (
	"context"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/pagination"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Verify records the use of a session at most once a minute (#193).
func TestSessionStore_RealSchema_VerifyRecordsTheUse(t *testing.T) {
	f := newSessionFixture(t)
	ctx := context.Background()
	require.NoError(t, f.store.Create(ctx, f.session, f.event(audit.ActionUserSignIn)))
	lastUsed := func() time.Time {
		var at time.Time
		require.NoError(t, f.conn.QueryRow(`SELECT last_used_at FROM sessions WHERE id = $1`, f.session.ID).Scan(&at))
		return at
	}
	created := lastUsed()

	require.NoError(t, f.store.Verify(ctx, f.session.ID, f.userID))
	assert.Equal(t, created, lastUsed(), "a use within a minute is not written again")

	_, err := f.conn.Exec(`UPDATE sessions SET last_used_at = NOW() - INTERVAL '2 minutes' WHERE id = $1`, f.session.ID)
	require.NoError(t, err)
	require.NoError(t, f.store.Verify(ctx, f.session.ID, f.userID))
	assert.WithinDuration(t, time.Now(), lastUsed(), 5*time.Second)

	// A revoked session is not touched, and stays invalid.
	_, err = f.conn.Exec(`UPDATE sessions SET status = 'REVOKED', last_used_at = NOW() - INTERVAL '2 minutes' WHERE id = $1`, f.session.ID)
	require.NoError(t, err)
	before := lastUsed()
	assert.ErrorIs(t, f.store.Verify(ctx, f.session.ID, f.userID), auth.ErrSessionInvalid)
	assert.Equal(t, before, lastUsed())
}

// The revocations of the session list commit even when their audit events
// cannot be written, like every revocation.
func TestSessionStore_RealSchema_RevokeOthersWithoutTheAuditLog(t *testing.T) {
	f := newSessionFixture(t)
	ctx := context.Background()
	require.NoError(t, f.store.Create(ctx, f.session, f.event(audit.ActionUserSignIn)))
	others := []string{uuid.NewString(), uuid.NewString()}
	for _, id := range others {
		s := f.session
		s.ID, s.AccessTokenHash, s.RefreshTokenHash = id, auth.HashToken("a-"+id), auth.HashToken("r-"+id)
		require.NoError(t, f.store.Create(ctx, s, f.event(audit.ActionUserSignIn)))
	}

	page, err := f.store.List(ctx, f.userID, pagination.Request{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, page.Items, 3)

	revoked, err := f.store.RevokeOthers(ctx, f.userID, f.session.ID, f.unwritable(audit.ActionUserSessionRevoke))
	require.NoError(t, err)
	assert.Equal(t, 2, revoked)
	for _, id := range others {
		assert.ErrorIs(t, f.store.Verify(ctx, id, f.userID), auth.ErrSessionInvalid)
	}
	require.NoError(t, f.store.Verify(ctx, f.session.ID, f.userID), "the current session stays")

	assert.ErrorIs(t, f.store.RevokeOwn(ctx, others[0], f.userID, f.event(audit.ActionUserSessionRevoke)), auth.ErrSessionInvalid)
	assert.ErrorIs(t, f.store.RevokeOwn(ctx, f.session.ID, uuid.NewString(), f.event(audit.ActionUserSessionRevoke)), auth.ErrSessionInvalid,
		"another user cannot end the session")
	require.NoError(t, f.store.RevokeOwn(ctx, f.session.ID, f.userID, f.unwritable(audit.ActionUserSessionRevoke)))
	assert.Equal(t, "REVOKED", f.status(t))
}
