package currencies

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/assetregistry"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registryFile loads the registry file of the repository.
func registryFile(t *testing.T) *assetregistry.Registry {
	t.Helper()
	reg, err := assetregistry.Load(filepath.Join("..", "..", "config", "assets.yaml"))
	require.NoError(t, err)
	return reg
}

var importOrigin = audit.Event{ActorType: audit.ActorSystem, UserAgent: "cmd/admin assets import; test"}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(query, args...).Scan(&n))
	return n
}

func TestImport_LoadsTheRegistryFile(t *testing.T) {
	ctx := context.Background()
	db := pgtest.Migrated(t, testDBName)
	store := Store{DB: db}
	reg := registryFile(t)

	t.Run("a dry run writes nothing", func(t *testing.T) {
		report, err := store.Import(ctx, reg, importOrigin, true)
		require.NoError(t, err)
		assert.Len(t, report.Created, len(reg.Assets))
		assert.Positive(t, report.Networks.Created)
		assert.Equal(t, len(reg.Markets), report.Markets.Created)
		assert.Zero(t, count(t, db, `SELECT COUNT(*) FROM currencies`))
		assert.Zero(t, count(t, db, `SELECT COUNT(*) FROM audit_events`))
	})

	report, err := store.Import(ctx, reg, importOrigin, false)
	require.NoError(t, err)
	assert.Len(t, report.Created, len(reg.Assets))
	networks := 0
	for _, a := range reg.Assets {
		networks += len(a.Networks)
	}
	assert.Equal(t, ImportCounts{Created: networks}, report.Networks)
	assert.Equal(t, ImportCounts{Created: len(reg.Markets)}, report.Markets)

	// USDT is one currency, a token, with a network on TRON and one on
	// Ethereum, each with its contract and the currency of its fee.
	var hk, status string
	var isToken bool
	require.NoError(t, db.QueryRow(`SELECT hk, status, (info->>'is_token')::boolean FROM currencies WHERE info->>'symbol' = 'USDT'`).
		Scan(&hk, &status, &isToken))
	assert.Equal(t, "approved", status, "ACTIVE in the file")
	assert.True(t, isToken)
	rows, err := db.Query(`SELECT n.network, n.contract_address, n.decimals, f.info->>'symbol'
		FROM currency_networks n JOIN currencies f ON f.hk = n.fee_currency_hk WHERE n.currency_hk = $1 ORDER BY n.network`, hk)
	require.NoError(t, err)
	var got []string
	for rows.Next() {
		var network, contract, fee string
		var decimals int
		require.NoError(t, rows.Scan(&network, &contract, &decimals, &fee))
		got = append(got, network+" "+contract+" "+fee)
		assert.Equal(t, 6, decimals)
	}
	require.NoError(t, rows.Close())
	assert.Equal(t, []string{
		"ethereum 0xdAC17F958D2ee523a2206206994597C13D831ec7 ETH",
		"tron TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t TRX",
	}, got)
	var usdc string
	require.NoError(t, db.QueryRow(`SELECT status FROM currencies WHERE info->>'symbol' = 'USDC'`).Scan(&usdc))
	assert.Equal(t, "pending", usdc, "INACTIVE in the file")

	// A market is of two currencies.
	assert.Equal(t, 1, count(t, db, `SELECT COUNT(*) FROM markets m
		JOIN currencies b ON b.hk = m.base_currency_hk JOIN currencies q ON q.hk = m.quote_currency_hk
		WHERE b.info->>'symbol' = 'BTC' AND q.info->>'symbol' = 'USDT'`))

	// Each change is audited.
	assert.Equal(t, len(reg.Assets), count(t, db, `SELECT COUNT(*) FROM audit_events WHERE action = 'currency.create'`))
	assert.Equal(t, networks, count(t, db, `SELECT COUNT(*) FROM audit_events WHERE action = 'currency.network_set'`))
	assert.Equal(t, len(reg.Markets), count(t, db, `SELECT COUNT(*) FROM audit_events WHERE action = 'market.set' AND target_type = 'market'`))

	t.Run("a second import changes nothing", func(t *testing.T) {
		events := count(t, db, `SELECT COUNT(*) FROM audit_events`)
		report, err := store.Import(ctx, reg, importOrigin, false)
		require.NoError(t, err)
		assert.Empty(t, report.Created)
		assert.Len(t, report.Existing, len(reg.Assets))
		assert.Equal(t, ImportCounts{Unchanged: networks}, report.Networks)
		assert.Equal(t, ImportCounts{Unchanged: len(reg.Markets)}, report.Markets)
		assert.Equal(t, events, count(t, db, `SELECT COUNT(*) FROM audit_events`))
	})

	t.Run("a changed limit is written and audited", func(t *testing.T) {
		changed := registryFile(t)
		for i := range changed.Assets {
			if changed.Assets[i].Symbol == "USDT" {
				changed.Assets[i].Networks[0].WithdrawalFee = "2.5"
				changed.Assets[i].FaName = "تتر دلار"
			}
		}
		report, err := store.Import(ctx, changed, importOrigin, false)
		require.NoError(t, err)
		assert.Equal(t, ImportCounts{Updated: 1, Unchanged: networks - 1}, report.Networks)
		assert.Equal(t, []string{"USDT: the names differ from the file's; the admin API changes them"}, report.Notes)
		var fee string
		require.NoError(t, db.QueryRow(`SELECT withdrawal_fee::text FROM currency_networks WHERE currency_hk = $1 AND network = 'tron'`, hk).
			Scan(&fee))
		assert.Equal(t, "2.5", fee)
		var after string
		require.NoError(t, db.QueryRow(`SELECT changes->'after'->>'withdrawal_fee' FROM audit_events
			WHERE action = 'currency.network_set' ORDER BY hk DESC LIMIT 1`).Scan(&after))
		assert.Equal(t, "2.5", after, "the event holds the changed field")
	})
}

func TestImport_RefusesWhatItCannotApply(t *testing.T) {
	ctx := context.Background()
	db := pgtest.Migrated(t, testDBName)
	store := Store{DB: db}

	t.Run("a file that fails its checks", func(t *testing.T) {
		reg := registryFile(t)
		reg.Assets[0].FaName = ""
		_, err := store.Import(ctx, reg, importOrigin, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "fa_name is required")
	})

	t.Run("a new currency that only a delisting reaches", func(t *testing.T) {
		reg := registryFile(t)
		reg.Assets[0].Status = "DELISTED"
		_, err := store.Import(ctx, reg, importOrigin, false)
		require.ErrorIs(t, err, ErrImport)
		assert.Zero(t, count(t, db, `SELECT COUNT(*) FROM currencies`), "nothing is written")
	})
}
