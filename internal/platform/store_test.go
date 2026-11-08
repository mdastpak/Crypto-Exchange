package platform_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDBName = "testdb_platform"

var ctx = context.Background()

func origin(admin string) audit.Event {
	if admin == "" {
		return audit.Operator("cmd/admin platform", "tester", "", "")
	}
	return audit.Event{ActorType: audit.ActorAdmin, ActorID: admin}
}

func create(t *testing.T, db *sql.DB, n platform.New) *platform.Control {
	t.Helper()
	if n.Reason == "" {
		n.Reason = "test"
	}
	c, err := platform.Create(ctx, db, n, origin(n.CreatedBy))
	require.NoError(t, err)
	return c
}

func version(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	v, err := platform.Version(ctx, db)
	require.NoError(t, err)
	return v
}

func auditActions(t *testing.T, db *sql.DB, target string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT action FROM audit_events WHERE target_type = 'platform_control' AND target_id = $1 ORDER BY hk`, target)
	require.NoError(t, err)
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var a string
		require.NoError(t, rows.Scan(&a))
		actions = append(actions, a)
	}
	return actions
}

func locked(t *testing.T, err error) *platform.Locked {
	t.Helper()
	var l *platform.Locked
	require.True(t, errors.As(err, &l), "want a refusal, got %v", err)
	return l
}

// check runs platform.Check in a transaction of its own.
func check(t *testing.T, db *sql.DB, use platform.Feature, s platform.Subject) error {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	return platform.Check(ctx, tx, use, s)
}

func TestStore_CreateUpdateLift(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	admin := uuid.NewString()
	before := version(t, db)

	end := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	c := create(t, db, platform.New{Kind: platform.KindMaintenance, EndsAt: &end, CreatedBy: admin,
		Message: platform.Message{FA: "نگهداری", EN: "Maintenance"}, AllowNetworks: []string{"10.0.0.1"},
		AllowUsers: []string{uuid.NewString()}})
	assert.True(t, c.Active)
	assert.Nil(t, c.Feature)
	assert.Equal(t, []string{"10.0.0.1/32"}, c.AllowNetworks)
	assert.Equal(t, admin, *c.CreatedBy)
	assert.Equal(t, int64(1), c.Version)
	assert.Greater(t, version(t, db), before, "a change raises the state version")

	t.Run("the end and message change, with a version check", func(t *testing.T) {
		stale := int64(7)
		_, err := platform.Update(ctx, db, c.HK, platform.Changes{EndsAt: &end}, &stale, origin(admin))
		var conflict *platform.VersionConflict
		require.ErrorAs(t, err, &conflict)
		assert.Equal(t, int64(1), conflict.Current)

		later := end.Add(time.Hour)
		u, err := platform.Update(ctx, db, c.HK, platform.Changes{EndsAt: &later, Message: &platform.Message{EN: "Longer"}}, &c.Version, origin(admin))
		require.NoError(t, err)
		assert.True(t, later.Equal(*u.EndsAt))
		assert.Equal(t, "Longer", u.Message.EN)
		assert.Empty(t, u.Message.FA)
		assert.Equal(t, int64(2), u.Version)

		_, err = platform.Update(ctx, db, c.HK, platform.Changes{}, nil, origin(admin))
		assert.ErrorAs(t, err, new(platform.Invalid))
	})

	t.Run("a lift ends it, once", func(t *testing.T) {
		_, err := platform.Lift(ctx, db, platform.LiftRequest{HK: c.HK, By: admin}, origin(admin))
		assert.ErrorAs(t, err, new(platform.Invalid), "a lift needs a note")
		r, err := platform.Lift(ctx, db, platform.LiftRequest{HK: c.HK, By: admin, Note: "done"}, origin(admin))
		require.NoError(t, err)
		assert.False(t, r.Pending)
		assert.False(t, r.Control.Active)
		assert.Equal(t, "done", r.Control.LiftNote)
		_, err = platform.Lift(ctx, db, platform.LiftRequest{HK: c.HK, By: admin, Note: "again"}, origin(admin))
		assert.ErrorIs(t, err, platform.ErrEnded)
		_, err = platform.Update(ctx, db, c.HK, platform.Changes{Message: &platform.Message{EN: "x"}}, nil, origin(admin))
		assert.ErrorIs(t, err, platform.ErrEnded)
	})

	t.Run("unknown controls are not found", func(t *testing.T) {
		_, err := platform.Lift(ctx, db, platform.LiftRequest{HK: uuid.NewString(), Note: "x"}, origin(""))
		assert.ErrorIs(t, err, platform.ErrNotFound)
		_, err = platform.Get(ctx, db, "not-a-uuid")
		assert.ErrorIs(t, err, platform.ErrNotFound)
	})

	assert.Equal(t, []string{audit.ActionPlatformControlCreate, audit.ActionPlatformControlUpdate, audit.ActionPlatformControlLift},
		auditActions(t, db, c.HK))
}

func TestStore_LiftApproval(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	first, second := uuid.NewString(), uuid.NewString()
	approval := []platform.Feature{platform.FeatureWithdrawals}
	c := create(t, db, platform.New{Kind: platform.KindLockdown, Feature: platform.FeatureWithdrawals, CreatedBy: first})
	lift := func(by string) (platform.LiftResult, error) {
		return platform.Lift(ctx, db, platform.LiftRequest{HK: c.HK, By: by, Note: "checked", Approval: approval}, origin(by))
	}

	v := version(t, db)
	r, err := lift(first)
	require.NoError(t, err)
	assert.True(t, r.Pending)
	assert.Equal(t, first, *r.Control.LiftRequestedBy)
	assert.True(t, r.Control.Active, "the lockdown applies until the approval")
	assert.Equal(t, v, version(t, db), "a request changes nothing for users")

	_, err = lift(first)
	assert.ErrorIs(t, err, platform.ErrSameApprover)
	r, err = lift(second)
	require.NoError(t, err)
	assert.False(t, r.Pending)
	assert.Equal(t, second, *r.Control.LiftedBy)

	t.Run("a scheduled lockdown is cancelled without approval", func(t *testing.T) {
		start := time.Now().Add(time.Hour)
		s := create(t, db, platform.New{Kind: platform.KindLockdown, Feature: platform.FeatureWithdrawals, StartsAt: &start, CreatedBy: first})
		r, err := platform.Lift(ctx, db, platform.LiftRequest{HK: s.HK, By: first, Note: "not needed", Approval: approval}, origin(first))
		require.NoError(t, err)
		assert.False(t, r.Pending)
	})

	t.Run("cmd/admin lifts at once", func(t *testing.T) {
		l := create(t, db, platform.New{Kind: platform.KindLockdown, Feature: platform.FeatureWithdrawals})
		assert.Nil(t, l.CreatedBy)
		r, err := platform.Lift(ctx, db, platform.LiftRequest{HK: l.HK, Note: "operator", Approval: approval}, origin(""))
		require.NoError(t, err)
		assert.False(t, r.Pending)
		assert.Nil(t, r.Control.LiftedBy)
		assert.NotNil(t, r.Control.LiftedAt)
	})

	assert.Equal(t, []string{audit.ActionPlatformControlCreate, audit.ActionPlatformControlLiftRequest, audit.ActionPlatformControlLift},
		auditActions(t, db, c.HK))
}

func TestCheck(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	require.NoError(t, check(t, db, platform.FeatureWithdrawals, platform.Subject{}))

	c := create(t, db, platform.New{Kind: platform.KindLockdown, Feature: platform.FeatureWithdrawals})
	l := locked(t, check(t, db, platform.FeatureWithdrawals, platform.Subject{}))
	assert.Equal(t, platform.CodeFeatureLocked, l.Code())
	require.NoError(t, check(t, db, platform.FeatureDeposits, platform.Subject{}))
	_, err := platform.Lift(ctx, db, platform.LiftRequest{HK: c.HK, Note: "x"}, origin(""))
	require.NoError(t, err)
	require.NoError(t, check(t, db, platform.FeatureWithdrawals, platform.Subject{}))

	t.Run("a scheduled control applies from its start", func(t *testing.T) {
		start := time.Now().Add(400 * time.Millisecond)
		create(t, db, platform.New{Kind: platform.KindLockdown, Feature: platform.FeatureTrading, StartsAt: &start})
		require.NoError(t, check(t, db, platform.FeatureTrading, platform.Subject{}))
		time.Sleep(time.Until(start) + 50*time.Millisecond)
		_ = locked(t, check(t, db, platform.FeatureTrading, platform.Subject{}))
	})

	t.Run("a maintenance lets its allowed networks through", func(t *testing.T) {
		m := create(t, db, platform.New{Kind: platform.KindMaintenance, AllowNetworks: []string{"192.0.2.0/24"}})
		l := locked(t, check(t, db, platform.FeatureSignUp, platform.Subject{IP: "198.51.100.1"}))
		assert.Equal(t, platform.CodeMaintenance, l.Code())
		require.NoError(t, check(t, db, platform.FeatureSignUp, platform.Subject{IP: "192.0.2.10"}))
		_, err := platform.Lift(ctx, db, platform.LiftRequest{HK: m.HK, Note: "x"}, origin(""))
		require.NoError(t, err)
	})
}

// TestCheck_AnOperationInFlightCommitsBeforeTheLockdown starts an operation
// that checked withdrawals, creates a lockdown meanwhile, and expects the
// lockdown to wait for the operation, and the next operation to be refused:
// no withdrawal commits after the lockdown.
func TestCheck_AnOperationInFlightCommitsBeforeTheLockdown(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, platform.Check(ctx, tx, platform.FeatureWithdrawals, platform.Subject{}))

	var created atomic.Bool
	done := make(chan error, 1)
	go func() {
		_, err := platform.Create(ctx, db, platform.New{Kind: platform.KindLockdown, Feature: platform.FeatureWithdrawals, Reason: "incident"}, origin(""))
		created.Store(true)
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	assert.False(t, created.Load(), "the lockdown waits for the operation in flight")
	require.NoError(t, tx.Commit())
	require.NoError(t, <-done)
	_ = locked(t, check(t, db, platform.FeatureWithdrawals, platform.Subject{}))
}

func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// TestWatcher_EveryInstanceAppliesAChange runs two watchers, as two
// instances, and expects a change made on neither to apply on both within
// the poll interval, and a scheduled control to start on both, with a
// status event each.
func TestWatcher_EveryInstanceAppliesAChange(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	opts := platform.Options{PollInterval: 100 * time.Millisecond, StaleAfter: time.Minute, Logger: quietLogger()}
	type instance struct {
		w        *platform.Watcher
		mu       sync.Mutex
		statuses []platform.Status
	}
	instances := []*instance{{w: platform.NewWatcher(db, opts)}, {w: platform.NewWatcher(db, opts)}}
	for _, in := range instances {
		in := in
		in.w.OnChange(func(s platform.Status) {
			in.mu.Lock()
			defer in.mu.Unlock()
			in.statuses = append(in.statuses, s)
		})
		in.w.Start(ctx)
		t.Cleanup(func() { _ = in.w.Stop(ctx) })
	}
	eventually := func(cond func(*instance) bool, msg string) {
		t.Helper()
		for _, in := range instances {
			require.Eventually(t, func() bool { return cond(in) }, 2*time.Second, 20*time.Millisecond, msg)
		}
	}

	c := create(t, db, platform.New{Kind: platform.KindLockdown, Feature: platform.FeatureWithdrawals})
	eventually(func(in *instance) bool { return in.w.Locked(platform.FeatureWithdrawals, platform.Subject{}) != nil },
		"every instance applies a new lockdown within the poll interval")
	_, err := platform.Lift(ctx, db, platform.LiftRequest{HK: c.HK, Note: "x"}, origin(""))
	require.NoError(t, err)
	eventually(func(in *instance) bool { return in.w.Locked(platform.FeatureWithdrawals, platform.Subject{}) == nil },
		"every instance applies a lift")

	start := time.Now().Add(500 * time.Millisecond)
	end := start.Add(time.Hour)
	create(t, db, platform.New{Kind: platform.KindMaintenance, StartsAt: &start, EndsAt: &end, Message: platform.Message{EN: "Soon"}})
	eventually(func(in *instance) bool {
		s := in.w.Status()
		return len(s.Scheduled) == 1 && s.Mode == platform.ModeOpen
	}, "the maintenance is announced before it starts")
	eventually(func(in *instance) bool { return in.w.Status().Mode == platform.ModeMaintenance },
		"the maintenance starts on time, without a change in the database")
	eventually(func(in *instance) bool {
		in.mu.Lock()
		defer in.mu.Unlock()
		return len(in.statuses) > 0 && in.statuses[len(in.statuses)-1].Mode == platform.ModeMaintenance
	}, "the listeners hear of the start within the poll interval")
	for _, in := range instances {
		assert.NotNil(t, in.w.Locked(platform.UseService, platform.Subject{}))
		assert.False(t, in.w.Paused(platform.FeatureScheduler), "a maintenance pauses no background work")
	}
}

// TestWatcher_FailsClosedWhileTheControlsAreUnknown closes the database
// under a watcher and expects the fail-closed features to close after
// StaleAfter, and to stay open before it.
func TestWatcher_FailsClosedWhileTheControlsAreUnknown(t *testing.T) {
	pgtest.Migrated(t, testDBName)
	db, err := sql.Open("pgx", pgtest.DSN(testDBName))
	require.NoError(t, err)
	w := platform.NewWatcher(db, platform.Options{PollInterval: 50 * time.Millisecond, StaleAfter: 300 * time.Millisecond,
		FailClosed: []platform.Feature{platform.FeatureWithdrawals}, Logger: quietLogger()})
	w.Start(ctx)
	t.Cleanup(func() { _ = w.Stop(ctx) })
	assert.Nil(t, w.Locked(platform.FeatureWithdrawals, platform.Subject{}))

	require.NoError(t, db.Close())
	assert.Nil(t, w.Locked(platform.FeatureWithdrawals, platform.Subject{}), "the last controls apply for a while")
	require.Eventually(t, func() bool {
		l := w.Locked(platform.FeatureWithdrawals, platform.Subject{})
		return l != nil && l.StateUnknown
	}, 2*time.Second, 20*time.Millisecond)
	assert.Nil(t, w.Locked(platform.FeatureTrading, platform.Subject{}), "only the fail-closed features close")
}

func TestWatcher_StartsClosedWhenTheFirstReadFails(t *testing.T) {
	pgtest.Migrated(t, testDBName)
	db, err := sql.Open("pgx", pgtest.DSN(testDBName))
	require.NoError(t, err)
	require.NoError(t, db.Close())
	w := platform.NewWatcher(db, platform.Options{Logger: quietLogger()})
	w.Start(ctx)
	t.Cleanup(func() { _ = w.Stop(ctx) })
	for _, f := range platform.DefaultFailClosed {
		assert.NotNil(t, w.Locked(f, platform.Subject{}), "%s is closed until a read succeeds", f)
	}
	assert.Nil(t, w.Locked(platform.FeatureSignIn, platform.Subject{}))
}

func TestWatcher_PausesBackgroundWork(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	w := platform.NewWatcher(db, platform.Options{Logger: quietLogger()})
	w.Start(ctx)
	t.Cleanup(func() { _ = w.Stop(ctx) })
	c := create(t, db, platform.New{Kind: platform.KindLockdown, Feature: platform.FeatureOutbox})
	require.NoError(t, w.Refresh(ctx))
	assert.True(t, w.Paused(platform.FeatureOutbox))
	assert.False(t, w.Paused(platform.FeatureScheduler))
	assert.Empty(t, w.Status().Lockdowns, "users never see a lockdown of background work")
	_, err := platform.Lift(ctx, db, platform.LiftRequest{HK: c.HK, Note: "x"}, origin(""))
	require.NoError(t, err)
	require.NoError(t, w.Refresh(ctx))
	assert.False(t, w.Paused(platform.FeatureOutbox))
}

func TestList(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	a := create(t, db, platform.New{Kind: platform.KindLockdown, Feature: platform.FeatureSignUp})
	b := create(t, db, platform.New{Kind: platform.KindLockdown, Feature: platform.FeatureSignIn})
	list, err := platform.List(ctx, db, 0)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, []string{b.HK, a.HK}, []string{list[0].HK, list[1].HK}, "newest first")
	open, err := platform.Open(ctx, db)
	require.NoError(t, err)
	assert.Len(t, open, 2)
}
