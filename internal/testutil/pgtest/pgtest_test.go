package pgtest

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLock_LetsAFewProcessesChangeSchemasAtOnce takes every schema slot, and
// expects another session to get none until one is released (#338). Other
// test packages may hold slots meanwhile; each Lock waits for its turn.
func TestLock_LetsAFewProcessesChangeSchemasAtOnce(t *testing.T) {
	unlocks := make([]func(), schemaSlots)
	for i := range unlocks {
		unlocks[i] = Lock(t)
	}
	other := open(t, "postgres")
	defer other.Close()
	ctx := context.Background()
	session, err := other.Conn(ctx)
	require.NoError(t, err)
	defer session.Close()
	for i := int64(0); i < schemaSlots; i++ {
		var taken bool
		require.NoError(t, session.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, schemaLockBase+i).Scan(&taken))
		assert.False(t, taken, "slot %d is held", i)
	}

	// A released slot is taken again; Lock fails the test if no slot comes
	// free within schemaLockWait.
	unlocks[0]()
	Lock(t)()
}

func TestValidName_AcceptsOnlyTestDatabases(t *testing.T) {
	for _, name := range []string{"testdb_x", "testdb_admin_currencies", "testdb_2"} {
		assert.NoError(t, validName(name), name)
	}
	for _, name := range []string{"exchange", "postgres", "testdb", "testdb_", "TESTDB_X", "testdb_x; DROP DATABASE exchange", "testdb-x", "my_testdb_x"} {
		assert.Error(t, validName(name), name)
	}
}

func TestMigrationsURL_PointsAtTheRepositoryMigrations(t *testing.T) {
	url, err := MigrationsURL()
	require.NoError(t, err)
	dir := strings.TrimPrefix(url, "file://")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.NotEmpty(t, entries)
	_, err = os.Stat(dir + "/1_baseline.up.sql")
	assert.NoError(t, err)
}

func TestMigrated_BuildsTheSchemaOfTheMigrations(t *testing.T) {
	conn := Migrated(t, "testdb_pgtest")

	var version int
	var dirty bool
	require.NoError(t, conn.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty))
	assert.GreaterOrEqual(t, version, 1)
	assert.False(t, dirty)

	var index bool
	require.NoError(t, conn.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'user_contacts_value_unique')`).Scan(&index))
	assert.True(t, index, "the users tables have the indexes of the migrations")

	// A second call starts from an empty database again.
	_, err := conn.Exec(`INSERT INTO users DEFAULT VALUES`)
	require.NoError(t, err)
	conn = Migrated(t, "testdb_pgtest")
	var users int
	require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users))
	assert.Zero(t, users)
}
