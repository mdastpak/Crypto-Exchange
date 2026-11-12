package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseArgs_Assets(t *testing.T) {
	cmd, err := parseArgs(strings.Fields("assets import -file x.yaml -dry-run"), &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "assets import", cmd.name)
	assert.Equal(t, assetArgs{file: "x.yaml", dryRun: true}, cmd.assets)
	for _, args := range []string{"assets", "assets export", "assets import extra", "assets import -force"} {
		_, err := parseArgs(strings.Fields(args), &bytes.Buffer{})
		assert.Error(t, err, args)
	}
}

// TestRun_AssetsImport imports the registry file of the repository: a dry
// run first, which writes nothing, then the import, which a second run
// finds done.
func TestRun_AssetsImport(t *testing.T) {
	resetCLITestDB(t)
	conn, err := sql.Open("pgx", fmt.Sprintf("host=localhost port=5432 user=postgres password=test dbname=%s sslmode=disable", cliTestDBName))
	require.NoError(t, err)
	defer conn.Close()
	file := filepath.Join("..", "..", "config", "assets.yaml")
	currencies := func() int {
		var n int
		require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM currencies`).Scan(&n))
		return n
	}

	code, stdout, stderr := runCLI(t, append(writeConfig(t), "assets", "import", "-file", file, "-dry-run")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "Dry run, nothing written")
	assert.Zero(t, currencies())

	code, stdout, stderr = runCLI(t, append(writeConfig(t), "-operator", "alice", "assets", "import", "-file", file)...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "Imported")
	assert.Contains(t, stdout, "Markets: 8 created")
	assert.Equal(t, 9, currencies())
	var agent string
	require.NoError(t, conn.QueryRow(`SELECT user_agent FROM audit_events WHERE action = 'currency.create' LIMIT 1`).Scan(&agent))
	assert.Contains(t, agent, "operator=alice")

	code, stdout, stderr = runCLI(t, append(writeConfig(t), "assets", "import", "-file", file)...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "0 created")
	assert.Contains(t, stdout, "Markets: 0 created, 0 updated, 8 unchanged.")
}
