package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseArgs_Recovery(t *testing.T) {
	cmd, err := parseArgs([]string{"backup", "status"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "backup status", cmd.name)
	assert.Equal(t, DefaultRPO, cmd.recovery.rpo)

	cmd, err = parseArgs([]string{"backup", "status", "-rpo", "15m"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, 15*time.Minute, cmd.recovery.rpo)

	cmd, err = parseArgs([]string{"restore", "verify", "-not-before", "2026-11-08T10:00:00Z",
		"-not-after", "2026-11-08T10:01:00.5Z", "-keys"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "restore verify", cmd.name)
	assert.Equal(t, time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC), *cmd.recovery.notBefore)
	assert.Equal(t, time.Date(2026, 11, 8, 10, 1, 0, 5e8, time.UTC), *cmd.recovery.notAfter)
	assert.True(t, cmd.recovery.keys)

	for _, args := range []string{
		"backup", "backup start", "backup status -rpo 0s", "backup status extra",
		"restore", "restore now", "restore verify -not-before yesterday",
		"restore verify -not-before 2026-11-08T10:00:00Z -not-after 2026-11-08T09:00:00Z",
	} {
		_, err := parseArgs(strings.Fields(args), &bytes.Buffer{})
		assert.Error(t, err, args)
	}
}

// TestRun_ChecksBackupsAndRestores runs both commands against the test
// database: the test server does not archive WAL, so backup status fails,
// and a migrated database passes restore verify until it is broken.
func TestRun_ChecksBackupsAndRestores(t *testing.T) {
	resetCLITestDB(t)
	base := writeConfig(t, "")
	code, _, stderr := runCLI(t, append(base, "platform", "status")...)
	require.Equal(t, 0, code, "migrate the database: %s", stderr)

	code, stdout, stderr := runCLI(t, append(base, "backup", "status")...)
	assert.Equal(t, 1, code, "the test server does not archive WAL: %s%s", stdout, stderr)
	assert.Contains(t, stdout, "archive_mode:")
	assert.Contains(t, stdout, "FAIL")

	code, stdout, stderr = runCLI(t, append(base, "restore", "verify")...)
	require.Equal(t, 0, code, stdout+stderr)
	assert.Contains(t, stdout, "ok   schema: version")
	assert.Contains(t, stdout, "OK: the restored database passed every check.")

	code, stdout, _ = runCLI(t, append(base, "restore", "verify", "-not-before", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stdout, "FAIL point in time")

	conn, err := sql.Open("pgx", fmt.Sprintf("host=localhost port=5432 user=postgres password=test dbname=%s sslmode=disable", cliTestDBName))
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(`UPDATE schema_migrations SET dirty = true`)
	require.NoError(t, err)
	code, stdout, _ = runCLI(t, append(base, "restore", "verify")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stdout, "FAIL schema")
	assert.Contains(t, stdout, "do not put it in service")
	var dirty bool
	require.NoError(t, conn.QueryRow(`SELECT dirty FROM schema_migrations`).Scan(&dirty))
	assert.True(t, dirty, "restore verify never migrates or repairs the database")

	code, _, stderr = runCLI(t, append(base, "restore", "verify", "-keys")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "keys.provider")
}
