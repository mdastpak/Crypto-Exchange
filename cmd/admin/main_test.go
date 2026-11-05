package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/config"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cliTestDBName = "testdb_admin_cli"

func TestParseArgs(t *testing.T) {
	cmd, err := parseArgs([]string{"-config", "c.yaml", "create", "-email", "admin@example.com"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, command{
		configPath:     "c.yaml",
		migrationsPath: "file://internal/db/migrations",
		name:           "create",
		email:          "admin@example.com",
	}, cmd)

	for _, args := range [][]string{
		{},
		{"promote", "-email", "admin@example.com"},
		{"create"},
		{"create", "-email", ""},
		{"-unknown", "create", "-email", "admin@example.com"},
	} {
		_, err := parseArgs(args, &bytes.Buffer{})
		assert.Error(t, err, "%q", args)
	}
}

// resetCLITestDB empties a dedicated database and skips the test when
// PostgreSQL is not reachable.
func resetCLITestDB(t *testing.T) {
	t.Helper()
	admin, err := sql.Open("pgx", "host=localhost port=5432 user=postgres password=test dbname=postgres sslmode=disable")
	require.NoError(t, err)
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Skipf("PostgreSQL is not reachable at localhost:5432: %v", err)
	}
	var exists bool
	require.NoError(t, admin.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, cliTestDBName).Scan(&exists))
	if !exists {
		_, err := admin.Exec("CREATE DATABASE " + cliTestDBName)
		require.NoError(t, err)
	}
	conn, err := sql.Open("pgx", fmt.Sprintf("host=localhost port=5432 user=postgres password=test dbname=%s sslmode=disable", cliTestDBName))
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec("DROP SCHEMA public CASCADE; CREATE SCHEMA public;")
	require.NoError(t, err)
}

// writeConfig writes a configuration for the test database and returns the
// arguments that select it.
func writeConfig(t *testing.T, totpKey string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := fmt.Sprintf(`postgres:
  host: "localhost"
  port: "5432"
  user: "postgres"
  password: "test"
  dbname: %q
  sslmode: "disable"
  connect_retries: 1
  connect_retry_delay_ms: 10
admin:
  totp_key: %q
exchange:
  name: "Test Exchange"
`, cliTestDBName, totpKey)
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	migrations, err := filepath.Abs(filepath.Join("..", "..", "internal", "db", "migrations"))
	require.NoError(t, err)
	return []string{"-config", path, "-migrations", "file://" + filepath.ToSlash(migrations)}
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	saved := config.Config
	savedPath, savedMigrations := config.ConfigPath(), config.MigrationsPath()
	t.Cleanup(func() {
		config.Config = saved
		config.SetConfigPath(savedPath)
		config.SetMigrationsPath(savedMigrations)
	})
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRun_ManagesAdministrators(t *testing.T) {
	resetCLITestDB(t)
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	base := writeConfig(t, base64.StdEncoding.EncodeToString(key))

	code, stdout, stderr := runCLI(t, append(base, "create", "-email", "admin@example.com")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "Created administrator admin@example.com")
	assert.Contains(t, stdout, "otpauth://totp/Test%20Exchange:admin@example.com?")
	assert.Contains(t, stdout, "Manual entry key: ")

	code, _, stderr = runCLI(t, append(base, "create", "-email", "admin@example.com")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "already exists")

	code, stdout, stderr = runCLI(t, append(base, "reset-totp", "-email", "admin@example.com")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "The old secret no longer works")

	code, stdout, stderr = runCLI(t, append(base, "disable", "-email", "admin@example.com")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "Disabled administrator admin@example.com")

	code, _, stderr = runCLI(t, append(base, "disable", "-email", "nobody@example.com")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "no active administrator")
}

func TestRun_RequiresTheTOTPKey(t *testing.T) {
	resetCLITestDB(t)

	code, _, stderr := runCLI(t, append(writeConfig(t, ""), "create", "-email", "admin@example.com")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "admin.totp_key")

	code, _, stderr = runCLI(t, append(writeConfig(t, "too-short"), "create", "-email", "admin@example.com")...)
	assert.Equal(t, 1, code)
	assert.True(t, strings.Contains(stderr, "32 random bytes"), stderr)
}
