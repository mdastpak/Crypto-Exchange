package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseArgs_Fields(t *testing.T) {
	for _, name := range []string{"status", "rekey", "decrypt"} {
		cmd, err := parseArgs([]string{"fields", name}, &bytes.Buffer{})
		require.NoError(t, err, name)
		assert.Equal(t, "fields "+name, cmd.name)
	}
	for _, args := range []string{"fields", "fields encrypt", "fields status extra", "fields rekey -all"} {
		_, err := parseArgs(strings.Fields(args), &bytes.Buffer{})
		assert.Error(t, err, args)
	}
}

// TestRun_Fields checks that the commands write the personal fields
// encrypted, as the server does, and that the fields commands encrypt the
// rows written without keys.
func TestRun_Fields(t *testing.T) {
	resetCLITestDB(t)
	conn, err := sql.Open("pgx", fmt.Sprintf("host=localhost port=5432 user=postgres password=test dbname=%s sslmode=disable", cliTestDBName))
	require.NoError(t, err)
	defer conn.Close()

	code, _, stderr := runCLI(t, append(writeConfig(t), "fields", "status")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "keys.provider", "the fields need the data keys")

	withKeys := writeLocalKeysConfig(t)
	code, _, stderr = runCLI(t, append(withKeys, "create", "-email", "admin@example.com")...)
	require.Equal(t, 0, code, stderr)
	encryptedOnly := func() int {
		var n int
		require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM user_contacts WHERE value IS NULL AND value_enc IS NOT NULL`).Scan(&n))
		return n
	}
	assert.Equal(t, 1, encryptedOnly(), "the address of the administrator is stored encrypted only")

	// A contact written without keys, as an earlier release wrote it.
	_, err = conn.Exec(`WITH u AS (INSERT INTO users (status) VALUES ('ACTIVE') RETURNING hk)
		INSERT INTO user_contacts (user_hk, type, value, verified_at) SELECT hk, 'email', 'old@example.com', NOW() FROM u`)
	require.NoError(t, err)
	code, stdout, stderr := runCLI(t, append(withKeys, "fields", "status")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "1 contacts, 0 users")

	code, stdout, stderr = runCLI(t, append(withKeys, "fields", "rekey")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "Encrypted 1 contacts and 0 users")
	assert.Equal(t, 2, encryptedOnly())
	code, stdout, _ = runCLI(t, append(withKeys, "fields", "status")...)
	require.Equal(t, 0, code)
	assert.Contains(t, stdout, "Still in plaintext, or encrypted with an older key: 0 contacts, 0 users.")
	assert.Contains(t, stdout, "Stored encrypted only, without plaintext: 2 contacts, 0 users.")

	code, _, stderr = runCLI(t, append(withKeys, "keys", "rotate", "-purpose", "field-enc")...)
	require.Equal(t, 0, code, stderr)
	code, _, stderr = runCLI(t, append(withKeys, "keys", "retire", "-purpose", "field-enc", "-version", "1")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "still used by", "a version that a value uses stays")
}
