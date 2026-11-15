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

func TestParseArgs_Verification(t *testing.T) {
	cmd, err := parseArgs([]string{"-operator", "Sara", "verification", "grant", "-user", "alice@example.com",
		"-level", "2", "-reason", "testnet run", "-dry-run"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "verification grant", cmd.name)
	assert.Equal(t, verificationArgs{user: "alice@example.com", level: 2, reason: "testnet run", dryRun: true}, cmd.verification)

	for _, args := range []string{
		"verification",
		"-operator Sara verification revoke -user u -level 0 -reason x",
		"-operator Sara verification grant -level 1 -reason x",
		"-operator Sara verification grant -user u -reason x",
		"-operator Sara verification grant -user u -level 3 -reason x",
		"-operator Sara verification grant -user u -level 1",
		"verification grant -user u -level 1 -reason x",
		"-operator Sara verification grant -user u -level 1 -reason x extra",
	} {
		_, err := parseArgs(strings.Fields(args), &bytes.Buffer{})
		assert.Error(t, err, args)
	}
	_, err = parseArgs([]string{"-operator", "Sara", "verification", "grant", "-user", "u", "-level", "1",
		"-reason", strings.Repeat("x", 501)}, &bytes.Buffer{})
	assert.ErrorContains(t, err, "at most 500 characters")
}

// TestRun_GrantsAVerificationLevel finds a user by email address and by
// mobile number in another form, reports a dry run without writing it,
// sets the level in the sealed row, and audits it with the operator. The
// output names the user by ID, never by contact.
func TestRun_GrantsAVerificationLevel(t *testing.T) {
	resetCLITestDB(t)
	code, _, stderr := runCLI(t, append(append(writeConfig(t), "-operator", "Sara"),
		"verification", "grant", "-user", "alice@example.com", "-level", "2", "-reason", "testnet run")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "keys.provider", "the level is sealed, so the command needs the data keys")

	base := writeLocalKeysConfig(t)
	// The first administrator creates the data keys, as the server's first
	// start would.
	code, _, stderr = runCLI(t, append(base, "create", "-email", "admin@example.com")...)
	require.Equal(t, 0, code, stderr)
	conn, err := sql.Open("pgx", fmt.Sprintf("host=localhost port=5432 user=postgres password=test dbname=%s sslmode=disable", cliTestDBName))
	require.NoError(t, err)
	defer conn.Close()
	var alice string
	require.NoError(t, conn.QueryRow(`INSERT INTO users (status) VALUES ('ACTIVE') RETURNING hk::text`).Scan(&alice))
	_, err = conn.Exec(`INSERT INTO user_contacts (user_hk, type, value) VALUES ($1, 'email', 'alice@example.com'), ($1, 'mobile', '09121234567')`, alice)
	require.NoError(t, err)
	grant := func(user string, extra ...string) (int, string, string) {
		args := append(append(base, "-operator", "Sara"), "verification", "grant", "-user", user, "-level", "2", "-reason", "testnet run")
		return runCLI(t, append(args, extra...)...)
	}
	levelOf := func() (tier, status string, sealed bool) {
		require.NoError(t, conn.QueryRow(`SELECT kyc_tier, kyc_status, row_seal IS NOT NULL FROM users WHERE hk = $1`, alice).
			Scan(&tier, &status, &sealed))
		return tier, status, sealed
	}

	code, stdout, stderr := grant("Alice@Example.com", "-dry-run")
	require.Equal(t, 0, code, stderr)
	assert.Equal(t, fmt.Sprintf("Dry run: user %s would go from level 0 (NONE, PENDING) to level 2 (STANDARD, APPROVED). Nothing was written.\n", alice), stdout)
	tier, status, _ := levelOf()
	assert.Equal(t, []string{"NONE", "PENDING"}, []string{tier, status}, "a dry run writes nothing")

	code, stdout, stderr = grant("+98 912 123 4567")
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, fmt.Sprintf("User %s went from level 0 (NONE, PENDING) to level 2 (STANDARD, APPROVED), audited as user.verification_set.", alice))
	assert.NotContains(t, stdout, "0912", "the output never shows the contact")
	tier, status, sealed := levelOf()
	assert.Equal(t, []string{"STANDARD", "APPROVED"}, []string{tier, status})
	assert.True(t, sealed, "the row is sealed with the new level")

	code, stdout, stderr = grant(alice)
	require.Equal(t, 0, code, stderr)
	assert.Equal(t, fmt.Sprintf("User %s is at level 2 (STANDARD, APPROVED) already; nothing changed.\n", alice), stdout)

	code, _, stderr = grant("bob@example.com")
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "no account has that email")
	code, _, stderr = grant("not a contact")
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "-user must be a user ID, an email address, or a mobile number")

	var events []cliAuditEvent
	for _, e := range cliAuditEvents(t) {
		if e.action == "user.verification_set" {
			events = append(events, e)
		}
	}
	require.Len(t, events, 1, "one change; the dry run, the same level again, and the refusals record nothing")
	assert.Equal(t, "system", events[0].actor)
	assert.Equal(t, alice, events[0].target)
	assert.Regexp(t, `^cmd/admin verification grant; operator=Sara; os_user=`, events[0].agent)
}
