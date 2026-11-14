package currencies

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/assetregistry"
	"github.com/mdastpak/crypto-exchange/internal/domain/market"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// imported loads the registry file of the repository into a migrated test
// database.
func imported(t *testing.T) (*sql.DB, Store) {
	t.Helper()
	db := pgtest.Migrated(t, testDBName)
	store := Store{DB: db}
	_, err := store.Import(context.Background(), registryFile(t), importOrigin, false)
	require.NoError(t, err)
	return db, store
}

func hkOf(t *testing.T, db *sql.DB, symbol string) string {
	t.Helper()
	var hk string
	require.NoError(t, db.QueryRow(`SELECT hk FROM currencies WHERE info->>'symbol' = $1`, symbol).Scan(&hk))
	return hk
}

// usdtOnTron is the tron network of USDT in the registry file.
func usdtOnTron() assetregistry.Network {
	return assetregistry.Network{
		Network: "tron", ContractAddress: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", FeeAsset: "TRX",
		MinWithdrawal: "20.0", MaxWithdrawal: "1000000.0", WithdrawalFee: "1.0",
		DepositEnabled: true, WithdrawalEnabled: true, CustodyMode: "ON_CHAIN", ExplorerURL: "https://tronscan.org/#/transaction/",
	}
}

func problemFieldsOf(t *testing.T, err error) []string {
	t.Helper()
	var invalid *Invalid
	require.ErrorAs(t, err, &invalid)
	var fields []string
	for _, p := range invalid.Problems {
		fields = append(fields, p.Field)
	}
	return fields
}

func TestSetNetwork_WritesANetworkWithTheChecksOfTheFile(t *testing.T) {
	ctx := context.Background()
	db, store := imported(t)
	usdt := hkOf(t, db, "USDT")
	events := func() int {
		return count(t, db, `SELECT COUNT(*) FROM audit_events WHERE action = 'currency.network_set'`)
	}
	before := events()

	t.Run("as the file has it, nothing changes", func(t *testing.T) {
		w, err := store.SetNetwork(ctx, usdt, usdtOnTron(), 6, importOrigin)
		require.NoError(t, err)
		assert.False(t, w.Changed)
		assert.True(t, w.Public, "USDT is approved")
		assert.Equal(t, before, events())
	})

	t.Run("a changed fee is written and audited", func(t *testing.T) {
		n := usdtOnTron()
		n.WithdrawalFee, n.WithdrawalEnabled = "2", false
		w, err := store.SetNetwork(ctx, usdt, n, 6, importOrigin)
		require.NoError(t, err)
		assert.True(t, w.Changed)
		assert.False(t, w.Created)
		assert.Equal(t, "2", w.Network.WithdrawalFee.String())
		assert.False(t, w.Network.WithdrawalEnabled)
		assert.Equal(t, "tron", w.Network.Network)
		require.NotNil(t, w.Network.FeeCurrencyHK)
		assert.Equal(t, hkOf(t, db, "TRX"), w.Network.FeeCurrencyHK.String())
		assert.Equal(t, before+1, events())
		var after string
		require.NoError(t, db.QueryRow(`SELECT changes->'after'->>'withdrawal_fee' FROM audit_events
			WHERE action = 'currency.network_set' ORDER BY occurred_at DESC, hk DESC LIMIT 1`).Scan(&after))
		assert.Equal(t, "2", after)
	})

	t.Run("the contract and the decimals of a network are fixed", func(t *testing.T) {
		n := usdtOnTron()
		n.ContractAddress = "TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8"
		_, err := store.SetNetwork(ctx, usdt, n, 6, importOrigin)
		require.ErrorIs(t, err, ErrContractFixed)
		_, err = store.SetNetwork(ctx, usdt, usdtOnTron(), 18, importOrigin)
		require.ErrorIs(t, err, ErrContractFixed)
	})

	t.Run("a new network of a currency that is not public", func(t *testing.T) {
		n := usdtOnTron()
		n.ContractAddress = "TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8"
		w, err := store.SetNetwork(ctx, hkOf(t, db, "USDC"), n, 6, importOrigin)
		require.NoError(t, err)
		assert.True(t, w.Created)
		assert.False(t, w.Public, "USDC is pending")
	})

	t.Run("another currency's contract", func(t *testing.T) {
		_, err := store.SetNetwork(ctx, hkOf(t, db, "WBTC"), usdtOnTron(), 6, importOrigin)
		require.ErrorIs(t, err, ErrContractTaken)
	})

	t.Run("a network that fails the checks of the file", func(t *testing.T) {
		n := usdtOnTron()
		n.Network = "solana"
		_, err := store.SetNetwork(ctx, usdt, n, 6, importOrigin)
		assert.Equal(t, []string{"network"}, problemFieldsOf(t, err))

		n = usdtOnTron()
		n.ContractAddress, n.FeeAsset, n.MinWithdrawal = "", "USDT", "-1"
		_, err = store.SetNetwork(ctx, hkOf(t, db, "ETH"), n, 40, importOrigin)
		assert.ElementsMatch(t, []string{"contract_address", "fee_asset", "min_withdrawal", "decimals"}, problemFieldsOf(t, err))
		assert.Equal(t, before+2, events(), "nothing refused is written")
	})

	t.Run("a currency that does not exist", func(t *testing.T) {
		_, err := store.SetNetwork(ctx, uuid.NewString(), usdtOnTron(), 6, importOrigin)
		require.ErrorIs(t, err, ErrNotFound)
		_, err = store.Networks(ctx, uuid.NewString())
		require.ErrorIs(t, err, ErrNotFound)
	})

	list, err := store.Networks(ctx, usdt)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "ethereum", list[0].Network)
	assert.Equal(t, "0xdAC17F958D2ee523a2206206994597C13D831ec7", list[0].ContractAddress)
	assert.Equal(t, 6, list[0].Decimals)
	assert.Equal(t, "tron", list[1].Network)
}

