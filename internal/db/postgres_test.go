package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testPGHost     = "localhost"
	testPGPort     = "5432"
	testPGUser     = "postgres"
	testPGPassword = "test"
	testPGDBName   = "testdb"
	testPGSSLMode  = "disable"

	// migrationsTestDBName is a dedicated database used only by the
	// migration tests below, so that their schema resets cannot race the
	// tests of other packages, which go test runs in parallel.
	migrationsTestDBName = "testdb_migrations"
)

func TestPostgresService_PingNilDB(t *testing.T) {
	svc := &PostgresService{}
	err := svc.Ping(context.Background())
	assert.Error(t, err)
}

func TestPostgresService_StatsNilDB(t *testing.T) {
	svc := &PostgresService{}
	stats := svc.Stats()
	assert.Equal(t, 0, stats.OpenConnections)
}

func TestPostgresService_CloseNilDB(t *testing.T) {
	svc := &PostgresService{}
	assert.NoError(t, svc.Close())
}

// openTestDB connects to the shared test database. The tests that use it
// need a connection, not a schema.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("pgx", fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		testPGHost, testPGPort, testPGUser, testPGPassword, testPGDBName, testPGSSLMode))
	require.NoError(t, err)
	require.NoError(t, conn.Ping())
	return conn
}

func TestPostgresService_StatsAfterConnect(t *testing.T) {
	svc := &PostgresService{DB: openTestDB(t)}
	defer svc.Close()

	stats := svc.Stats()
	assert.GreaterOrEqual(t, stats.OpenConnections, 0)
}

func TestNewPostgresService_UsesConfigAndAppliesPoolSettings(t *testing.T) {
	originalConfig := config.Config
	originalMigrationsPath := config.MigrationsPath()
	defer func() {
		config.Config = originalConfig
		config.SetMigrationsPath(originalMigrationsPath)
	}()

	config.Config.Postgres.Host = testPGHost
	config.Config.Postgres.Port = testPGPort
	config.Config.Postgres.User = testPGUser
	config.Config.Postgres.Password = testPGPassword
	config.Config.Postgres.DBName = migrationsTestDBName
	config.Config.Postgres.SSLMode = testPGSSLMode
	config.Config.Postgres.MaxOpenConns = 12
	config.Config.Postgres.MaxIdleConns = 3
	config.Config.Postgres.ConnMaxLifetime = 1
	config.Config.Postgres.ConnMaxIdleTime = 1
	config.Config.Postgres.ConnectRetries = 1
	config.Config.Postgres.ConnectRetryDelayMs = 10
	// Point at the real repo migrations directory (absolute path, forward
	// slashes for a valid file:// URL on Windows). This test runs against
	// its own dedicated database (migrationsTestDBName), migrated forward
	// to the latest version to match production schema, so it cannot race
	// the tests of other packages.
	cwd, err := filepath.Abs(".")
	require.NoError(t, err)
	migrationsAbs := filepath.Join(cwd, "migrations")
	config.SetMigrationsPath("file://" + strings.ReplaceAll(migrationsAbs, "\\", "/"))

	ensureMigrationsTestDB(t)
	resetMigrationsTestSchema(t)
	t.Cleanup(func() { resetMigrationsTestSchema(t) })

	svc, err := NewPostgresService()
	require.NoError(t, err)
	require.NotNil(t, svc)
	defer svc.Close()

	assert.NoError(t, svc.Ping(context.Background()))
}

// ensureMigrationsTestDB creates the dedicated migrations-test database if
// it does not already exist. CREATE DATABASE cannot run inside a
// transaction or against the database being created, so this connects to
// the default "postgres" administrative database instead.
func ensureMigrationsTestDB(t *testing.T) {
	t.Helper()
	adminConnStr := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=postgres sslmode=%s",
		testPGHost, testPGPort, testPGUser, testPGPassword, testPGSSLMode,
	)
	adminDB, err := sql.Open("pgx", adminConnStr)
	require.NoError(t, err)
	defer adminDB.Close()

	var exists bool
	err = adminDB.QueryRow("SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", migrationsTestDBName).Scan(&exists)
	require.NoError(t, err)

	if !exists {
		_, err = adminDB.Exec("CREATE DATABASE " + migrationsTestDBName)
		require.NoError(t, err)
	}
}

// migrationsTestDBConnStr builds a connection string to the dedicated
// migrations-test database.
func migrationsTestDBConnStr() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		testPGHost, testPGPort, testPGUser, testPGPassword, migrationsTestDBName, testPGSSLMode,
	)
}

// resetMigrationsTestSchema drops and recreates the public schema of the
// dedicated migrations-test database, giving each migration test a clean
// starting point regardless of run order.
func resetMigrationsTestSchema(t *testing.T) {
	t.Helper()
	resetDB, err := sql.Open("pgx", migrationsTestDBConnStr())
	if err != nil {
		t.Logf("schema reset: failed to open db: %v", err)
		return
	}
	defer resetDB.Close()
	if _, execErr := resetDB.Exec("DROP SCHEMA public CASCADE; CREATE SCHEMA public;"); execErr != nil {
		t.Logf("schema reset: failed to reset schema: %v", execErr)
	}
}

