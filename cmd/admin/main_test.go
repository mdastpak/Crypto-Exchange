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
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cliTestDBName = "testdb_admin_cli"

// TestParseArgs_ConfigNamedOnlyWhenGiven checks that only a -config on the
// command line makes the file required (#306).
func TestParseArgs_ConfigNamedOnlyWhenGiven(t *testing.T) {
	cmd, err := parseArgs([]string{"create", "-email", "admin@example.com"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.False(t, cmd.configNamed)
	assert.Equal(t, "config/config.yaml", cmd.configPath)
}

func TestParseArgs(t *testing.T) {
	cmd, err := parseArgs([]string{"-config", "c.yaml", "create", "-email", "admin@example.com"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, command{
		configPath:     "c.yaml",
		configNamed:    true,
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
func writeConfig(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := fmt.Sprintf(`# A development configuration: an instance that names no mode runs in
# release mode (#306).
server:
  mode: "debug"
postgres:
  host: "localhost"
  port: "5432"
  user: "postgres"
  password: "test"
  dbname: %q
  sslmode: "disable"
  connect_retries: 1
  connect_retry_delay_ms: 10
exchange:
  name: "Test Exchange"
`, cliTestDBName)
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
	base := writeLocalKeysConfig(t)

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

	// Each change is audited, with the operator (#195).
	code, _, stderr = runCLI(t, append(append(base, "-operator", "alice"), "reset-totp", "-email", "admin@example.com")...)
	require.Equal(t, 0, code, stderr)
	var events []cliAuditEvent
	for _, e := range cliAuditEvents(t) {
		if strings.HasPrefix(e.action, "admin.") { // the first command also created the data keys
			events = append(events, e)
		}
	}
	require.Len(t, events, 4, "create, reset-totp, disable, and reset-totp; the refused commands record nothing")
	assert.Equal(t, []string{"admin.create", "admin.totp_reset", "admin.disable", "admin.totp_reset"},
		[]string{events[0].action, events[1].action, events[2].action, events[3].action})
	for _, e := range events {
		assert.Equal(t, "system", e.actor)
		assert.Regexp(t, `^cmd/admin \S+; (operator=alice; )?os_user=`, e.agent)
	}
	assert.Contains(t, events[3].agent, "operator=alice")
}

type cliAuditEvent struct{ action, actor, target, agent string }

// cliAuditEvents returns the audit events of the test database, oldest first.
func cliAuditEvents(t *testing.T) []cliAuditEvent {
	t.Helper()
	conn, err := sql.Open("pgx", fmt.Sprintf("host=localhost port=5432 user=postgres password=test dbname=%s sslmode=disable", cliTestDBName))
	require.NoError(t, err)
	defer conn.Close()
	rows, err := conn.Query(`SELECT action, actor_type, COALESCE(target_id, ''), COALESCE(user_agent, '') FROM audit_events ORDER BY hk`)
	require.NoError(t, err)
	defer rows.Close()
	var events []cliAuditEvent
	for rows.Next() {
		var e cliAuditEvent
		require.NoError(t, rows.Scan(&e.action, &e.actor, &e.target, &e.agent))
		events = append(events, e)
	}
	require.NoError(t, rows.Err())
	return events
}

// TestRun_AdministratorsNeedTheDataKeys checks that nothing but the data
// keys seals an authenticator secret: without keys.provider no
// administrator is created, and with it the command creates the keys when
// the server has not started yet.
func TestRun_AdministratorsNeedTheDataKeys(t *testing.T) {
	resetCLITestDB(t)
	conn, err := sql.Open("pgx", fmt.Sprintf("host=localhost port=5432 user=postgres password=test dbname=%s sslmode=disable", cliTestDBName))
	require.NoError(t, err)
	defer conn.Close()

	code, _, stderr := runCLI(t, append(writeConfig(t), "create", "-email", "admin@example.com")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "keys.provider")

	code, _, stderr = runCLI(t, append(writeLocalKeysConfig(t), "create", "-email", "admin@example.com")...)
	require.Equal(t, 0, code, stderr)
	var stored string
	require.NoError(t, conn.QueryRow(`SELECT totp_secret FROM users WHERE totp_secret IS NOT NULL`).Scan(&stored))
	assert.True(t, strings.HasPrefix(stored, "v1:"), "sealed with a field-enc data key")
	var created int
	require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM data_keys WHERE purpose = 'field-enc'`).Scan(&created))
	assert.Equal(t, 1, created)
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
	args := writeConfig(t)
	f, err := os.OpenFile(args[1], os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(keysYAML)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return args
}

// writeLocalKeysConfig writes a configuration with the local key provider
// and a new key-encryption key.
func writeLocalKeysConfig(t *testing.T) []string {
	t.Helper()
	kek := make([]byte, 32)
	_, err := rand.Read(kek)
	require.NoError(t, err)
	return writeKeysConfig(t, fmt.Sprintf("keys:\n  provider: \"local\"\n  local:\n    kek: %q\n", base64.StdEncoding.EncodeToString(kek)))
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

	assert.Contains(t, stdout, "writes with it from", "a new version waits for keys.activation_delay_seconds (#233)")

	code, stdout, stderr = runCLI(t, append(base, "keys", "list")...)
	require.Equal(t, 0, code, stderr)
	assert.Regexp(t, `(?m)^row-mac\s+1\s+current\s`, stdout, "the first version writes at once")
	assert.Regexp(t, `(?m)^row-mac\s+2\s+pending\s`, stdout)

	code, _, stderr = runCLI(t, append(base, "keys", "retire", "-purpose", "row-mac", "-version", "2")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "newest version")
	code, _, stderr = runCLI(t, append(base, "keys", "retire", "-purpose", "row-mac", "-version", "1")...)
	assert.Equal(t, 1, code, "v1 still writes until v2 is active")
	assert.Contains(t, stderr, "still writes")

	conn, err := sql.Open("pgx", fmt.Sprintf("host=localhost port=5432 user=postgres password=test dbname=%s sslmode=disable", cliTestDBName))
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(`UPDATE data_keys SET active_from = NOW() - INTERVAL '1 second' WHERE version = 2`)
	require.NoError(t, err, "the activation delay passes")
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
	assert.Regexp(t, `^PURPOSE\s+VERSION\s+STATE\s+KEK\s+CREATED\s+ACTIVE FROM\s+RETIRED$`, lines[0])
	assert.Regexp(t, `^row-mac\s+1\s+retired\s+local:[0-9a-f]{16}\s+\S+\s+\S+\s+\d{4}-`, lines[1])
	assert.Regexp(t, `^row-mac\s+2\s+current\s+local:[0-9a-f]{16}\s+\S+\s+\S+\s+-$`, lines[2])
	assert.NotContains(t, stdout, base64.StdEncoding.EncodeToString(kek))

	var targets []string
	for _, e := range cliAuditEvents(t) {
		targets = append(targets, e.action+" "+e.target)
	}
	assert.Equal(t, []string{
		"data_key.rotate row-mac/v1", "data_key.rotate row-mac/v2", "data_key.retire row-mac/v1",
		"data_key.rewrap row-mac/v1", "data_key.rewrap row-mac/v2",
	}, targets)
}

// TestRun_KeepsTheKeysOfSealedRows refuses to retire a row-mac version that
// still seals rows (#337): audit events for good, since they are never
// sealed again, and the rows of the sealed tables until the verifier has
// moved them to the current version.
func TestRun_KeepsTheKeysOfSealedRows(t *testing.T) {
	resetCLITestDB(t)
	kek := make([]byte, 32)
	_, err := rand.Read(kek)
	require.NoError(t, err)
	base := writeKeysConfig(t, fmt.Sprintf("keys:\n  provider: \"local\"\n  local:\n    kek: %q\n", base64.StdEncoding.EncodeToString(kek)))
	for range 3 {
		code, _, stderr := runCLI(t, append(base, "keys", "rotate", "-purpose", "row-mac")...)
		require.Equal(t, 0, code, stderr)
	}
	conn, err := sql.Open("pgx", fmt.Sprintf("host=localhost port=5432 user=postgres password=test dbname=%s sslmode=disable", cliTestDBName))
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(`UPDATE data_keys SET active_from = NOW() - INTERVAL '1 second'`)
	require.NoError(t, err, "every version is active")
	_, err = conn.Exec(`INSERT INTO audit_events (hk, actor_type, action, outcome, row_seal, seal_key_version)
		VALUES (gen_random_uuid(), 'system', 'test.seal', 'success', '\x00', 1)`)
	require.NoError(t, err)
	hk := usertest.Insert(t, conn, usertest.User{Status: "ACTIVE"})
	_, err = conn.Exec(`UPDATE users SET row_seal = '\x00', seal_key_version = 2 WHERE hk = $1`, hk)
	require.NoError(t, err)

	code, _, stderr := runCLI(t, append(base, "keys", "retire", "-purpose", "row-mac", "-version", "1")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "row-mac/v1 seals audit events")
	code, _, stderr = runCLI(t, append(base, "keys", "retire", "-purpose", "row-mac", "-version", "2")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "row-mac/v2 still seals rows of users")

	_, err = conn.Exec(`UPDATE users SET seal_key_version = 3 WHERE hk = $1`, hk)
	require.NoError(t, err, "the verifier moved the row")
	code, stdout, stderr := runCLI(t, append(base, "keys", "retire", "-purpose", "row-mac", "-version", "2")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "Retired data key row-mac/v2")
}

func TestRun_KeysNeedAProvider(t *testing.T) {
	resetCLITestDB(t)

	code, _, stderr := runCLI(t, append(writeKeysConfig(t, ""), "keys", "list")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "keys.provider (KEYS_PROVIDER) is not set")

	code, _, stderr = runCLI(t, append(writeKeysConfig(t, "keys:\n  provider: \"local\"\n  local:\n    kek: \"short\"\n"), "keys", "list")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "KEYS_LOCAL_KEK")

	// The file names debug mode; SERVER_MODE overrides it. A release names
	// its region (#306).
	t.Setenv("SERVER_MODE", "release")
	release := fmt.Sprintf("deployment:\n  region: \"ir\"\nkeys:\n  provider: \"local\"\n  local:\n    kek: %q\n", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	code, _, stderr = runCLI(t, append(writeKeysConfig(t, release), "keys", "list")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "refused in release mode")
}

func TestParseArgs_Outbox(t *testing.T) {
	for args, want := range map[string]command{
		"outbox list":              {name: "outbox list"},
		"outbox retry -id 7":       {name: "outbox retry", id: 7},
		"outbox discard -id 12345": {name: "outbox discard", id: 12345},
	} {
		cmd, err := parseArgs(strings.Fields(args), &bytes.Buffer{})
		require.NoError(t, err, args)
		want.configPath, want.migrationsPath = "config/config.yaml", "file://internal/db/migrations"
		assert.Equal(t, want, cmd, args)
	}
	for _, args := range []string{"outbox", "outbox purge", "outbox retry", "outbox retry -id 0", "outbox discard -id x", "outbox list extra"} {
		_, err := parseArgs(strings.Fields(args), &bytes.Buffer{})
		assert.Error(t, err, args)
	}
}

func TestRun_ManagesDeadLetters(t *testing.T) {
	resetCLITestDB(t)
	base := writeConfig(t)

	code, stdout, stderr := runCLI(t, append(base, "outbox", "list")...)
	require.Equal(t, 0, code, stderr)
	assert.Equal(t, "0 messages pending; 0 dead letters.\n", stdout)

	conn, err := sql.Open("pgx", fmt.Sprintf("host=localhost port=5432 user=postgres password=test dbname=%s sslmode=disable", cliTestDBName))
	require.NoError(t, err)
	defer conn.Close()
	var retryID, discardID int64
	require.NoError(t, conn.QueryRow(`INSERT INTO outbox_events (topic, payload, attempts, last_error, failed_at)
		VALUES ('currencies', '{}', 20, 'refused
by the broker', NOW()) RETURNING id`).Scan(&retryID))
	require.NoError(t, conn.QueryRow(`INSERT INTO outbox_events (topic, payload, attempts, failed_at)
		VALUES ('currencies', '{}', 20, NOW()) RETURNING id`).Scan(&discardID))
	_, err = conn.Exec(`INSERT INTO outbox_events (topic, payload) VALUES ('currencies', '{}')`)
	require.NoError(t, err)

	code, stdout, stderr = runCLI(t, append(base, "outbox", "list")...)
	require.Equal(t, 0, code, stderr)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	require.Len(t, lines, 4, stdout)
	assert.Regexp(t, `^1 messages pending, the oldest for \S+; 2 dead letters\.$`, lines[0])
	assert.Regexp(t, `^ID\s+TOPIC\s+ATTEMPTS\s+CREATED\s+FAILED\s+LAST ERROR$`, lines[1])
	assert.Regexp(t, fmt.Sprintf(`^%d\s+currencies\s+20\s+\S+\s+\S+\s+refused by the broker$`, retryID), lines[2])

	code, stdout, stderr = runCLI(t, append(base, "outbox", "retry", "-id", fmt.Sprint(retryID))...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "is pending again")
	code, stdout, stderr = runCLI(t, append(base, "outbox", "discard", "-id", fmt.Sprint(discardID))...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "never published")

	code, _, stderr = runCLI(t, append(base, "outbox", "discard", "-id", fmt.Sprint(retryID))...)
	assert.Equal(t, 1, code, "a pending message is not a dead letter")
	assert.Contains(t, stderr, "no dead letter has this id")

	code, stdout, stderr = runCLI(t, append(base, "outbox", "list")...)
	require.Equal(t, 0, code, stderr)
	assert.Regexp(t, `^2 messages pending, the oldest for \S+; 0 dead letters\.\n$`, stdout)

	events := cliAuditEvents(t)
	require.Len(t, events, 2)
	assert.Equal(t, cliAuditEvent{"outbox.retry", "system", fmt.Sprint(retryID), events[0].agent}, events[0])
	assert.Equal(t, cliAuditEvent{"outbox.discard", "system", fmt.Sprint(discardID), events[1].agent}, events[1])
}
