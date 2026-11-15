package verification

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// operatorOrigin is the origin of the changes of an operator with
// cmd/admin.
var operatorOrigin = audit.Operator("cmd/admin verification grant", "operator-1", "", "")

func TestTierOf_GivesTheLevelBack(t *testing.T) {
	for _, level := range []Level{Level0, Level1, Level2} {
		tier, status, err := TierOf(level)
		require.NoError(t, err)
		assert.Equal(t, level, LevelOf(tier, status), "level %d", level)
	}
	for _, level := range []Level{Level0 - 1, Level2 + 1} {
		_, _, err := TierOf(level)
		require.ErrorIs(t, err, ErrInvalidLevel)
	}
}

// setLevel sets the level of the user in a transaction of its own.
func (f fixture) setLevel(userHK string, level Level, reason string) (Change, error) {
	f.t.Helper()
	tx, err := f.DB.BeginTx(context.Background(), nil)
	require.NoError(f.t, err)
	defer func() { _ = tx.Rollback() }()
	change, err := SetLevel(context.Background(), tx, userHK, level, reason, operatorOrigin)
	if err != nil {
		return Change{}, err
	}
	return change, tx.Commit()
}

// levelEvent is an audit event of a change of level.
type levelEvent struct{ actor, agent, changes string }

func (f fixture) levelEvents(userHK string) []levelEvent {
	f.t.Helper()
	rows, err := f.DB.Query(`SELECT actor_type, COALESCE(user_agent, ''), changes::text FROM audit_events
		WHERE action = $1 AND target_type = $2 AND target_id = $3 ORDER BY hk`,
		audit.ActionUserVerificationSet, audit.TargetUser, userHK)
	require.NoError(f.t, err)
	defer rows.Close()
	var events []levelEvent
	for rows.Next() {
		var e levelEvent
		require.NoError(f.t, rows.Scan(&e.actor, &e.agent, &e.changes))
		events = append(events, e)
	}
	require.NoError(f.t, rows.Err())
	return events
}

// TestSetLevel_GrantsAndLowersALevel raises a new user to level 2, as the
// testnet run needs, and lowers them to level 1. Each change is sealed,
// so the level counts in enforce mode, applies from the next check, and
// is audited with the operator and the reason. The same level again
// writes and records nothing.
func TestSetLevel_GrantsAndLowersALevel(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	sealtest.Use(t, f.DB, rowseal.ModeEnforce)
	alice := f.user(shared.KYCTierNone, shared.KYCStatusPending)
	r := refusalOf(t, f.Check(ctx, f.DB, alice, DepositCrypto))
	assert.Equal(t, Level0, r.Level)

	change, err := f.setLevel(alice, Level2, "  testnet run  ")
	require.NoError(t, err)
	assert.True(t, change.Changed())
	assert.Equal(t, Status{Level: Level0, Tier: shared.KYCTierNone, Status: shared.KYCStatusPending}, change.Before)
	assert.Equal(t, Status{Level: Level2, Tier: shared.KYCTierStandard, Status: shared.KYCStatusApproved}, change.After)
	st, err := f.Level(ctx, f.DB, alice)
	require.NoError(t, err)
	assert.Equal(t, Level2, st.Level, "the row is sealed again, so the level counts in enforce mode")
	require.NoError(t, f.Check(ctx, f.DB, alice, DepositCrypto))
	require.NoError(t, f.Check(ctx, f.DB, alice, WithdrawCrypto))

	again, err := f.setLevel(alice, Level2, "again")
	require.NoError(t, err)
	assert.False(t, again.Changed(), "the user has the level already")

	_, err = f.setLevel(alice, Level1, "the video check was revoked")
	require.NoError(t, err)
	r = refusalOf(t, f.Check(ctx, f.DB, alice, WithdrawCrypto))
	assert.Equal(t, Level1, r.Level, "a lower level applies from the next request")
	require.NoError(t, f.Check(ctx, f.DB, alice, DepositCrypto))

	events := f.levelEvents(alice)
	require.Len(t, events, 2, "two changes; the same level again records nothing")
	for _, e := range events {
		assert.Equal(t, audit.ActorSystem, e.actor)
		assert.Contains(t, e.agent, "operator=operator-1")
	}
	assert.JSONEq(t, `{"before": {"level": 0, "kyc_tier": "NONE", "kyc_status": "PENDING"},
		"after": {"level": 2, "kyc_tier": "STANDARD", "kyc_status": "APPROVED", "reason": "testnet run"}}`, events[0].changes)
	assert.JSONEq(t, `{"before": {"level": 2, "kyc_tier": "STANDARD", "kyc_status": "APPROVED"},
		"after": {"level": 1, "kyc_tier": "BASIC", "kyc_status": "APPROVED", "reason": "the video check was revoked"}}`, events[1].changes)

	// A tier that expired is level 0; granting the level renews it.
	bob := f.user(shared.KYCTierStandard, shared.KYCStatusExpired)
	change, err = f.setLevel(bob, Level2, "renewed after review")
	require.NoError(t, err)
	assert.True(t, change.Changed())
	assert.Equal(t, Level0, change.Before.Level)
}

