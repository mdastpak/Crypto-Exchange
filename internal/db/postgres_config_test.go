package db

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// trickyValues are passwords, users, and database names with every
// character that the keyword/value syntax or a URL treats specially, and
// values that tried to add a keyword before #229.
var trickyValues = []string{
	"a b", "it's", `back\slash`, "p=q", "p=q sslmode=disable", "user@host", "a/b", "what?", "#hash",
	"%41", "colon:here", "ünïcode", `'quoted' "double"`, " leading and trailing ",
}

// usePostgres sets the postgres section for one test.
func usePostgres(t *testing.T, host, port, user, password, dbname, sslmode string) {
	t.Helper()
	saved := config.Config
	t.Cleanup(func() { config.Config = saved })
	config.Config.Postgres.Host, config.Config.Postgres.Port = host, port
	config.Config.Postgres.User, config.Config.Postgres.Password = user, password
	config.Config.Postgres.DBName, config.Config.Postgres.SSLMode = dbname, sslmode
	config.Config.Postgres.ConnectRetries, config.Config.Postgres.ConnectRetryDelayMs = 1, 10
}

// TestPostgresConfig_KeepsEveryValueAsItIs parses the settings with every
// tricky value in each field, and checks that each reaches the
// configuration unchanged and that nothing else changed (#229).
func TestPostgresConfig_KeepsEveryValueAsItIs(t *testing.T) {
	for _, value := range trickyValues {
		t.Run(value, func(t *testing.T) {
			usePostgres(t, "db.internal", "6432", value, value, value, "require")
			cfg, err := PostgresConfig()
			require.NoError(t, err)
			assert.Equal(t, value, cfg.User)
			assert.Equal(t, value, cfg.Password)
			assert.Equal(t, value, cfg.Database)
			assert.Equal(t, "db.internal", cfg.Host)
			assert.Equal(t, uint16(6432), cfg.Port)
			assert.NotNil(t, cfg.TLSConfig, "sslmode=require stays in force")
			for _, fallback := range cfg.Fallbacks {
				assert.NotNil(t, fallback.TLSConfig, "no fallback without TLS: sslmode was not overridden")
			}
			assert.Empty(t, cfg.RuntimeParams["sslmode"])
			for key := range cfg.RuntimeParams {
				assert.Contains(t, []string{"application_name"}, key, "no value adds a run-time parameter")
			}
		})
	}
}

// TestPostgresConfig_SSLModeCannotBeInjected checks that a value can no
// longer turn TLS off, as "p=q sslmode=disable" could in the keyword/value
// string, whatever field it is in.
func TestPostgresConfig_SSLModeCannotBeInjected(t *testing.T) {
	injected := "x sslmode=disable target_session_attrs=any"
	usePostgres(t, "db.internal", "5432", injected, injected, injected, "verify-full")
	cfg, err := PostgresConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg.TLSConfig)
	assert.False(t, cfg.TLSConfig.InsecureSkipVerify, "verify-full checks the certificate")
	assert.Empty(t, cfg.Fallbacks, "verify-full has no plaintext fallback")
	assert.Equal(t, injected, cfg.Password)
}

// TestPostgresConfig_ErrorsNeverShowThePassword fails to parse and to
// connect, and checks that neither error holds the password.
func TestPostgresConfig_ErrorsNeverShowThePassword(t *testing.T) {
	const password = "s3cr3t p@ss=word"
	usePostgres(t, "localhost", "not-a-port", "u", password, "d", "disable")
	_, err := PostgresConfig()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), password)
	assert.NotContains(t, err.Error(), "s3cr3t")

	usePostgres(t, "127.0.0.1", "1", "u", password, "d", "disable")
	svc, err := ConnectPostgres()
	require.Error(t, err)
	assert.Nil(t, svc)
	assert.NotContains(t, err.Error(), "s3cr3t")
}

// TestConnectPostgres_SignsInWithTrickyCredentials creates a role and a
// database whose names and password hold the tricky characters, signs in
// through the configuration, and checks what the server received.
func TestConnectPostgres_SignsInWithTrickyCredentials(t *testing.T) {
	admin := openTestDB(t)
	defer admin.Close()
	ctx := context.Background()

	const role = `testdb_dsn user's "x"`
	const database = `testdb_dsn db/what?#x`
	password := strings.Join(trickyValues, "")
	quote := func(s string) string { return pgx.Identifier{s}.Sanitize() }
	cleanup := func() {
		_, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quote(database))
		_, _ = admin.ExecContext(ctx, "DROP ROLE IF EXISTS "+quote(role))
	}
	cleanup()
	t.Cleanup(cleanup)
	// A role password cannot be a bind parameter of CREATE ROLE; the
	// literal is quoted with the standard escaping of single quotes.
	literal := "'" + strings.ReplaceAll(password, "'", "''") + "'"
	_, err := admin.ExecContext(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD %s", quote(role), literal))
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s OWNER %s", quote(database), quote(role)))
	require.NoError(t, err)

	usePostgres(t, testPGHost, testPGPort, role, password, database, testPGSSLMode)
	svc, err := ConnectPostgres()
	require.NoError(t, err, "the password that reached the server equals the configured one")
	defer svc.Close()
	var user, db string
	require.NoError(t, svc.DB.QueryRowContext(ctx, `SELECT current_user, current_database()`).Scan(&user, &db))
	assert.Equal(t, role, user)
	assert.Equal(t, database, db)

	usePostgres(t, testPGHost, testPGPort, role, password+"x", database, testPGSSLMode)
	_, err = ConnectPostgres()
	require.Error(t, err, "a wrong password is refused, so the check above is meaningful")
	assert.NotContains(t, err.Error(), password)
}
