package db

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertCurrency creates a currency, which balances and the networks of
// currencies reference (#309), and returns its hk.
func insertCurrency(t *testing.T, conn *sql.DB, symbol string) string {
	t.Helper()
	var hk string
	require.NoError(t, conn.QueryRow(`INSERT INTO currencies (status, info)
		VALUES ('approved', jsonb_build_object('symbol', $1::text, 'en_name', $1::text, 'fa_name', $1::text,
			'if_fiat', false, 'is_token', false, 'contract', ''))
		RETURNING hk`, symbol).Scan(&hk))
	return hk
}

// insertUserAndCurrency creates a user and a currency that balances can
// reference, and returns their hks.
func insertUserAndCurrency(t *testing.T, conn *sql.DB, symbol string) (string, string) {
	t.Helper()
	var userHK string
	require.NoError(t, conn.QueryRow(`INSERT INTO users (status) VALUES ('ACTIVE') RETURNING hk`).Scan(&userHK))
	return userHK, insertCurrency(t, conn, symbol)
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// TestSchema_EnforcesTheBalanceInvariants checks what the database itself
// refuses on balances and their changes, whatever the application does.
func TestSchema_EnforcesTheBalanceInvariants(t *testing.T) {
	m := newMigrationsTestMigrator(t)
	require.NoError(t, m.Up())
	conn, err := sql.Open("pgx", migrationsTestDBConnStr())
	require.NoError(t, err)
	defer conn.Close()
	user, currency := insertUserAndCurrency(t, conn, "BTC")

	var balanceID, total string
	require.NoError(t, conn.QueryRow(`INSERT INTO balances (user_id, currency_hk, available, locked, pending_deposit)
		VALUES ($1, $2, 1.5, 0.25, 7) RETURNING id, total`, user, currency).Scan(&balanceID, &total))
	assert.Equal(t, "1.75", total, "total is available + locked; pending deposits are not owned yet")
	_, err = conn.Exec(`UPDATE balances SET locked = locked + 1 WHERE id = $1`, balanceID)
	require.NoError(t, err)
	require.NoError(t, conn.QueryRow(`SELECT total FROM balances WHERE id = $1`, balanceID).Scan(&total))
	assert.Equal(t, "2.75", total, "the database keeps total in step")

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

	_, err = conn.Exec(`INSERT INTO balances (user_id, currency_hk) VALUES ($1, $2)`, user, currency)
	assert.Equal(t, "23505", pgCode(err), "a second balance of the currency without a sub-account: %v", err)

	insertChange := func(amount, newAvailable string) error {
		_, err := conn.Exec(`INSERT INTO balance_changes (balance_id, user_id, currency_hk, change_type, amount,
			old_available, new_available, old_locked, new_locked, old_pending, new_pending)
			VALUES ($1, $2, $3, 'LOCK', $4, 1, $5, 0, 1, 0, 0)`, balanceID, user, currency, amount, newAvailable)
		return err
	}
	require.NoError(t, insertChange("1", "0"))
	assert.Equal(t, "23514", pgCode(insertChange("0", "1")), "a change of nothing")
	assert.Equal(t, "23514", pgCode(insertChange("2", "-1")), "a change that ends below zero")

	var onChain bool
	require.NoError(t, conn.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'balances' AND column_name = 'on_chain')`).Scan(&onChain))
	assert.False(t, onChain)
}
