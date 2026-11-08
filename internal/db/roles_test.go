package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The roles tests (#180) use a database of their own, and a runtime role
// that they create once in the cluster and keep: a role that has
// privileges in some database cannot simply be dropped, and the schema
// reset of each test removes every privilege it had here.
const (
	rolesTestDBName   = "testdb_roles"
	rolesTestRole     = "testdb_roles_runtime"
	rolesTestPassword = "runtime-test"
)

// ensureRuntimeRole creates the test's runtime role if it does not exist.
func ensureRuntimeRole(t *testing.T) {
	t.Helper()
	admin, err := sql.Open("pgx", pgtest.DSN("postgres"))
	require.NoError(t, err)
	defer admin.Close()
	_, err = admin.Exec(fmt.Sprintf(`DO $$ BEGIN
		CREATE ROLE %s LOGIN PASSWORD '%s';
	EXCEPTION WHEN duplicate_object THEN NULL;
	END $$`, rolesTestRole, rolesTestPassword))
	require.NoError(t, err)
}

// withRoles configures the two roles against the empty roles database:
// postgres as the migration role, and the test's runtime role as
// postgres.user, in the given migrations mode.
func withRoles(t *testing.T, mode string) {
	t.Helper()
	ensureRuntimeRole(t)
	pgtest.Reset(t, rolesTestDBName)

	saved, savedPath := config.Config, config.MigrationsPath()
	t.Cleanup(func() {
		config.Config = saved
		config.SetMigrationsPath(savedPath)
	})
	source, err := pgtest.MigrationsURL()
	require.NoError(t, err)
	config.SetMigrationsPath(source)

	config.Config = config.Configuration{}
	pg := &config.Config.Postgres
	pg.Host, pg.Port, pg.DBName, pg.SSLMode = testPGHost, testPGPort, rolesTestDBName, testPGSSLMode
	pg.User, pg.Password = rolesTestRole, rolesTestPassword
	pg.Migrations.Mode = mode
	pg.Migrations.User, pg.Migrations.Password = testPGUser, testPGPassword
	pg.MaxOpenConns, pg.MaxIdleConns = 4, 1
	pg.ConnectRetries, pg.ConnectRetryDelayMs = 1, 10
	pg.MigrationLockTimeoutSeconds = 30
}

// openAs connects to the roles database as user.
func openAs(t *testing.T, user, password string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("pgx", fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		testPGHost, testPGPort, user, password, rolesTestDBName, testPGSSLMode))
	require.NoError(t, err)
	require.NoError(t, conn.Ping())
	t.Cleanup(func() { conn.Close() })
	return conn
}

// requireDenied checks that statement fails for lack of privilege.
func requireDenied(t *testing.T, conn *sql.DB, statement string) {
	t.Helper()
	_, err := conn.Exec(statement)
	require.Error(t, err, statement)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "%s: %v", statement, err)
	assert.Equal(t, "42501", pgErr.Code, "%s must fail with insufficient_privilege, not: %v", statement, err)
}