func TestImport_MayStillChangeAContract(t *testing.T) {
	ctx := context.Background()
	db, store := imported(t)
	reg := registryFile(t)
	for i := range reg.Assets {
		if reg.Assets[i].Symbol == "USDT" {
			reg.Assets[i].Networks[0].ContractAddress = "TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8"
		}
	}
	report, err := store.Import(ctx, reg, importOrigin, false)
	require.NoError(t, err, "the reviewed file may, unlike the admin API")
	assert.Equal(t, 1, report.Networks.Updated)
	var contract string
	require.NoError(t, db.QueryRow(`SELECT contract_address FROM currency_networks WHERE currency_hk = $1 AND network = 'tron'`,
		hkOf(t, db, "USDT")).Scan(&contract))
	assert.Equal(t, "TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8", contract)
}

func btcUSDT() assetregistry.Market {
	return assetregistry.Market{
		Base: "BTC", Quote: "USDT", Status: market.MarketStatusActive, MinOrderSize: "0.0001", MaxOrderSize: "10.0",
		MinNotional: "10.0", TickSize: "0.01", LotSize: "0.0001", MakerFeeBPS: 10, TakerFeeBPS: 20,
	}
}

func TestSetMarket_WritesAMarketWithTheChecksOfTheFile(t *testing.T) {
	ctx := context.Background()
	db, store := imported(t)
	events := func() int { return count(t, db, `SELECT COUNT(*) FROM audit_events WHERE action = 'market.set'`) }
	before := events()

	w, err := store.SetMarket(ctx, btcUSDT(), importOrigin)
	require.NoError(t, err)
	assert.False(t, w.Changed, "as the file has it")
	assert.Equal(t, before, events())

	m := btcUSDT()
	m.Status, m.TakerFeeBPS = market.MarketStatusSuspended, 25
	w, err = store.SetMarket(ctx, m, importOrigin)
	require.NoError(t, err)
	assert.True(t, w.Changed)
	assert.Equal(t, market.MarketStatusSuspended, w.Market.Status)
	assert.Equal(t, 25, w.Market.TakerFeeBPS)
	assert.Equal(t, "BTC/USDT", w.Market.Symbol())
	assert.Equal(t, before+1, events())

	m = btcUSDT()
	m.Base, m.Status = "USDC", market.MarketStatusInactive
	w, err = store.SetMarket(ctx, m, importOrigin)
	require.NoError(t, err)
	assert.True(t, w.Created)
	assert.Equal(t, hkOf(t, db, "USDC"), w.Market.BaseCurrencyHK.String())

	for name, tc := range map[string]struct {
		edit   func(*assetregistry.Market)
		fields []string
	}{
		"one currency":            {func(m *assetregistry.Market) { m.Quote = "BTC" }, []string{"quote"}},
		"an unknown currency":     {func(m *assetregistry.Market) { m.Base = "DOGE" }, []string{"base"}},
		"a zero tick and a fee":   {func(m *assetregistry.Market) { m.TickSize, m.MakerFeeBPS = "0", 2000 }, []string{"tick_size", "maker_fee_bps"}},
		"an unknown status":       {func(m *assetregistry.Market) { m.Status = "LIVE" }, []string{"status"}},
		"a minimum over the most": {func(m *assetregistry.Market) { m.MinOrderSize = "11" }, []string{"min_order_size"}},
	} {
		t.Run(name, func(t *testing.T) {
			m := btcUSDT()
			tc.edit(&m)
			_, err := store.SetMarket(ctx, m, importOrigin)
			assert.ElementsMatch(t, tc.fields, problemFieldsOf(t, err))
		})
	}

	list, err := store.Markets(ctx)
	require.NoError(t, err)
	assert.Len(t, list, len(registryFile(t).Markets)+1)
	got, err := store.Market(ctx, "BTC", "USDT")
	require.NoError(t, err)
	assert.Equal(t, 25, got.TakerFeeBPS)
	_, err = store.Market(ctx, "BTC", "DOGE")
	require.ErrorIs(t, err, ErrMarketNotFound)
}

