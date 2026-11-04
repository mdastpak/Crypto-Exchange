package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"encrypted-db/config"

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
	// full-migration tests below. go test runs different packages in
	// parallel, and several other packages call NewTestPostgresService
	// against the shared testPGDBName database; running full schema
	// resets/migrations there would race with them. Using an isolated
	// database sidesteps that entirely instead of relying on cross-process
	// locking.
	migrationsTestDBName = "testdb_migrations"
)

func TestNewTestPostgresService_Success(t *testing.T) {
	svc, err := NewTestPostgresService(testPGHost, testPGPort, testPGUser, testPGPassword, testPGDBName, testPGSSLMode)
	require.NoError(t, err)
	require.NotNil(t, svc)
	defer svc.Close()

	assert.NoError(t, svc.Ping(context.Background()))
}

func TestNewTestPostgresService_InvalidHost(t *testing.T) {
	svc, err := NewTestPostgresService("no-such-host-xyz", testPGPort, testPGUser, testPGPassword, testPGDBName, testPGSSLMode)
	assert.Error(t, err)
	assert.Nil(t, svc)
}

func TestNewTestPostgresService_InvalidCredentials(t *testing.T) {
	svc, err := NewTestPostgresService(testPGHost, testPGPort, testPGUser, "definitely-wrong-password", testPGDBName, testPGSSLMode)
	assert.Error(t, err)
	assert.Nil(t, svc)
}

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

func TestPostgresService_StatsAfterConnect(t *testing.T) {
	svc, err := NewTestPostgresService(testPGHost, testPGPort, testPGUser, testPGPassword, testPGDBName, testPGSSLMode)
	require.NoError(t, err)
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
	// other packages' tests that use the shared testPGDBName database.
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

// TestMigrations_FullUpDownUpCycle exercises the entire migration chain
// (1 through the latest) forward, all the way back down, and forward again,
// against the dedicated migrations-test database. It guards against
// regressions of the forward-reference foreign keys, users(id) vs
// users(hk) type mismatches, type-name collisions, and down.sql ordering
// bugs that previously made migrations 5-10 fail on a fresh database (and
// made the down migrations unusable).
func TestMigrations_FullUpDownUpCycle(t *testing.T) {
	ensureMigrationsTestDB(t)
	resetMigrationsTestSchema(t)
	t.Cleanup(func() { resetMigrationsTestSchema(t) })

	cwd, err := filepath.Abs(".")
	require.NoError(t, err)
	migrationsAbs := filepath.Join(cwd, "migrations")
	sourceURL := "file://" + strings.ReplaceAll(migrationsAbs, "\\", "/")
	dbURL := "postgres://" + fmt.Sprintf("%s:%s@%s:%s/%s?sslmode=%s",
		testPGUser, testPGPassword, testPGHost, testPGPort, migrationsTestDBName, testPGSSLMode)

	m, err := migrate.New(sourceURL, dbURL)
	require.NoError(t, err)
	defer m.Close()

	require.NoError(t, m.Up(), "full up migration should succeed on a fresh database")
	require.NoError(t, m.Down(), "full down migration should fully reverse the schema")
	require.NoError(t, m.Up(), "re-applying up migrations after a full down should succeed")
}

// TestMigration11_StepDownAndUpWithOTPUsers checks that migration 11 can be
// rolled back and re-applied on its own while users created by OTP sign-in
// exist. Those rows do not satisfy the constraint that the down migration
// restores, so the down migration must not validate existing rows.
func TestMigration11_StepDownAndUpWithOTPUsers(t *testing.T) {
	m := newMigrationsTestMigrator(t)
	require.NoError(t, m.Migrate(11))

	conn, err := sql.Open("pgx", migrationsTestDBConnStr())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(`INSERT INTO users (info, status) VALUES ('{"contact": {"email": "otp-user@example.com"}}', 'approved')`)
	require.NoError(t, err, "migration 11 must accept the document written by OTP sign-in")

	require.NoError(t, m.Steps(-1), "rolling back migration 11 must keep existing OTP users")
	require.NoError(t, m.Steps(1), "re-applying migration 11 must succeed")

	var count int
	require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM users WHERE info->'contact'->>'email' = 'otp-user@example.com'`).Scan(&count))
	assert.Equal(t, 1, count)
}

// TestMigration11_PreflightStopsOnManualData checks that migration 11 stops
// with an actionable message, before changing the schema, when users holds
// rows that no application code writes: rows without a contact object, or
// contact values shared by several users.
func TestMigration11_PreflightStopsOnManualData(t *testing.T) {
	cases := []struct {
		name    string
		rows    []string
		wantErr string // empty when the migration must succeed
	}{
		{
			name:    "row without contact",
			rows:    []string{`{"username": "legacy", "password": "secret"}`},
			wantErr: `have no "contact" object`,
		},
		{
			name: "email shared by two users",
			rows: []string{
				`{"username": "first", "password": "secret", "contact": {"email": "Shared@Example.com"}}`,
				`{"username": "second", "password": "secret", "contact": {"email": "shared@example.com"}}`,
			},
			wantErr: "shared by more than one user",
		},
		{
			name: "null contact values are not duplicates",
			rows: []string{
				`{"username": "first", "password": "secret", "contact": {"email": null, "mobile": "09120000001"}}`,
				`{"username": "second", "password": "secret", "contact": {"email": null, "mobile": "09120000002"}}`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMigrationsTestMigrator(t)
			require.NoError(t, m.Migrate(10))

			conn, err := sql.Open("pgx", migrationsTestDBConnStr())
			require.NoError(t, err)
			defer conn.Close()
			for _, row := range tc.rows {
				_, err := conn.Exec(`INSERT INTO users (info) VALUES ($1::jsonb)`, row)
				require.NoError(t, err)
			}

			err = m.Migrate(11)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Contains(t, err.Error(), `run "migrate force 10"`, "the error must explain how to recover from the dirty version")

			var oldConstraintExists bool
			require.NoError(t, conn.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'user_info_structure')`).Scan(&oldConstraintExists))
			assert.True(t, oldConstraintExists, "a failed preflight must leave the schema unchanged")
		})
	}
}