// TestSetLevel_RefusesWhatItMustNotChange: a user row changed in SQL, an
// account that is not an active user, an unknown or deleted one, a level
// out of range, and a missing reason are refused, and nothing is written
// or recorded.
func TestSetLevel_RefusesWhatItMustNotChange(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	sealtest.Use(t, f.DB, rowseal.ModeEnforce)

	// A level raised in SQL is not built on.
	tampered := f.user(shared.KYCTierNone, shared.KYCStatusPending)
	_, err := f.DB.Exec(`UPDATE users SET kyc_tier = 'STANDARD', kyc_status = 'APPROVED' WHERE hk = $1`, tampered)
	require.NoError(t, err)
	_, err = f.setLevel(tampered, Level1, "review")
	require.ErrorIs(t, err, rowseal.ErrUnverified)
	var tier string
	require.NoError(t, f.DB.QueryRow(`SELECT kyc_tier FROM users WHERE hk = $1`, tampered).Scan(&tier))
	assert.Equal(t, "STANDARD", tier, "the row is left as it was found, with its finding")
	st, err := f.Level(ctx, f.DB, tampered)
	require.NoError(t, err)
	assert.Equal(t, Level0, st.Level)

	other := func(status, role string, deleted bool) string {
		hk := f.user(shared.KYCTierNone, shared.KYCStatusPending)
		_, err := f.DB.Exec(`UPDATE users SET status = $2, role = $3, deleted_at = CASE WHEN $4 THEN NOW() END WHERE hk = $1`,
			hk, status, role, deleted)
		require.NoError(t, err)
		require.NoError(t, rowseal.Reseal(ctx, f.DB, rowseal.Users, "hk = $1", hk))
		return hk
	}
	suspended := other("SUSPENDED", "user", false)
	_, err = f.setLevel(suspended, Level1, "review")
	require.ErrorIs(t, err, ErrInactive)
	admin := other("ACTIVE", "admin", false)
	_, err = f.setLevel(admin, Level1, "review")
	require.ErrorIs(t, err, ErrNotAUser)
	deleted := other("ACTIVE", "user", true)
	_, err = f.setLevel(deleted, Level1, "review")
	require.ErrorIs(t, err, ErrUserNotFound)
	for _, id := range []string{shared.NewUUID().String(), "not-an-id", ""} {
		_, err = f.setLevel(id, Level1, "review")
		require.ErrorIs(t, err, ErrUserNotFound, id)
	}

	alice := f.user(shared.KYCTierNone, shared.KYCStatusPending)
	for _, level := range []Level{Level0 - 1, Level2 + 1} {
		_, err = f.setLevel(alice, level, "review")
		require.ErrorIs(t, err, ErrInvalidLevel)
	}
	for _, reason := range []string{"", "   ", strings.Repeat("x", MaxReason+1)} {
		_, err = f.setLevel(alice, Level1, reason)
		require.ErrorIs(t, err, ErrReasonRequired)
	}
	_, err = f.setLevel(alice, Level1, strings.Repeat("ر", MaxReason))
	require.NoError(t, err, "the bound counts characters, not bytes")

	for _, hk := range []string{tampered, suspended, admin, deleted} {
		assert.Empty(t, f.levelEvents(hk), hk)
	}
}

// TestSetLevel_WaitsForAnOperationInFlight: an operation holds a share
// lock of the user (restrictions.Lock) while it checks the level and moves
// funds. A change of level waits for it, so the operation runs on the
// level it read, and the next one reads the new level.
func TestSetLevel_WaitsForAnOperationInFlight(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	sealtest.Use(t, f.DB, rowseal.ModeEnforce)
	alice := f.user(shared.KYCTierStandard, shared.KYCStatusApproved)

	op, err := f.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = op.Rollback() }()
	_, err = op.ExecContext(ctx, `SELECT 1 FROM users WHERE hk = $1 FOR SHARE`, alice)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		tx, err := f.DB.BeginTx(ctx, nil)
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := SetLevel(ctx, tx, alice, Level0, "lowered while a withdrawal is in flight", operatorOrigin); err != nil {
			done <- err
			return
		}
		done <- tx.Commit()
	}()
	select {
	case err := <-done:
		t.Fatalf("the change did not wait for the operation: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	require.NoError(t, f.Check(ctx, op, alice, WithdrawCrypto), "the operation keeps the level it read")
	require.NoError(t, op.Commit())

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the change did not finish after the operation committed")
	}
	r := refusalOf(t, f.Check(ctx, f.DB, alice, WithdrawCrypto))
	assert.Equal(t, Level0, r.Level, "the next operation reads the new level")
}
