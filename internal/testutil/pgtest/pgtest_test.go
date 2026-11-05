package pgtest

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	_, err = os.Stat(dir + "/1_create_currencies_table.up.sql")
	assert.NoError(t, err)
}

func TestMigrated_BuildsTheSchemaOfTheMigrations(t *testing.T) {
	conn := Migrated(t, "testdb_pgtest")

	var version int
	var dirty bool
	require.NoError(t, conn.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty))
	assert.GreaterOrEqual(t, version, 13)
	assert.False(t, dirty)

	var constraint bool
	require.NoError(t, conn.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'user_info_contact')`).Scan(&constraint))
	assert.True(t, constraint, "the users table has the constraints of the migrations")

	// A second call starts from an empty database again.
	_, err := conn.Exec(`INSERT INTO users (info) VALUES ('{"contact": {"email": "a@example.com"}}')`)
	require.NoError(t, err)
	conn = Migrated(t, "testdb_pgtest")
	var users int
	require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users))
	assert.Zero(t, users)
}
