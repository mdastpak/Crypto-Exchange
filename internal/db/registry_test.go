package db

import (
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// canonical are the tables of the one registry of the assets that the
// exchange supports (#309): the currencies, and how each moves on a
// network.
var canonical = map[string]bool{"currencies": true, "currency_networks": true}

// inCatalog says whether a table belongs to the market data catalog, the
// unverified global list of assets (#262). Its tables reference each other
// by the catalog's own asset IDs, and link to a currency by one column,
// market_assets.currency_hk.
func inCatalog(table string) bool { return strings.HasPrefix(table, "market_") }

// TestSchema_AssetsHaveOneRegistry fails when a migration brings back a
// second registry of assets: a table of assets outside the catalog, or a
// foreign key from the exchange's tables to an asset that is not a
// currency. Balances, markets, orders, trades, custody, restrictions,
// favorites, and delistings must all mean the same row by "BTC".
func TestSchema_AssetsHaveOneRegistry(t *testing.T) {
	conn := pgtest.Migrated(t, "testdb_db_registry")

	tables, err := conn.Query(`SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename LIKE '%asset%'`)
	require.NoError(t, err)
	defer tables.Close()
	for tables.Next() {
		var name string
		require.NoError(t, tables.Scan(&name))
		assert.True(t, inCatalog(name), "table %s holds assets outside the market data catalog; the exchange's assets are currencies (#309)", name)
	}
	require.NoError(t, tables.Err())

	rows, err := conn.Query(`
		SELECT c.conrelid::regclass::text, a.attname, c.confrelid::regclass::text
		FROM pg_constraint c
		CROSS JOIN LATERAL unnest(c.conkey) AS k(attnum)
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
		WHERE c.contype = 'f' AND c.connamespace = 'public'::regnamespace`)
	require.NoError(t, err)
	defer rows.Close()
	checked := 0
	for rows.Next() {
		var table, column, referenced string
		require.NoError(t, rows.Scan(&table, &column, &referenced))
		if inCatalog(table) && inCatalog(referenced) {
			continue
		}
		names := strings.Contains(column, "asset") || strings.Contains(column, "currency")
		switch {
		case strings.Contains(referenced, "asset"):
			assert.Fail(t, "a second registry of assets",
				"%s.%s references %s; outside the market data catalog, an asset is a currency (#309)", table, column, referenced)
		case names && !canonical[referenced]:
			assert.Fail(t, "a second registry of assets",
				"%s.%s names an asset but references %s, not currencies or currency_networks (#309)", table, column, referenced)
		case canonical[referenced]:
			checked++
		}
	}
	require.NoError(t, rows.Err())
	assert.GreaterOrEqual(t, checked, 20, "the references to currencies are found")
}