// TestRuntimeRole_CannotChangeTheSchema is the acceptance test of #180: with
// the privileges that the migrations grant it, the runtime role reads and
// writes rows, inserts into the audit log, and is refused everything that
// changes the schema or the audit log.
func TestRuntimeRole_CannotChangeTheSchema(t *testing.T) {
	withRoles(t, config.MigrationsModeExternal)
	require.NoError(t, Migrate(context.Background()))
	runtime := openAs(t, rolesTestRole, rolesTestPassword)

	_, err := runtime.Exec(`INSERT INTO audit_events (hk, actor_type, action, outcome) VALUES ($1, 'system', 'test.roles', 'success')`, uuid.New())
	require.NoError(t, err, "the server writes the audit log, with the sequence of its id")
	var count int
	require.NoError(t, runtime.QueryRow(`SELECT count(*) FROM audit_events`).Scan(&count))
	assert.Equal(t, 1, count)
	var version int
	require.NoError(t, runtime.QueryRow(`SELECT version FROM schema_migrations`).Scan(&version), "the version check reads schema_migrations")

	var canRead, canWrite bool
	require.NoError(t, runtime.QueryRow(`SELECT has_table_privilege('users', 'SELECT'),
		has_table_privilege('users', 'INSERT') AND has_table_privilege('users', 'UPDATE') AND has_table_privilege('users', 'DELETE')`).Scan(&canRead, &canWrite))
	assert.True(t, canRead)
	assert.True(t, canWrite, "the other tables are read and written as before")

	for _, statement := range []string{
		`ALTER TABLE audit_events ADD COLUMN note TEXT`,
		`ALTER TABLE users DROP CONSTRAINT IF EXISTS users_pkey`,
		`DROP TRIGGER audit_events_no_update_or_delete ON audit_events`,
		`DROP TABLE audit_events`,
		`TRUNCATE audit_events`,
		`TRUNCATE users`,
		`UPDATE audit_events SET outcome = 'failure'`,
		`DELETE FROM audit_events`,
		`UPDATE schema_migrations SET dirty = true`,
		`CREATE TABLE intruder (id INT)`,
		`CREATE FUNCTION intruder() RETURNS INT LANGUAGE sql AS 'SELECT 1'`,
		`CREATE SCHEMA intruder`,
		`CREATE INDEX intruder ON users (created_at)`,
	} {
		requireDenied(t, runtime, statement)
	}

	problems, err := RoleProblems(context.Background(), runtime)
	require.NoError(t, err)
	assert.Empty(t, problems, "the granted role is a proper runtime role")
}

// TestRoleProblems_NamesWhatTheRoleCouldChange checks the release check on
// the owner of the schema, and on a runtime role that was given too much by
// hand, which the next migration takes back.
func TestRoleProblems_NamesWhatTheRoleCouldChange(t *testing.T) {
	withRoles(t, config.MigrationsModeExternal)
	ctx := context.Background()
	require.NoError(t, Migrate(ctx))

	owner := openAs(t, testPGUser, testPGPassword)
	problems, err := RoleProblems(ctx, owner)
	require.NoError(t, err)
	joined := strings.Join(problems, "\n")
	assert.Contains(t, joined, "the role postgres: it has the SUPERUSER attribute")
	assert.Contains(t, joined, "it owns tables, such as")
	assert.Contains(t, joined, "it can UPDATE or DELETE rows of the append-only table audit_events")

	for _, grant := range []string{
		`GRANT TRUNCATE ON users TO ` + rolesTestRole,
		`GRANT UPDATE ON audit_events TO ` + rolesTestRole,
		`GRANT CREATE ON SCHEMA public TO ` + rolesTestRole,
	} {
		_, err := owner.Exec(grant)
		require.NoError(t, err)
	}
	runtime := openAs(t, rolesTestRole, rolesTestPassword)
	problems, err = RoleProblems(ctx, runtime)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"the role " + rolesTestRole + ": it can create objects in the schema public",
		"the role " + rolesTestRole + ": it has TRUNCATE, REFERENCES, or TRIGGER on tables, such as users",
		"the role " + rolesTestRole + ": it can UPDATE or DELETE rows of the append-only table audit_events",
	}, problems)

	require.NoError(t, Migrate(ctx), "migrating again, with nothing to apply, grants again")
	problems, err = RoleProblems(ctx, runtime)
	require.NoError(t, err)
	assert.Empty(t, problems, "the grants take back what was granted by hand")
}

