package db

import (
	"database/sql"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSchema_ASubAccountBelongsToItsUser checks that a row naming a user
// and a sub-account names a sub-account of that user (migration 20), and
// that the actions on delete stay as they were.
func TestSchema_ASubAccountBelongsToItsUser(t *testing.T) {
	conn := pgtest.Migrated(t, "testdb_db_sub_accounts")
	user := func() string {
		var hk string
		require.NoError(t, conn.QueryRow(`INSERT INTO users (status) VALUES ('ACTIVE') RETURNING hk::text`).Scan(&hk))
		return hk
	}
	alice, bob := user(), user()
	var trading string
	require.NoError(t, conn.QueryRow(`INSERT INTO sub_accounts (user_id, label) VALUES ($1, 'trading') RETURNING id::text`, alice).Scan(&trading))
	currency := insertCurrency(t, conn, "USDT")

	_, err := conn.Exec(`INSERT INTO balances (user_id, sub_account_id, currency_hk) VALUES ($1, $2, $3)`, alice, trading, currency)
	require.NoError(t, err, "a balance in a sub-account of its user")
	_, err = conn.Exec(`INSERT INTO balances (user_id, sub_account_id, currency_hk) VALUES ($1, $2, $3)`, bob, trading, currency)
	assert.Equal(t, "23503", pgCode(err), "a balance of bob in alice's sub-account: %v", err)
	_, err = conn.Exec(`INSERT INTO balances (user_id, currency_hk) VALUES ($1, $2)`, bob, currency)
	require.NoError(t, err, "a main account is not checked")

	// A session of a deleted sub-account goes on as one of the user's main
	// account; a deposit address of it goes with it.
	var session string
	require.NoError(t, conn.QueryRow(`INSERT INTO sessions (user_id, sub_account_id, auth_method, access_token_hash, refresh_token_hash, expires_at)
		VALUES ($1, $2, 'otp', 'a', 'r', NOW() + interval '1 hour') RETURNING id::text`, alice, trading).Scan(&session))
	_, err = conn.Exec(`INSERT INTO deposit_addresses (user_id, sub_account_id, currency_hk, address) VALUES ($1, $2, $3, 'T1')`,
		alice, trading, currency)
	require.NoError(t, err)
	_, err = conn.Exec(`DELETE FROM balances WHERE sub_account_id = $1`, trading)
	require.NoError(t, err)
	_, err = conn.Exec(`DELETE FROM sub_accounts WHERE id = $1`, trading)
	require.NoError(t, err)
	var sessionUser string
	var sessionSub sql.NullString
	require.NoError(t, conn.QueryRow(`SELECT user_id::text, sub_account_id::text FROM sessions WHERE id = $1`, session).Scan(&sessionUser, &sessionSub))
	assert.Equal(t, alice, sessionUser, "the user stays")
	assert.False(t, sessionSub.Valid, "only the sub-account is cleared")
	var addresses int
	require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM deposit_addresses WHERE user_id = $1`, alice).Scan(&addresses))
	assert.Zero(t, addresses, "the addresses of the sub-account go with it")
}
