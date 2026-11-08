package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/adminauth"
	"github.com/mdastpak/crypto-exchange/internal/keys"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseArgs_TOTPSecrets(t *testing.T) {
	for args, want := range map[string]command{
		"totp-secrets status":               {name: "totp-secrets status"},
		"totp-secrets reseal -to data-keys": {name: "totp-secrets reseal", format: adminauth.FormatDataKeys},
		"totp-secrets reseal -to legacy":    {name: "totp-secrets reseal", format: adminauth.FormatLegacy},
	} {
		cmd, err := parseArgs(strings.Fields(args), &bytes.Buffer{})
		require.NoError(t, err, args)
		want.configPath, want.migrationsPath = "config/config.yaml", "file://internal/db/migrations"
		assert.Equal(t, want, cmd, args)
	}
	for _, args := range []string{
		"totp-secrets",
		"totp-secrets move",
		"totp-secrets reseal",
		"totp-secrets reseal -to plain",
		"totp-secrets status -to legacy",
		"totp-secrets status extra",
	} {
		_, err := parseArgs(strings.Fields(args), &bytes.Buffer{})
		assert.Error(t, err, args)
	}
}

// writeTOTPConfig writes a configuration with admin.totp_key (empty for
// none), the local key provider with kek, and keys.legacy_secrets.
func writeTOTPConfig(t *testing.T, totpKey, kek, legacySecrets string) []string {
	t.Helper()
	args := writeConfig(t, totpKey)
	f, err := os.OpenFile(args[1], os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = fmt.Fprintf(f, "keys:\n  provider: \"local\"\n  local:\n    kek: %q\n  legacy_secrets: %q\n", kek, legacySecrets)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return args
}

func storedTOTPFormats(t *testing.T) []string {
	t.Helper()
	conn, err := sql.Open("pgx", fmt.Sprintf("host=localhost port=5432 user=postgres password=test dbname=%s sslmode=disable", cliTestDBName))
	require.NoError(t, err)
	defer conn.Close()
	rows, err := conn.Query(`SELECT left(totp_secret, 3) FROM users WHERE totp_secret IS NOT NULL ORDER BY hk`)
	require.NoError(t, err)
	defer rows.Close()
	var formats []string
	for rows.Next() {
		var f string
		require.NoError(t, rows.Scan(&f))
		formats = append(formats, f)
	}
	return formats
}

// TestRun_MovesTOTPSecretsToTheDataKeys runs the steps of
// docs/data-protection.md#moving-off-the-legacy-secrets from the command line.
func TestRun_MovesTOTPSecretsToTheDataKeys(t *testing.T) {
	resetCLITestDB(t)
	random := func() string {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		return base64.StdEncoding.EncodeToString(b)
	}
	totpKey, kek := random(), random()
	writing := writeTOTPConfig(t, totpKey, kek, "write")

	// Before the server has created the data keys, admin.totp_key seals.
	code, _, stderr := runCLI(t, append(writing, "create", "-email", "first@example.com")...)
	require.Equal(t, 0, code, stderr)
	code, _, stderr = runCLI(t, append(writing, "totp-secrets", "reseal", "-to", "data-keys")...)
	assert.Equal(t, 1, code, "no data keys yet")
	assert.Contains(t, stderr, "sealing with the data keys needs keys.provider")

	// The server's first start creates the data keys.
	conn, err := sql.Open("pgx", fmt.Sprintf("host=localhost port=5432 user=postgres password=test dbname=%s sslmode=disable", cliTestDBName))
	require.NoError(t, err)
	defer conn.Close()
	provider, err := keys.NewLocalProvider(kek)
	require.NoError(t, err)
	_, err = (&keys.Store{DB: conn, Provider: provider}).Load(context.Background())
	require.NoError(t, err)

	code, _, stderr = runCLI(t, append(writing, "create", "-email", "second@example.com")...)
	require.Equal(t, 0, code, stderr)
	assert.Equal(t, []string{"v1:", "v1:"}, storedTOTPFormats(t), "legacy_secrets write keeps sealing with admin.totp_key")

	code, stdout, stderr := runCLI(t, append(writing, "totp-secrets", "status")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "Sealed with admin.totp_key: 2")
	assert.Contains(t, stdout, "Sealed with the data keys:  0")
	assert.Contains(t, stdout, "New secrets are sealed with admin.totp_key.")

	reading := writeTOTPConfig(t, totpKey, kek, "read")
	code, stdout, stderr = runCLI(t, append(reading, "totp-secrets", "reseal", "-to", "data-keys")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "Sealed 2 authenticator secret(s) again with the data keys.")
	assert.Equal(t, []string{"v2:", "v2:"}, storedTOTPFormats(t))
	code, stdout, stderr = runCLI(t, append(reading, "totp-secrets", "status")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "New secrets are sealed with the data keys.")
	assert.Contains(t, stdout, "it can be removed from the configuration")

	// Without admin.totp_key, the commands work with the data keys alone.
	dataOnly := writeTOTPConfig(t, "", kek, "read")
	code, _, stderr = runCLI(t, append(dataOnly, "reset-totp", "-email", "first@example.com")...)
	require.Equal(t, 0, code, stderr)
	code, _, stderr = runCLI(t, append(dataOnly, "totp-secrets", "reseal", "-to", "legacy")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "needs admin.totp_key")

	// The way back, before a rollback.
	code, stdout, stderr = runCLI(t, append(reading, "totp-secrets", "reseal", "-to", "legacy")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "Sealed 2 authenticator secret(s) again with admin.totp_key.")
	assert.Equal(t, []string{"v1:", "v1:"}, storedTOTPFormats(t))

	var actions []string
	for _, e := range cliAuditEvents(t) {
		actions = append(actions, e.action)
	}
	assert.Equal(t, 4, strings.Count(strings.Join(actions, " "), "admin.totp_resealed"), actions)
}

func TestRun_TOTPCommandsNeedAKey(t *testing.T) {
	resetCLITestDB(t)
	code, _, stderr := runCLI(t, append(writeConfig(t, ""), "totp-secrets", "status")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "keys.provider")
	assert.Contains(t, stderr, "admin.totp_key")
}
