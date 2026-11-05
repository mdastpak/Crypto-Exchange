package db

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertUserAndAsset creates a user and an asset that balances can
// reference, and returns their IDs.
func insertUserAndAsset(t *testing.T, conn *sql.DB, symbol string) (string, string) {
	t.Helper()
	var userHK, assetID string
	require.NoError(t, conn.QueryRow(`INSERT INTO users (status) VALUES ('ACTIVE') RETURNING hk`).Scan(&userHK))
	require.NoError(t, conn.QueryRow(`INSERT INTO assets (symbol, name, type) VALUES ($1, $1, 'COIN') RETURNING id`, symbol).Scan(&assetID))
	return userHK, assetID
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestMigration17_EnforcesTheBalanceInvariants(t *testing.T) {
	m := newMigrationsTestMigrator(t)
	require.NoError(t, m.Migrate(17))
	conn, err := sql.Open("pgx", migrationsTestDBConnStr())
	require.NoError(t, err)
	defer conn.Close()
	user, asset := insertUserAndAsset(t, conn, "BTC")

	var balanceID, total string
	require.NoError(t, conn.QueryRow(`INSERT INTO balances (user_id, asset_id, available, locked, pending_deposit)
		VALUES ($1, $2, 1.5, 0.25, 7) RETURNING id, total`, user, asset).Scan(&balanceID, &total))
	assert.Equal(t, "1.750000000000000000", total, "total is available + locked; pending deposits are not owned yet")
	_, err = conn.Exec(`UPDATE balances SET locked = locked + 1 WHERE id = $1`, balanceID)
	require.NoError(t, err)
	require.NoError(t, conn.QueryRow(`SELECT total FROM balances WHERE id = $1`, balanceID).Scan(&total))
	assert.Equal(t, "2.750000000000000000", total, "the database keeps total in step")

	_, err = conn.Exec(`UPDATE balances SET total = 100 WHERE id = $1`, balanceID)
	assert.Error(t, err, "total cannot be written")
	for name, update := range map[string]string{
		"negative available": `UPDATE balances SET available = -0.000000000000000001 WHERE id = $1`,
		"negative locked":    `UPDATE balances SET locked = -1 WHERE id = $1`,
		"negative pending":   `UPDATE balances SET pending_deposit = -1 WHERE id = $1`,
	} {
		_, err := conn.Exec(update, balanceID)
		assert.Equal(t, "23514", pgCode(err), "%s: %v", name, err)
	}

	_, err = conn.Exec(`INSERT INTO balances (user_id, asset_id) VALUES ($1, $2)`, user, asset)
	assert.Equal(t, "23505", pgCode(err), "a second balance of the asset without a sub-account: %v", err)

	insertChange := func(amount, newAvailable string) error {
		_, err := conn.Exec(`INSERT INTO balance_changes (balance_id, user_id, asset_id, change_type, amount,
			old_available, new_available, old_locked, new_locked, old_pending, new_pending)
			VALUES ($1, $2, $3, 'LOCK', $4, 1, $5, 0, 1, 0, 0)`, balanceID, user, asset, amount, newAvailable)
		return err
	}
	require.NoError(t, insertChange("1", "0"))
	assert.Equal(t, "23514", pgCode(insertChange("0", "1")), "a change of nothing")
	assert.Equal(t, "23514", pgCode(insertChange("2", "-1")), "a change that ends below zero")

	var onChain bool
	require.NoError(t, conn.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'balances' AND column_name = 'on_chain')`).Scan(&onChain))
	assert.False(t, onChain)

	// Rolling back keeps the balances, with total as a plain column again.
	require.NoError(t, m.Migrate(16))
	require.NoError(t, conn.QueryRow(`SELECT total FROM balances WHERE id = $1`, balanceID).Scan(&total))
	assert.Equal(t, "2.750000000000000000", total)
	require.NoError(t, m.Migrate(17), "migrating up again works on the rolled-back data")
}

func TestMigration17_PreflightStopsOnDataItCannotKeep(t *testing.T) {
	for name, tc := range map[string]struct {
		insert  string
		wantErr string
	}{
		"on-chain amount": {
			insert:  `INSERT INTO balances (user_id, asset_id, on_chain) VALUES ($1, $2, 1)`,
			wantErr: "hold on-chain amounts",
		},
		"negative amount": {
			insert:  `INSERT INTO balances (user_id, asset_id, available) VALUES ($1, $2, -1)`,
			wantErr: "negative amounts",
		},
		"duplicate balance without a sub-account": {
			insert:  `INSERT INTO balances (user_id, asset_id) VALUES ($1, $2), ($1, $2)`,
			wantErr: "several balances of one asset",
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := newMigrationsTestMigrator(t)
			require.NoError(t, m.Migrate(16))
			conn, err := sql.Open("pgx", migrationsTestDBConnStr())
			require.NoError(t, err)
			defer conn.Close()
			user, asset := insertUserAndAsset(t, conn, "ETH")
			_, err = conn.Exec(tc.insert, user, asset)
			require.NoError(t, err)

			err = m.Migrate(17)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Contains(t, err.Error(), `run "migrate force 16"`)
			var onChain bool
			require.NoError(t, conn.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'balances' AND column_name = 'on_chain')`).Scan(&onChain))
			assert.True(t, onChain, "a failed preflight leaves the schema unchanged")
		})
	}
}