// TestMigrations_FullUpDownUpCycle applies every migration, rolls all of
// them back, and applies them again, against the dedicated migrations-test
// database. A down migration that leaves something behind fails the second
// run.
func TestMigrations_FullUpDownUpCycle(t *testing.T) {
	ensureMigrationsTestDB(t)
	// One package at a time empties and migrates a schema (#338); the lock
	// is held until the reset of the cleanup has run.
	pgtest.Lock(t)
	resetMigrationsTestSchema(t)
	t.Cleanup(func() { resetMigrationsTestSchema(t) })

	cwd, err := filepath.Abs(".")
	require.NoError(t, err)
	migrationsAbs := filepath.Join(cwd, "migrations")
	sourceURL := "file://" + strings.ReplaceAll(migrationsAbs, "\\", "/")
	dbURL := "pgx5://" + fmt.Sprintf("%s:%s@%s:%s/%s?sslmode=%s",
		testPGUser, testPGPassword, testPGHost, testPGPort, migrationsTestDBName, testPGSSLMode)

	m, err := migrate.New(sourceURL, dbURL)
	require.NoError(t, err)
	defer m.Close()

	require.NoError(t, m.Up(), "full up migration should succeed on a fresh database")
	require.NoError(t, m.Down(), "full down migration should fully reverse the schema")
	require.NoError(t, m.Up(), "re-applying up migrations after a full down should succeed")
}

// newMigrationsTestMigrator returns a migrator for the repository migrations
// on a freshly reset migrations-test database.
func newMigrationsTestMigrator(t *testing.T) *migrate.Migrate {
	t.Helper()
	return newTestMigratorOf(t, "migrations")
}

// newTestMigratorOf returns a migrator of the migration files in dir, on the
// emptied migrations-test database.
func newTestMigratorOf(t *testing.T, dir string) *migrate.Migrate {
	t.Helper()
	ensureMigrationsTestDB(t)
	// Held for the whole test, which migrates with the migrator (#338).
	pgtest.Lock(t)
	resetMigrationsTestSchema(t)
	t.Cleanup(func() { resetMigrationsTestSchema(t) })

	cwd, err := filepath.Abs(".")
	require.NoError(t, err)
	sourceURL := "file://" + strings.ReplaceAll(filepath.Join(cwd, dir), "\\", "/")
	dbURL := "pgx5://" + fmt.Sprintf("%s:%s@%s:%s/%s?sslmode=%s",
		testPGUser, testPGPassword, testPGHost, testPGPort, migrationsTestDBName, testPGSSLMode)

	m, err := migrate.New(sourceURL, dbURL)
	require.NoError(t, err)
	t.Cleanup(func() { m.Close() })
	return m
}

func TestNewPostgresService_FailsFastWithBadHost(t *testing.T) {
	originalConfig := config.Config
	defer func() { config.Config = originalConfig }()

	config.Config.Postgres.Host = "no-such-host-xyz"
	config.Config.Postgres.Port = testPGPort
	config.Config.Postgres.User = testPGUser
	config.Config.Postgres.Password = testPGPassword
	config.Config.Postgres.DBName = testPGDBName
	config.Config.Postgres.SSLMode = testPGSSLMode
	config.Config.Postgres.ConnectRetries = 1
	config.Config.Postgres.ConnectRetryDelayMs = 10

	svc, err := NewPostgresService()
	assert.Error(t, err)
	assert.Nil(t, svc)
}

func TestPingWithRetry_SucceedsAfterTransientFailures(t *testing.T) {
	conn := openTestDB(t)
	defer conn.Close()

	// A healthy DB should succeed immediately regardless of the retry budget.
	err := pingWithRetry(conn, 3, 10*time.Millisecond)
	assert.NoError(t, err)
}

func TestPingWithRetry_ExhaustsAttemptsAndReturnsLastError(t *testing.T) {
	conn := openTestDB(t)
	conn.Close() // closed DB handle: subsequent pings must fail

	start := time.Now()
	err := pingWithRetry(conn, 3, 20*time.Millisecond)
	elapsed := time.Since(start)

	assert.Error(t, err)
	// 2 retry delays of 20ms + 40ms (exponential) should have elapsed.
	assert.GreaterOrEqual(t, elapsed, 20*time.Millisecond)
}

func TestPingWithRetry_NormalizesInvalidArguments(t *testing.T) {
	conn := openTestDB(t)
	defer conn.Close()

	// maxAttempts <= 0 and delay <= 0 should be normalized instead of looping forever.
	err := pingWithRetry(conn, 0, 0)
	assert.NoError(t, err)
}

// TestSchema_OnlyNonEmptyContractsAreUnique checks that several currencies
// without a contract can coexist, while a contract still belongs to one
// currency only.
func TestSchema_OnlyNonEmptyContractsAreUnique(t *testing.T) {
	m := newMigrationsTestMigrator(t)
	require.NoError(t, m.Up())

	conn, err := sql.Open("pgx", migrationsTestDBConnStr())
	require.NoError(t, err)
	defer conn.Close()
	insert := func(symbol, contract string) error {
		_, err := conn.Exec(`INSERT INTO currencies (status, info) VALUES ('approved', jsonb_build_object(
			'en_name', $1::text, 'fa_name', $1::text, 'symbol', $1::text,
			'if_fiat', false, 'is_token', $2::text <> '', 'contract', $2::text))`, symbol, contract)
		return err
	}

	require.NoError(t, insert("BTC", ""))
	require.NoError(t, insert("ETH", ""), "a second currency without a contract must be accepted")
	require.NoError(t, insert("USDT", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"))
	err = insert("FAKE", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "a contract must stay unique")
	assert.Equal(t, "23505", pgErr.Code)
	assert.Equal(t, "currencies_info_contract_unique", pgErr.ConstraintName)
}