func TestMarketList_ServesThePublicMarkets(t *testing.T) {
	ctx := context.Background()
	_, store := imported(t)
	list := NewMarketList(store.DB, time.Minute)
	now := time.Now()
	list.now = func() time.Time { return now }

	markets, err := list.List(ctx)
	require.NoError(t, err)
	symbols := map[string]PublicMarket{}
	for _, m := range markets {
		symbols[m.Symbol] = m
	}
	require.Contains(t, symbols, "BTC/USDT")
	assert.NotContains(t, symbols, "BTC/TRX", "INACTIVE in the file")
	btc := symbols["BTC/USDT"]
	assert.Equal(t, "ACTIVE", btc.Status)
	assert.Equal(t, "10", btc.MaxOrderSize, "amounts without trailing zeros")
	assert.Equal(t, "0.01", btc.TickSize)
	assert.Equal(t, 20, btc.TakerFeeBPS)

	// A market of a currency that is not approved is not public.
	m := btcUSDT()
	m.Base = "USDC"
	_, err = store.SetMarket(ctx, m, importOrigin)
	require.NoError(t, err)
	m = btcUSDT()
	m.Status = market.MarketStatusSuspended
	_, err = store.SetMarket(ctx, m, importOrigin)
	require.NoError(t, err)

	bySymbol := func() map[string]PublicMarket {
		markets, err = list.List(ctx)
		require.NoError(t, err)
		symbols := map[string]PublicMarket{}
		for _, m := range markets {
			symbols[m.Symbol] = m
		}
		return symbols
	}
	assert.Equal(t, "ACTIVE", bySymbol()["BTC/USDT"].Status, "served from memory until the time is out")
	now = now.Add(time.Minute)
	symbols = bySymbol()
	assert.Equal(t, "SUSPENDED", symbols["BTC/USDT"].Status, "paused markets are listed")
	assert.NotContains(t, symbols, "USDC/USDT")

	// While the database cannot be read, the markets read last are served.
	list.load = func(context.Context) ([]PublicMarket, error) { return nil, errors.New("database down") }
	now = now.Add(time.Minute)
	stale, err := list.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, markets, stale)

	empty := NewMarketList(store.DB, 0)
	empty.load = list.load
	_, err = empty.List(ctx)
	require.Error(t, err, "nothing to serve")
}

func TestCatalog_ShowsTheNetworksOfThePublicCurrencies(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	_, err := Store{DB: f.db}.Import(ctx, registryFile(t), importOrigin, false)
	require.NoError(t, err)

	list, err := f.catalog.List(ctx)
	require.NoError(t, err)
	bySymbol := map[string][]string{}
	for _, cur := range list {
		for _, n := range cur.Networks {
			bySymbol[cur.Info.Symbol] = append(bySymbol[cur.Info.Symbol], n.Network)
		}
	}
	assert.Equal(t, []string{"ethereum", "tron"}, bySymbol["USDT"])
	assert.Equal(t, []string{"internal"}, bySymbol["IRR"])
	assert.NotContains(t, bySymbol, "USDC", "pending")

	usdt, found, err := f.catalog.Get(ctx, hkOf(t, f.db, "USDT"))
	require.NoError(t, err)
	require.True(t, found)
	tron := usdt.Networks[1]
	assert.Equal(t, "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", tron.ContractAddress)
	assert.Equal(t, "TRX", tron.FeeCurrency)
	assert.Equal(t, "20", tron.MinWithdrawal, "without trailing zeros")
	assert.Equal(t, "1", tron.WithdrawalFee)
	assert.Equal(t, "0", tron.MinDeposit)
	assert.Equal(t, 6, tron.Decimals)
	assert.True(t, tron.DepositEnabled)
}

