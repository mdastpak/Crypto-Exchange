package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/platform"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseArgs_Platform(t *testing.T) {
	cmd, err := parseArgs([]string{"platform", "lockdown", "-feature", "withdrawals", "-reason", "Hot wallet alert",
		"-ends-at", "2030-01-02T03:04:05Z", "-message-en", "Withdrawals are paused"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "platform lockdown", cmd.name)
	assert.Equal(t, platform.KindLockdown, cmd.control.Kind)
	assert.Equal(t, platform.FeatureWithdrawals, cmd.control.Feature)
	assert.Equal(t, time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC), *cmd.control.EndsAt)
	assert.Equal(t, "Withdrawals are paused", cmd.control.Message.EN)

	cmd, err = parseArgs([]string{"platform", "maintenance", "-reason", "Upgrade", "-allow-networks", "10.0.0.0/8, 192.0.2.1"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, platform.KindMaintenance, cmd.control.Kind)
	assert.Equal(t, []string{"10.0.0.0/8", "192.0.2.1"}, cmd.control.AllowNetworks)

	for _, args := range []string{
		"platform", "platform pause", "platform status extra", "platform maintenance",
		"platform lockdown -reason x", "platform lockdown -feature everything -reason x",
		"platform maintenance -reason x -ends-at tomorrow", "platform lift -id x", "platform lift -note x",
	} {
		_, err := parseArgs(strings.Fields(args), &bytes.Buffer{})
		assert.Error(t, err, args)
	}
}

// TestRun_ManagesPlatformControls locks withdrawals, shows the status, and
// lifts the lockdown at once, without a second administrator, with the
// operator in the audit log.
func TestRun_ManagesPlatformControls(t *testing.T) {
	resetCLITestDB(t)
	base := append(writeConfig(t, ""), "-operator", "Sara")

	code, stdout, stderr := runCLI(t, append(base, "platform", "status")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "Mode: open. Locked: none.")

	code, stdout, stderr = runCLI(t, append(base, "platform", "lockdown", "-feature", "withdrawals", "-reason", "Hot wallet alert")...)
	require.Equal(t, 0, code, stderr)
	id := regexp.MustCompile(`[0-9a-f-]{36}`).FindString(stdout)
	require.NotEmpty(t, id, stdout)

	code, stdout, stderr = runCLI(t, append(base, "platform", "status")...)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "Mode: open. Locked: withdrawals.")
	assert.Contains(t, stdout, id)
	assert.Contains(t, stdout, "Hot wallet alert")

	code, _, stderr = runCLI(t, append(base, "platform", "lift", "-id", id, "-note", "Wallet checked")...)
	require.Equal(t, 0, code, stderr)
	code, _, stderr = runCLI(t, append(base, "platform", "lift", "-id", id, "-note", "Again")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "already ended")

	code, _, stderr = runCLI(t, append(base, "platform", "maintenance", "-reason", "x", "-ends-at", "2001-01-01T00:00:00Z")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "ends_at")

	conn, err := sql.Open("pgx", fmt.Sprintf("host=localhost port=5432 user=postgres password=test dbname=%s sslmode=disable", cliTestDBName))
	require.NoError(t, err)
	defer conn.Close()
	var actions []string
	rows, err := conn.Query(`SELECT action || ' ' || actor_type || ' ' || user_agent FROM audit_events WHERE target_id = $1 ORDER BY hk`, id)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var a string
		require.NoError(t, rows.Scan(&a))
		actions = append(actions, a)
	}
	require.Len(t, actions, 2)
	assert.True(t, strings.HasPrefix(actions[0], "platform.control_create system cmd/admin platform lockdown; operator=Sara"), actions[0])
	assert.True(t, strings.HasPrefix(actions[1], "platform.control_lift system cmd/admin platform lift; operator=Sara"), actions[1])
}