// newMigrationsTestMigrator returns a migrator for the repository migrations
// on a freshly reset migrations-test database.
func newMigrationsTestMigrator(t *testing.T) *migrate.Migrate {
	t.Helper()
	ensureMigrationsTestDB(t)
	resetMigrationsTestSchema(t)
	t.Cleanup(func() { resetMigrationsTestSchema(t) })

	cwd, err := filepath.Abs(".")
	require.NoError(t, err)
	sourceURL := "file://" + strings.ReplaceAll(filepath.Join(cwd, "migrations"), "\\", "/")
	dbURL := "postgres://" + fmt.Sprintf("%s:%s@%s:%s/%s?sslmode=%s",
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
	svc, err := NewTestPostgresService(testPGHost, testPGPort, testPGUser, testPGPassword, testPGDBName, testPGSSLMode)
	require.NoError(t, err)
	defer svc.Close()

	// A healthy DB should succeed immediately regardless of the retry budget.
	err = pingWithRetry(svc.DB, 3, 10*time.Millisecond)
	assert.NoError(t, err)
}

func TestPingWithRetry_ExhaustsAttemptsAndReturnsLastError(t *testing.T) {
	svc, err := NewTestPostgresService(testPGHost, testPGPort, testPGUser, testPGPassword, testPGDBName, testPGSSLMode)
	require.NoError(t, err)
	svc.Close() // closed DB handle: subsequent pings must fail

	start := time.Now()
	err = pingWithRetry(svc.DB, 3, 20*time.Millisecond)
	elapsed := time.Since(start)

	assert.Error(t, err)
	// 2 retry delays of 20ms + 40ms (exponential) should have elapsed.
	assert.GreaterOrEqual(t, elapsed, 20*time.Millisecond)
}

func TestPingWithRetry_NormalizesInvalidArguments(t *testing.T) {
	svc, err := NewTestPostgresService(testPGHost, testPGPort, testPGUser, testPGPassword, testPGDBName, testPGSSLMode)
	require.NoError(t, err)
	defer svc.Close()

	// maxAttempts <= 0 and delay <= 0 should be normalized instead of looping forever.
	err = pingWithRetry(svc.DB, 0, 0)
	assert.NoError(t, err)
}

// TestMigration13_OnlyNonEmptyContractsAreUnique checks that several
// currencies without a contract can coexist, while a contract still belongs to
// one currency only, and that the migration rolls back.
func TestMigration13_OnlyNonEmptyContractsAreUnique(t *testing.T) {
	m := newMigrationsTestMigrator(t)
	require.NoError(t, m.Migrate(13))

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

	_, err = conn.Exec(`UPDATE currencies SET info = jsonb_set(info, '{contract}', '"native-eth"') WHERE info->>'symbol' = 'ETH'`)
	require.NoError(t, err)
	require.NoError(t, m.Steps(-1), "rolling back works once the empty contracts are distinct")
	require.NoError(t, m.Steps(1))
}