// TestRegistry_SealsNetworksAndMarkets checks the seals of #57 in enforce
// mode: every write seals its row, and a network or a market changed in SQL
// is neither served nor written over.
func TestRegistry_SealsNetworksAndMarkets(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	sealtest.Use(t, f.db, rowseal.ModeEnforce)
	store := Store{DB: f.db}
	_, err := store.Import(ctx, registryFile(t), importOrigin, false)
	require.NoError(t, err)
	assert.Zero(t, count(t, f.db, `SELECT COUNT(*) FROM currency_networks WHERE row_seal IS NULL`), "the import seals")
	assert.Zero(t, count(t, f.db, `SELECT COUNT(*) FROM markets WHERE row_seal IS NULL`))
	usdt := hkOf(t, f.db, "USDT")

	t.Run("a write seals again", func(t *testing.T) {
		n := usdtOnTron()
		n.WithdrawalFee = "3"
		_, err := store.SetNetwork(ctx, usdt, n, 6, importOrigin)
		require.NoError(t, err)
		list, err := store.Networks(ctx, usdt)
		require.NoError(t, err)
		assert.Equal(t, "3", list[1].WithdrawalFee.String())
	})

	// A contract swapped in SQL for another token's.
	_, err = f.db.Exec(`UPDATE currency_networks SET contract_address = 'TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8'
		WHERE currency_hk = $1 AND network = 'tron'`, usdt)
	require.NoError(t, err)

	t.Run("a network changed in SQL", func(t *testing.T) {
		_, err := store.Networks(ctx, usdt)
		require.ErrorIs(t, err, rowseal.ErrUnverified, "the admin view")
		_, err = store.SetNetwork(ctx, usdt, usdtOnTron(), 6, importOrigin)
		require.ErrorIs(t, err, rowseal.ErrUnverified, "a write does not seal what nobody checked")
		_, err = store.Import(ctx, registryFile(t), importOrigin, false)
		require.ErrorIs(t, err, rowseal.ErrUnverified, "nor does the import")

		list, err := f.catalog.List(ctx)
		require.NoError(t, err)
		for _, cur := range list {
			if cur.Info.Symbol == "USDT" {
				require.Len(t, cur.Networks, 1, "users never see the swapped contract")
				assert.Equal(t, "ethereum", cur.Networks[0].Network)
			}
		}
		assert.Equal(t, 1, count(t, f.db, `SELECT COUNT(*) FROM integrity_findings
			WHERE table_name = 'currency_networks' AND resolved_at IS NULL`))
	})

	// A market made free in SQL.
	_, err = f.db.Exec(`UPDATE markets SET taker_fee_bps = 0 WHERE base_currency_hk = $1`, hkOf(t, f.db, "TRX"))
	require.NoError(t, err)

	t.Run("a market changed in SQL", func(t *testing.T) {
		_, err := store.Markets(ctx)
		require.ErrorIs(t, err, rowseal.ErrUnverified)
		_, err = store.Market(ctx, "TRX", "USDT")
		require.ErrorIs(t, err, rowseal.ErrUnverified)
		m := btcUSDT()
		m.Base = "TRX"
		_, err = store.SetMarket(ctx, m, importOrigin)
		require.ErrorIs(t, err, rowseal.ErrUnverified)
		_, err = store.Market(ctx, "BTC", "USDT")
		require.NoError(t, err, "the other markets are served")

		markets, err := NewMarketList(f.db, 0).List(ctx)
		require.NoError(t, err)
		for _, m := range markets {
			assert.NotEqual(t, "TRX", m.Base, m.Symbol)
		}
		assert.Contains(t, symbolsOf(markets), "BTC/USDT")
	})
}

func symbolsOf(markets []PublicMarket) []string {
	var symbols []string
	for _, m := range markets {
		symbols = append(symbols, m.Symbol)
	}
	return symbols
}
