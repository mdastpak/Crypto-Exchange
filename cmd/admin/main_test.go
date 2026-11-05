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

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/keys"

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

func TestParseArgs_Keys(t *testing.T) {
	for args, want := range map[string]command{
		"keys list":                    {name: "keys list"},
		"keys rewrap":                  {name: "keys rewrap"},
		"keys rotate -purpose row-mac": {name: "keys rotate", purpose: keys.PurposeRowMAC},
		"keys retire -purpose blind-index -version 2": {name: "keys retire", purpose: keys.PurposeBlindIndex, version: 2},
		"keys retire -version 3 -purpose field-enc":   {name: "keys retire", purpose: keys.PurposeFieldEnc, version: 3},
	} {
		cmd, err := parseArgs(strings.Fields(args), &bytes.Buffer{})
		require.NoError(t, err, args)
		want.configPath, want.migrationsPath = "config/config.yaml", "file://internal/db/migrations"
		assert.Equal(t, want, cmd, args)
	}

	for _, args := range []string{
		"keys",
		"keys delete",
		"keys rotate",
		"keys rotate -purpose kek",
		"keys retire -purpose row-mac",
		"keys retire -purpose row-mac -version 0",
		"keys list -purpose row-mac",
		"keys list extra",
	} {
		_, err := parseArgs(strings.Fields(args), &bytes.Buffer{})
		assert.Error(t, err, args)
	}
}

// writeKeysConfig writes a configuration for the test database with the
// keys section and returns the arguments that select it.
func writeKeysConfig(t *testing.T, keysYAML string) []string {
	t.Helper()
	args := writeConfig(t, "")
	f, err := os.OpenFile(args[1], os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(keysYAML)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return args
}

func TestRun_ManagesDataKeys(t *testing.T) {
	resetCLITestDB(t)
	kek := make([]byte, 32)
	_, err := rand.Read(kek)
	require.NoError(t, err)
	base := writeKeysConfig(t, fmt.Sprintf("keys:\n  provider: \"local\"\n  local:\n    kek: %q\n", base64.StdEncoding.EncodeToString(kek)))

	code, stdout, stderr := runCLI(t, append(base, "keys", "list")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "There are no data keys yet")

	// The CLI creates no first keys; the server does on its first start.
	code, stdout, stderr = runCLI(t, append(base, "keys", "rotate", "-purpose", "row-mac")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "Created data key row-mac/v1")
	code, stdout, stderr = runCLI(t, append(base, "keys", "rotate", "-purpose", "row-mac")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "Created data key row-mac/v2")

	code, _, stderr = runCLI(t, append(base, "keys", "retire", "-purpose", "row-mac", "-version", "2")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "newest version")
	code, stdout, stderr = runCLI(t, append(base, "keys", "retire", "-purpose", "row-mac", "-version", "1")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "Retired data key row-mac/v1")

	code, stdout, stderr = runCLI(t, append(base, "keys", "rewrap")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "Wrapped 2 data keys again")

	code, stdout, stderr = runCLI(t, append(base, "keys", "list")...)
	require.Equal(t, 0, code, stderr)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	require.Len(t, lines, 3, stdout)
	assert.Regexp(t, `^PURPOSE\s+VERSION\s+STATE\s+KEK\s+CREATED\s+RETIRED$`, lines[0])
	assert.Regexp(t, `^row-mac\s+1\s+retired\s+local:[0-9a-f]{16}\s+\S+\s+\d{4}-`, lines[1])
	assert.Regexp(t, `^row-mac\s+2\s+current\s+local:[0-9a-f]{16}\s+\S+\s+-$`, lines[2])
	assert.NotContains(t, stdout, base64.StdEncoding.EncodeToString(kek))
}

func TestRun_KeysNeedAProvider(t *testing.T) {
	resetCLITestDB(t)

	code, _, stderr := runCLI(t, append(writeKeysConfig(t, ""), "keys", "list")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "keys.provider (KEYS_PROVIDER) is not set")

	code, _, stderr = runCLI(t, append(writeKeysConfig(t, "keys:\n  provider: \"local\"\n  local:\n    kek: \"short\"\n"), "keys", "list")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "KEYS_LOCAL_KEK")

	release := fmt.Sprintf("server:\n  mode: \"release\"\nkeys:\n  provider: \"local\"\n  local:\n    kek: %q\n", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	code, _, stderr = runCLI(t, append(writeKeysConfig(t, release), "keys", "list")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "refused in release mode")
}
