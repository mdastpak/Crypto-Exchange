package userauth

import (
	"context"
	"database/sql"
	"encoding/base32"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"
	"github.com/mdastpak/crypto-exchange/internal/totp"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// codeOf returns the current code of an enrolled secret.
func codeOf(t *testing.T, enrollment TOTPEnrollment, at time.Time) string {
	t.Helper()
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enrollment.Secret)
	require.NoError(t, err)
	return totp.Code(secret, totp.Step(at))
}

// totpUser is a user with a password, and the authenticator store.
func totpUser(t *testing.T, p *Passwords, db *sql.DB, live *keys.Live) (string, *TOTP) {
	t.Helper()
	hk := newSealedUser(t, db)
	setPassword(t, p, hk)
	return hk, &TOTP{DB: db, Sealer: live, Passwords: p}
}

// TestTOTP_TheBackendSealsItsRows enrolls, confirms, and removes an
// authenticator through the backend, which seals each write (#337), so
// every check in enforce mode passes.
func TestTOTP_TheBackendSealsItsRows(t *testing.T) {
	p, db, live := newRecordEnv(t, rowseal.ModeEnforce)
	ctx := context.Background()
	hk, store := totpUser(t, p, db, live)
	event := audit.Event{ActorType: audit.ActorUser, ActorID: hk}

	enrollment, err := store.Begin(ctx, hk, "user@example.com")
	require.NoError(t, err)
	now := time.Now()
	_, err = store.Confirm(ctx, hk, codeOf(t, enrollment, now), event)
	require.NoError(t, err)
	enabled, err := store.Enabled(ctx, hk)
	require.NoError(t, err)
	assert.True(t, enabled)

	store.Now = func() time.Time { return now.Add(time.Minute) }
	require.NoError(t, store.Verify(ctx, hk, codeOf(t, enrollment, now.Add(time.Minute))))
	_, err = store.Remove(ctx, hk, event)
	require.NoError(t, err)
	enabled, err = store.Enabled(ctx, hk)
	require.NoError(t, err)
	assert.False(t, enabled)
}

// TestTOTP_TurnedOnInSQLProvesNothing turns an authenticator on in SQL: a
// secret that was never confirmed, and one that was removed. In enforce
// mode neither is enabled, and neither proves a code.
func TestTOTP_TurnedOnInSQLProvesNothing(t *testing.T) {
	p, db, live := newRecordEnv(t, rowseal.ModeEnforce)
	ctx := context.Background()

	pending, pendingStore := totpUser(t, p, db, live)
	enrollment, err := pendingStore.Begin(ctx, pending, "pending@example.com")
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE user_totp SET confirmed_at = NOW() WHERE user_hk = $1`, pending)
	require.NoError(t, err)
	_, err = pendingStore.Enabled(ctx, pending)
	assert.ErrorIs(t, err, rowseal.ErrUnverified)
	assert.ErrorIs(t, pendingStore.Verify(ctx, pending, codeOf(t, enrollment, time.Now())), rowseal.ErrUnverified,
		"a secret confirmed in SQL signs nobody in")

	removed, removedStore := totpUser(t, p, db, live)
	event := audit.Event{ActorType: audit.ActorUser, ActorID: removed}
	enrolled, err := removedStore.Begin(ctx, removed, "removed@example.com")
	require.NoError(t, err)
	_, err = removedStore.Confirm(ctx, removed, codeOf(t, enrolled, time.Now()), event)
	require.NoError(t, err)
	_, err = removedStore.Remove(ctx, removed, event)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE user_totp SET removed_at = NULL WHERE user_hk = $1`, removed)
	require.NoError(t, err)
	_, err = removedStore.Enabled(ctx, removed)
	assert.ErrorIs(t, err, rowseal.ErrUnverified, "an authenticator turned back on in SQL")

	var findings int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM integrity_findings WHERE table_name = 'user_totp' AND resolved_at IS NULL`).Scan(&findings))
	assert.Equal(t, 2, findings)
}

// recoveryUser is a user with a verified email address, which a recovery
// proves, and a password.
func recoveryUser(t *testing.T, p *Passwords, db *sql.DB, email string) (string, int64) {
	t.Helper()
	hk := usertest.Insert(t, db, usertest.User{Email: email})
	require.NoError(t, rowseal.Reseal(context.Background(), db, rowseal.Users, "hk = $1", hk))
	setPassword(t, p, hk)
	var contactID int64
	require.NoError(t, db.QueryRow(`SELECT id FROM user_contacts WHERE user_hk = $1`, hk).Scan(&contactID))
	return hk, contactID
}

func standardPlan() (string, []string) { return RecoveryStandard, nil }

// TestRecovery_TheBackendSealsItsRows takes a recovery from its start to a
// new password, through the backend, in enforce mode (#337).
func TestRecovery_TheBackendSealsItsRows(t *testing.T) {
	p, db, _ := newRecordEnv(t, rowseal.ModeEnforce)
	p.RecoveryWait = time.Millisecond
	ctx := context.Background()
	hk, contact := recoveryUser(t, p, db, "recovers@example.com")
	event := audit.Event{ActorType: audit.ActorUser, ActorID: hk}

	state, err := p.AdvanceRecovery(ctx, hk, RecoveryProof{ContactID: contact}, standardPlan, event)
	require.NoError(t, err)
	assert.Equal(t, RecoveryWaiting, state.Status, "the only contact is proved")
	time.Sleep(10 * time.Millisecond)
	_, err = p.CompleteRecovery(ctx, hk, contact, "a new strong password", event)
	require.NoError(t, err)
}

// TestRecovery_ChangedInSQLProvesNothing moves a recovery forward in SQL: a
// wait that ended at once. In enforce mode the recovery can neither be read
// nor completed.
func TestRecovery_ChangedInSQLProvesNothing(t *testing.T) {
	p, db, _ := newRecordEnv(t, rowseal.ModeEnforce)
	ctx := context.Background()
	hk, contact := recoveryUser(t, p, db, "skips@example.com")
	event := audit.Event{ActorType: audit.ActorUser, ActorID: hk}
	state, err := p.AdvanceRecovery(ctx, hk, RecoveryProof{ContactID: contact}, standardPlan, event)
	require.NoError(t, err)
	require.Equal(t, RecoveryWaiting, state.Status)

	_, err = db.Exec(`UPDATE account_recoveries SET ready_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, state.ID)
	require.NoError(t, err)
	_, err = p.OpenRecovery(ctx, hk)
	assert.ErrorIs(t, err, rowseal.ErrUnverified)
	_, err = p.CompleteRecovery(ctx, hk, contact, "the attacker's password", event)
	assert.ErrorIs(t, err, rowseal.ErrUnverified, "the wait was not skipped")
	assert.NoError(t, p.Verify(ctx, hk, recordTestPassword), "the password is the owner's")
}
