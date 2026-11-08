package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertCurrencyRow stores a currency with symbol as given, as rows written
// before #107 may hold it, and returns its hk.
func insertCurrencyRow(t *testing.T, conn *sql.DB, symbol, status string) string {
	t.Helper()
	var hk string
	require.NoError(t, conn.QueryRow(`INSERT INTO currencies (status, info) VALUES ($2,
		jsonb_build_object('en_name', $1::text, 'fa_name', $1::text, 'symbol', $1::text,
			'if_fiat', false, 'is_token', false, 'contract', 'c-' || $1::text))
		RETURNING hk`, symbol, status).Scan(&hk))
	return hk
}

// withoutReservations runs insert with the trigger of migration 29 turned
// off, as rows written before that migration were.
func withoutReservations(t *testing.T, conn *sql.DB, insert func()) {
	t.Helper()
	_, err := conn.Exec(`ALTER TABLE currencies DISABLE TRIGGER currency_symbols_follow`)
	require.NoError(t, err)
	insert()
	_, err = conn.Exec(`ALTER TABLE currencies ENABLE TRIGGER currency_symbols_follow`)
	require.NoError(t, err)
}

func symbolOf(t *testing.T, conn *sql.DB, hk string) (string, int64) {
	t.Helper()
	var symbol string
	var version int64
	require.NoError(t, conn.QueryRow(`SELECT info->>'symbol', version FROM currencies WHERE hk = $1`, hk).Scan(&symbol, &version))
	return symbol, version
}

func TestMigration36_UpperCasesTheSymbols(t *testing.T) {
	m := newMigrationsTestMigrator(t)
	require.NoError(t, m.Migrate(35))
	conn, err := sql.Open("pgx", migrationsTestDBConnStr())
	require.NoError(t, err)
	defer conn.Close()

	lower := insertCurrencyRow(t, conn, "usdt", "approved")
	mixed := insertCurrencyRow(t, conn, "Dot", "deleted")
	upper := insertCurrencyRow(t, conn, "BTC", "approved")
	// A row of before migration 29 that has no reservation of its symbol.
	_, err = conn.Exec(`DELETE FROM currency_symbols WHERE currency_hk = $1`, lower)
	require.NoError(t, err)

	require.NoError(t, m.Migrate(36))

	for hk, want := range map[string]struct {
		symbol  string
		version int64
	}{lower: {"USDT", 2}, mixed: {"DOT", 2}, upper: {"BTC", 1}} {
		symbol, version := symbolOf(t, conn, hk)
		assert.Equal(t, want.symbol, symbol)
		assert.Equal(t, want.version, version, "%s: only a changed row gets a new version", want.symbol)
	}

	var reservations int
	require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM currency_symbols WHERE currency_hk = $1 AND symbol = 'USDT' AND current`, lower).Scan(&reservations))
	assert.Equal(t, 1, reservations, "a currency without a reservation gets its own")
	require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM currency_symbols WHERE current`).Scan(&reservations))
	assert.Equal(t, 3, reservations, "one current reservation per currency")

	_, err = conn.Exec(`INSERT INTO currencies (info) VALUES ('{"en_name": "x", "fa_name": "x", "symbol": "usdt", "if_fiat": false, "is_token": false, "contract": "u"}')`)
	assert.Equal(t, "23514", pgCode(err), "a lower-case symbol is refused: %v", err)
	_, err = conn.Exec(`UPDATE currencies SET info = jsonb_set(info, '{symbol}', '"btc"') WHERE hk = $1`, upper)
	assert.Equal(t, "23514", pgCode(err), "an update to a lower-case symbol is refused: %v", err)

	// Rolling back keeps the upper-case symbols and allows any case again.
	require.NoError(t, m.Migrate(35))
	symbol, _ := symbolOf(t, conn, lower)
	assert.Equal(t, "USDT", symbol)
	insertCurrencyRow(t, conn, "eth", "pending")
	require.NoError(t, m.Migrate(36), "migrating up again works on the rolled-back data")
	var eth string
	require.NoError(t, conn.QueryRow(`SELECT info->>'symbol' FROM currencies WHERE info->>'en_name' = 'eth'`).Scan(&eth))
	assert.Equal(t, "ETH", eth)
}

func TestMigration36_PreflightStopsOnSymbolsItCannotMerge(t *testing.T) {
	for name, tc := range map[string]struct {
		setup   func(t *testing.T, conn *sql.DB)
		wantErr string
	}{
		"symbols that differ only in case": {
			setup: func(t *testing.T, conn *sql.DB) {
				insertCurrencyRow(t, conn, "USDT", "approved")
				withoutReservations(t, conn, func() { insertCurrencyRow(t, conn, "usdt", "pending") })
			},
			wantErr: "differ only in case: USDT, usdt",
		},
		"a deleted currency counts": {
			setup: func(t *testing.T, conn *sql.DB) {
				insertCurrencyRow(t, conn, "Xrp", "deleted")
				withoutReservations(t, conn, func() { insertCurrencyRow(t, conn, "xrp", "approved") })
			},
			wantErr: "differ only in case: Xrp, xrp",
		},
		"a symbol reserved by another currency": {
			setup: func(t *testing.T, conn *sql.DB) {
				// MATIC was rebranded to POL, so MATIC is reserved for
				// that currency.
				pol := insertCurrencyRow(t, conn, "MATIC", "approved")
				_, err := conn.Exec(`UPDATE currencies SET info = jsonb_set(info, '{symbol}', '"POL"') WHERE hk = $1`, pol)
				require.NoError(t, err)
				withoutReservations(t, conn, func() { insertCurrencyRow(t, conn, "matic", "approved") })
			},
			wantErr: "reserved by another currency once in upper case: matic",
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := newMigrationsTestMigrator(t)
			require.NoError(t, m.Migrate(35))
			conn, err := sql.Open("pgx", migrationsTestDBConnStr())
			require.NoError(t, err)
			defer conn.Close()
			tc.setup(t, conn)

			err = m.Migrate(36)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Contains(t, err.Error(), `run "migrate force 35"`)
			var changed int
			require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM currencies WHERE info->>'symbol' <> upper(info->>'symbol')`).Scan(&changed))
			assert.Positive(t, changed, "a failed preflight changes no symbol")
			var constraint bool
			require.NoError(t, conn.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'currencies_symbol_upper_case')`).Scan(&constraint))
			assert.False(t, constraint, "a failed preflight leaves the schema unchanged")
		})
	}
}
