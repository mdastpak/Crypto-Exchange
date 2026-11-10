package recovery

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDB = "testdb_recovery"

// latest is the newest migration of the repository.
func latest(t *testing.T) uint {
	t.Helper()
	url, err := pgtest.MigrationsURL()
	require.NoError(t, err)
	version, err := LatestMigration(url)
	require.NoError(t, err)
	return version
}

// failed returns the checks of r that failed, by name.
func failed(r Report) map[string]string {
	out := make(map[string]string)
	for _, c := range r.Checks {
		if !c.OK {
			out[c.Name] = c.Detail
		}
	}
	return out
}

// event records an audit event at the given time.
func event(t *testing.T, db *sql.DB, at time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO audit_events (hk, occurred_at, actor_type, action, outcome)
		VALUES (gen_random_uuid(), $1, 'system', 'test.event', 'success')`, at)
	require.NoError(t, err)
}

func TestLatestMigration_IsTheNewestFile(t *testing.T) {
	assert.GreaterOrEqual(t, latest(t), uint(1), "the baseline is migration 1")
	_, err := LatestMigration("file:///does/not/exist")
	assert.Error(t, err)
}

func TestVerify_PassesOnAFreshDatabase(t *testing.T) {
	db := pgtest.Migrated(t, testDB)
	report, err := Verify(context.Background(), db, Options{Schema: latest(t)})
	require.NoError(t, err, failed(report))
	assert.True(t, report.OK())
	assert.Nil(t, report.NewestEvent)
	assert.Len(t, report.Counts, len(counted))

	var out bytes.Buffer
	require.NoError(t, report.Print(&out))
	assert.Contains(t, out.String(), "ok   schema: version")
	assert.Contains(t, out.String(), "rows audit_events: 0")
}

func TestVerify_FindsWhatARestoreBreaks(t *testing.T) {
	ctx := context.Background()

	t.Run("another schema version", func(t *testing.T) {
		db := pgtest.Migrated(t, testDB)
		report, err := Verify(ctx, db, Options{Schema: latest(t) + 1})
		require.ErrorIs(t, err, ErrNotVerified)
		assert.Contains(t, failed(report)["schema"], "restore with the release that matches the backup")
	})

	t.Run("a dirty migration", func(t *testing.T) {
		db := pgtest.Migrated(t, testDB)
		_, err := db.Exec(`UPDATE schema_migrations SET dirty = true`)
		require.NoError(t, err)
		report, err := Verify(ctx, db, Options{})
		require.ErrorIs(t, err, ErrNotVerified)
		assert.Contains(t, failed(report)["schema"], "dirty")
	})

	t.Run("a constraint that is not validated", func(t *testing.T) {
		db := pgtest.Migrated(t, testDB)
		_, err := db.Exec(`ALTER TABLE currencies ADD CONSTRAINT test_not_valid CHECK (true) NOT VALID`)
		require.NoError(t, err)
		report, err := Verify(ctx, db, Options{})
		require.ErrorIs(t, err, ErrNotVerified)
		assert.Contains(t, failed(report)["constraints"], "currencies.test_not_valid")
	})

	t.Run("an invalid index", func(t *testing.T) {
		db := pgtest.Migrated(t, testDB)
		_, err := db.Exec(`CREATE INDEX test_invalid ON currencies (hk)`)
		require.NoError(t, err)
		// What an interrupted CREATE INDEX CONCURRENTLY leaves.
		_, err = db.Exec(`UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'test_invalid'::regclass`)
		require.NoError(t, err)
		report, err := Verify(ctx, db, Options{})
		require.ErrorIs(t, err, ErrNotVerified)
		assert.Contains(t, failed(report)["indexes"], "test_invalid")
	})

	t.Run("an audit log that is no longer append-only", func(t *testing.T) {
		db := pgtest.Migrated(t, testDB)
		_, err := db.Exec(`ALTER TABLE audit_events DISABLE TRIGGER audit_events_no_truncate`)
		require.NoError(t, err)
		report, err := Verify(ctx, db, Options{})
		require.ErrorIs(t, err, ErrNotVerified)
		assert.Contains(t, failed(report)["audit log"], "audit_events_no_truncate")
		assert.NotContains(t, failed(report)["audit log"], "audit_events_no_update_or_delete")
	})

	t.Run("data keys that do not unwrap", func(t *testing.T) {
		db := pgtest.Migrated(t, testDB)
		report, err := Verify(ctx, db, Options{Keys: func(context.Context) (int, error) {
			return 0, errors.New("unwrapping data key row-mac/v1: wrong KEK")
		}})
		require.ErrorIs(t, err, ErrNotVerified)
		assert.Contains(t, failed(report)["data keys"], "wrong KEK")

		report, err = Verify(ctx, db, Options{Keys: func(context.Context) (int, error) { return 3, nil }})
		require.NoError(t, err)
		assert.Equal(t, "3 keys unwrap with the configured KEK", report.Checks[len(report.Checks)-1].Detail)
	})
}

// TestVerify_ChecksThePointInTime restores, in effect, to a point between
// two changes, and checks both bounds of the target.
func TestVerify_ChecksThePointInTime(t *testing.T) {
	ctx := context.Background()
	db := pgtest.Migrated(t, testDB)
	goodChange := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	badChange := goodChange.Add(time.Minute)
	event(t, db, goodChange)

	report, err := Verify(ctx, db, Options{NotBefore: &goodChange, NotAfter: &badChange})
	require.NoError(t, err, failed(report))
	require.NotNil(t, report.NewestEvent)
	assert.Equal(t, goodChange, *report.NewestEvent)

	later := goodChange.Add(time.Second)
	report, err = Verify(ctx, db, Options{NotBefore: &later})
	require.ErrorIs(t, err, ErrNotVerified)
	assert.Contains(t, failed(report)["point in time"], "stopped too early")

	event(t, db, badChange.Add(time.Second))
	report, err = Verify(ctx, db, Options{NotAfter: &badChange})
	require.ErrorIs(t, err, ErrNotVerified)
	assert.Contains(t, failed(report)["point in time"], "went past its target")
}

// TestReadArchiving_ReportsTheServer reads the archiver of the test server,
// which does not archive: the check must say so rather than pass.
func TestReadArchiving_ReportsTheServer(t *testing.T) {
	db := pgtest.Migrated(t, testDB)
	a, err := ReadArchiving(context.Background(), db)
	require.NoError(t, err)
	assert.False(t, a.Now.IsZero())
	if a.Mode == "off" {
		problems := a.Problems(5 * time.Minute)
		require.Len(t, problems, 1)
		assert.Contains(t, problems[0], "archive_mode is off")
	}
	var out bytes.Buffer
	require.NoError(t, a.Report(&out))
	assert.Contains(t, out.String(), "archive_mode:")
}

func TestArchiving_Problems(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	at := func(ago time.Duration) *time.Time { t := now.Add(-ago); return &t }
	rpo := 5 * time.Minute
	healthy := Archiving{Mode: "on", Timeout: time.Minute, Archived: 10, LastArchived: at(30 * time.Second), LastWAL: "000000010000000000000010", Now: now}

	assert.Empty(t, healthy.Problems(rpo))

	cases := map[string]struct {
		change func(*Archiving)
		want   string
	}{
		"no timeout":             {func(a *Archiving) { a.Timeout = 0 }, "archive_timeout is 0s"},
		"a timeout over the RPO": {func(a *Archiving) { a.Timeout = 10 * time.Minute }, "at most the RPO"},
		"never archived":         {func(a *Archiving) { a.LastArchived = nil }, "no WAL segment was archived"},
		"archived too long ago":  {func(a *Archiving) { a.LastArchived = at(6 * time.Minute) }, "6m0s old"},
		"failing since the last success": {func(a *Archiving) {
			a.LastFailed, a.Failed = at(10*time.Second), 3
		}, "has not succeeded since (3 failures)"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := healthy
			tc.change(&a)
			problems := a.Problems(rpo)
			require.Len(t, problems, 1, problems)
			assert.Contains(t, problems[0], tc.want)
		})
	}

	recovered := healthy
	recovered.LastFailed, recovered.Failed = at(time.Minute), 1
	assert.Empty(t, recovered.Problems(rpo), "a failure followed by a success is over")
}

func TestParseSetting(t *testing.T) {
	for value, want := range map[string]time.Duration{
		"0": 0, "60": time.Minute, "60s": time.Minute, "5min": 5 * time.Minute,
		"1h": time.Hour, "1d": 24 * time.Hour, "500ms": 500 * time.Millisecond,
	} {
		got, err := parseSetting(value)
		require.NoError(t, err, value)
		assert.Equal(t, want, got, value)
	}
	for _, value := range []string{"", "soon", "-1s"} {
		_, err := parseSetting(value)
		assert.Error(t, err, value)
	}
}
