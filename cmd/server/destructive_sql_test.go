package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// destructiveSQL matches the statements that empty or remove database
// objects. The SQL of this module is written in upper case.
var destructiveSQL = regexp.MustCompile(`\b(DROP\s+(TABLE|SCHEMA|TYPE|DATABASE)|TRUNCATE\s)`)

// TestCommands_ContainNoDestructiveSQL checks the Go code that the server and
// the admin command are built from. Only the migrations, which are separate
// SQL files, may remove database objects, and the test helpers that empty
// databases must never be compiled into a command, where they could be run
// against a real database by mistake (#31).
func TestCommands_ContainNoDestructiveSQL(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go tool is not available")
	}
	out, err := exec.Command(goTool, "list", "-deps", "-f", "{{.ImportPath}}|{{.Dir}}|{{join .GoFiles \",\"}}",
		"encrypted-db/cmd/server", "encrypted-db/cmd/admin").CombinedOutput()
	require.NoError(t, err, string(out))

	checked := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), "|", 3)
		if len(fields) != 3 || !strings.HasPrefix(fields[0], "encrypted-db/") {
			continue
		}
		assert.NotContains(t, fields[0], "/testutil/", "a command must not depend on a test helper")
		for _, name := range strings.Split(fields[2], ",") {
			if name == "" {
				continue
			}
			source, err := os.ReadFile(filepath.Join(fields[1], name))
			require.NoError(t, err)
			checked++
			if m := destructiveSQL.Find(source); m != nil {
				t.Errorf("%s/%s contains %q", fields[0], name, m)
			}
		}
	}
	assert.Greater(t, checked, 20, "the commands and their packages must be checked")
}

func TestDestructiveSQL_Pattern(t *testing.T) {
	for _, statement := range []string{"DROP TABLE IF EXISTS users", "DROP SCHEMA public CASCADE", "DROP TYPE USER_STATUS", "TRUNCATE TABLE currencies", "TRUNCATE users"} {
		assert.True(t, destructiveSQL.MatchString(statement), statement)
	}
	for _, code := range []string{"d.Truncate(2)", "DROP INDEX IF EXISTS idx", "dropTable"} {
		assert.False(t, destructiveSQL.MatchString(code), code)
	}
}