// TestNewPostgresService_ReleaseRefusesARoleThatOwnsTheSchema starts the
// server's service in release mode with one role for everything, which is
// refused, and with the two roles, which starts.
func TestNewPostgresService_ReleaseRefusesARoleThatOwnsTheSchema(t *testing.T) {
	withRoles(t, config.MigrationsModeStartup)
	config.Config.Server.Mode = "release"
	config.Config.Postgres.User, config.Config.Postgres.Password = testPGUser, testPGPassword
	config.Config.Postgres.Migrations.User, config.Config.Postgres.Migrations.Password = "", ""

	_, err := NewPostgresService()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "release mode needs a runtime role that cannot change the schema")
	assert.Contains(t, err.Error(), "the role postgres: it has the SUPERUSER attribute")

	config.Config.Postgres.User, config.Config.Postgres.Password = rolesTestRole, rolesTestPassword
	config.Config.Postgres.Migrations.User, config.Config.Postgres.Migrations.Password = testPGUser, testPGPassword
	svc, err := NewPostgresService()
	require.NoError(t, err, "the startup mode migrates with the migration role and grants the runtime role")
	defer svc.Close()
	var user string
	require.NoError(t, svc.DB.QueryRow(`SELECT current_user`).Scan(&user))
	assert.Equal(t, rolesTestRole, user, "the pool of the server uses the runtime role")
}

// TestNewPostgresService_ExternalModeNeedsAMigratedDatabase checks the
// version check of the "external" mode: an instance refuses a database that
// misses migrations or holds a failed one, and accepts one that is ahead,
// as while a release rolls out.
func TestNewPostgresService_ExternalModeNeedsAMigratedDatabase(t *testing.T) {
	withRoles(t, config.MigrationsModeExternal)
	ctx := context.Background()

	_, err := NewPostgresService()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the database schema is at version 0, but this build needs")
	assert.Contains(t, err.Error(), `run "admin migrate" first`)
	owner := openAs(t, testPGUser, testPGPassword)
	var tables int
	require.NoError(t, owner.QueryRow(`SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`).Scan(&tables))
	assert.Zero(t, tables, "the server never migrates in the external mode")

	require.NoError(t, Migrate(ctx))
	svc, err := NewPostgresService()
	require.NoError(t, err)
	svc.Close()

	latest := latestFileMigration(t)
	_, err = owner.Exec(`UPDATE schema_migrations SET version = $1`, latest+1)
	require.NoError(t, err)
	svc, err = NewPostgresService()
	require.NoError(t, err, "a database ahead of the build is accepted")
	svc.Close()

	_, err = owner.Exec(`UPDATE schema_migrations SET version = $1, dirty = true`, latest)
	require.NoError(t, err)
	_, err = NewPostgresService()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "which failed half-way (dirty)")
}

// TestReadSchemaState_LatestIsTheNewestMigrationFile compares the latest
// version with the files of the migrations directory.
func TestReadSchemaState_LatestIsTheNewestMigrationFile(t *testing.T) {
	source, err := pgtest.MigrationsURL()
	require.NoError(t, err)
	latest, err := latestMigration(source)
	require.NoError(t, err)
	assert.Equal(t, latestFileMigration(t), latest)

	state := SchemaState{Version: 3, Latest: 5}
	assert.True(t, state.Behind())
	assert.False(t, state.Ahead())
	state.Version = 6
	assert.True(t, state.Ahead())
}

// latestFileMigration returns the highest version among the migration files.
func latestFileMigration(t *testing.T) uint {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(".", "migrations"))
	require.NoError(t, err)
	pattern := regexp.MustCompile(`^(\d+)_.*\.up\.sql$`)
	var latest uint
	for _, entry := range entries {
		if m := pattern.FindStringSubmatch(entry.Name()); m != nil {
			v, err := strconv.ParseUint(m[1], 10, 32)
			require.NoError(t, err)
			latest = max(latest, uint(v))
		}
	}
	require.NotZero(t, latest)
	return latest
}

// TestGrantRuntimeRole_RefusesAnEmptyRole keeps a missing postgres.user from
// turning into a GRANT to PUBLIC or a syntax error.
func TestGrantRuntimeRole_RefusesAnEmptyRole(t *testing.T) {
	err := grantRuntimeRole(context.Background(), nil, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "postgres.user is empty")
}
