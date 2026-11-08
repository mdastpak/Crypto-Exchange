package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cliRuntimeRole is the runtime role of the migrate tests. It is created
// once in the cluster and kept; each schema reset removes its privileges.
const cliRuntimeRole = "testdb_admin_cli_runtime"

func TestParseArgs_Migrate(t *testing.T) {
	for args, want := range map[string][]string{
		"migrate up":     {"migrate"},
		"migrate status": {"migrate", "status"},
	} {
		cmd, err := parseArgs(want, &bytes.Buffer{})
		require.NoError(t, err, "%q", want)
		assert.Equal(t, args, cmd.name)
	}
	cmd, err := parseArgs([]string{"migrate", "up"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "migrate up", cmd.name)

	for _, args := range [][]string{
		{"migrate", "down"},
		{"migrate", "up", "extra"},
		{"migrate", "status", "-force"},
	} {
		_, err := parseArgs(args, &bytes.Buffer{})
		assert.Error(t, err, "%q", args)
	}
}

// writeRolesConfig writes a configuration with the two roles of #180, in
// the external mode, and returns the arguments that select it.
func writeRolesConfig(t *testing.T) []string {
	t.Helper()
	admin, err := sql.Open("pgx", "host=localhost port=5432 user=postgres password=test dbname=postgres sslmode=disable")
	require.NoError(t, err)
	defer admin.Close()
	_, err = admin.Exec(fmt.Sprintf(`DO $$ BEGIN
		CREATE ROLE %s LOGIN PASSWORD 'runtime-test';
	EXCEPTION WHEN duplicate_object THEN NULL;
	END $$`, cliRuntimeRole))
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := fmt.Sprintf(`postgres:
  host: "localhost"
  port: "5432"
  user: %q
  password: "runtime-test"
  dbname: %q
  sslmode: "disable"
  connect_retries: 1
  connect_retry_delay_ms: 10
  migrations:
    mode: "external"
    user: "postgres"
    password: "test"
`, cliRuntimeRole, cliTestDBName)
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	migrations, err := filepath.Abs(filepath.Join("..", "..", "internal", "db", "migrations"))
	require.NoError(t, err)
	return []string{"-config", path, "-migrations", "file://" + filepath.ToSlash(migrations)}
}

// TestRun_MigratesAsAReleaseStep runs the release step of the external
// mode: before it, the status fails and the other commands refuse the
// database; after it, the database is current, the runtime role cannot
// change the schema, and the other commands work with that role.
func TestRun_MigratesAsAReleaseStep(t *testing.T) {
	resetCLITestDB(t)
	base := writeRolesConfig(t)

	code, stdout, stderr := runCLI(t, append(base, "migrate", "status")...)
	assert.Equal(t, 1, code, stdout+stderr)
	assert.Contains(t, stdout, "Schema version: 0")
	assert.Contains(t, stdout, `The database is behind: run "admin migrate up".`)

	code, _, stderr = runCLI(t, append(base, "platform", "status")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, `run "admin migrate" first`, "the other commands never migrate in the external mode")

	code, stdout, stderr = runCLI(t, append(base, "migrate")...)
	require.Equal(t, 0, code, stdout+stderr)
	assert.Contains(t, stdout, "The database is current.")
	assert.Contains(t, stdout, "Runtime role "+cliRuntimeRole+": cannot change the schema.")

	code, stdout, stderr = runCLI(t, append(base, "migrate", "up")...)
	require.Equal(t, 0, code, "running the step again is safe: %s%s", stdout, stderr)

	code, stdout, stderr = runCLI(t, append(base, "platform", "status")...)
	assert.Equal(t, 0, code, "the runtime role runs the commands: %s%s", stdout, stderr)

	conn, err := sql.Open("pgx", fmt.Sprintf("host=localhost port=5432 user=postgres password=test dbname=%s sslmode=disable", cliTestDBName))
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(`GRANT TRUNCATE ON users TO ` + cliRuntimeRole)
	require.NoError(t, err)
	code, stdout, _ = runCLI(t, append(base, "migrate", "status")...)
	assert.Equal(t, 0, code, "outside release mode, a role that can change the schema is only reported")
	assert.Contains(t, stdout, "it has TRUNCATE, REFERENCES, or TRIGGER on tables, such as users")
}
