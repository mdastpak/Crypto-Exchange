package rowseal_test

import (
	"context"
	"os"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigration48_KeepsTheSealsOfUsers checks that the seals of users written
// while deleted_at was a TIMESTAMP stay valid once migration 48 makes it a
// TIMESTAMPTZ (#315): the stored wall time, read as UTC, has the same
// canonical text.
func TestMigration48_KeepsTheSealsOfUsers(t *testing.T) {
	e := newEnv(t)
	sealer := e.sealer(rowseal.ModeEnforce)
	ctx := context.Background()
	migration := func(name string) {
		sql, err := os.ReadFile("../db/migrations/48_users_timestamptz." + name + ".sql")
		require.NoError(t, err)
		_, err = e.conn.Exec(string(sql))
		require.NoError(t, err)
	}

	// The table as it was before the migration, and its seal of deleted_at:
	// the stored wall time.
	migration("down")
	before := *rowseal.Users
	before.Columns = append([]rowseal.Column(nil), rowseal.Users.Columns...)
	for i, c := range before.Columns {
		if c.Name == "deleted_at" {
			before.Columns[i].Expr = `to_char(%[1]sdeleted_at, 'YYYY-MM-DD"T"HH24:MI:SS.US')`
		}
	}
	deleted := usertest.Insert(t, e.conn, usertest.User{Deleted: true})
	active := usertest.Insert(t, e.conn, usertest.User{})
	require.NoError(t, rowseal.Reseal(ctx, e.conn, &before, "hk IN ($1, $2)", deleted, active))
	// A row without a creation time, as migration 2 allowed.
	_, err := e.conn.Exec(`UPDATE users SET created_at = NULL WHERE hk = $1`, active)
	require.NoError(t, err)

	migration("up")

	for _, hk := range []string{deleted, active} {
		require.NoError(t, rowseal.Check(ctx, rowseal.Users, e.readUser(t, hk)))
	}
	report, err := sealer.VerifyAll(ctx)
	require.NoError(t, err)
	assert.Zero(t, report.Findings)
	assert.Empty(t, e.openFindings(t))

	var created bool
	require.NoError(t, e.conn.QueryRow(`SELECT created_at IS NOT NULL FROM users WHERE hk = $1`, active).Scan(&created))
	assert.True(t, created, "a row without a creation time gets one")
}
