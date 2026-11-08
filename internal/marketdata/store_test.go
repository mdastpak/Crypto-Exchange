package marketdata

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	conn := pgtest.Migrated(t, "testdb_marketdata")
	return NewStore(conn), conn
}

func dec(s string) *shared.Decimal {
	d := shared.MustNewDecimalFromString(s)
	return &d
}

func bitcoin() AssetDetails {
	rank := 1
	return AssetDetails{
		Symbol: "btc", Name: "Bitcoin", Kind: KindCoin,
		Description:       "Peer-to-peer electronic cash.",
		Links:             map[string]string{"website": "https://bitcoin.org"},
		LogoURL:           "https://example.com/btc.png",
		MarketCapRank:     &rank,
		CirculatingSupply: dec("19750000.123456789012345678"),
		MaxSupply:         dec("21000000"),
		Attribution:       "CoinGecko",
	}
}

func TestCreateAsset_StoresTheAssetAndItsIdentity(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	id, created, err := store.CreateAsset(ctx, "Bitcoin", "coingecko", "bitcoin", bitcoin())
	require.NoError(t, err)
	assert.True(t, created)

	a, err := store.Asset(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "bitcoin", a.Slug)
	assert.Equal(t, "BTC", a.Symbol, "symbols are stored in upper case")
	assert.Equal(t, StatusUnverified, a.Status, "a collected asset is unverified")
	assert.Nil(t, a.CurrencyHK)
	assert.True(t, a.Active)
	assert.Equal(t, "https://bitcoin.org", a.Links["website"])
	require.NotNil(t, a.CirculatingSupply)
	assert.Equal(t, "19750000.123456789012345678", a.CirculatingSupply.String(), "NUMERIC keeps every digit")
	assert.Nil(t, a.TotalSupply)

	owner, err := store.AssetBySource(ctx, "coingecko", "bitcoin")
	require.NoError(t, err)
	assert.Equal(t, id, owner)
}

func TestCreateAsset_AnExistingIdentityReturnsTheAsset(t *testing.T) {
	store, conn := newStore(t)
	ctx := context.Background()

	first, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", bitcoin())
	require.NoError(t, err)
	again, created, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", bitcoin())
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, first, again)

	var n int
	require.NoError(t, conn.QueryRow(`SELECT count(*) FROM market_assets`).Scan(&n))
	assert.Equal(t, 1, n, "the second call rolled its asset back")
}

func TestCreateAsset_NodesRacingCreateOneAsset(t *testing.T) {
	store, conn := newStore(t)
	ctx := context.Background()

	const nodes = 8
	ids := make([]int64, nodes)
	errs := make([]error, nodes)
	var wg sync.WaitGroup
	for i := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], _, errs[i] = store.CreateAsset(ctx, "tether", "coingecko", "tether",
				AssetDetails{Symbol: "USDT", Name: "Tether", Kind: KindToken})
		}()
	}
	wg.Wait()
	for i := range nodes {
		require.NoError(t, errs[i])
		assert.Equal(t, ids[0], ids[i])
	}
	var n int
	require.NoError(t, conn.QueryRow(`SELECT count(*) FROM market_assets`).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestCreateAsset_ASymbolDoesNotMergeAssets(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	// Two different assets share the symbol and the name's slug.
	a, _, err := store.CreateAsset(ctx, "Unicorn", "coingecko", "unicorn-a", AssetDetails{Symbol: "UNI", Name: "Unicorn"})
	require.NoError(t, err)
	b, _, err := store.CreateAsset(ctx, "Unicorn", "coingecko", "unicorn-b", AssetDetails{Symbol: "UNI", Name: "Unicorn"})
	require.NoError(t, err)
	assert.NotEqual(t, a, b)

	second, err := store.Asset(ctx, b)
	require.NoError(t, err)
	assert.Equal(t, "unicorn-2", second.Slug, "the second asset takes a suffixed slug")
}

func TestCreateAsset_RefusesInvalidDetails(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	bad := []AssetDetails{
		{Symbol: "", Name: "X"},
		{Symbol: "X", Name: ""},
		{Symbol: "X", Name: "X", Kind: "nft"},
		{Symbol: "X", Name: "X", LogoURL: "http://insecure.example/x.png"},
		{Symbol: "X", Name: "X", Links: map[string]string{"website": "javascript:alert(1)"}},
		{Symbol: "X", Name: "X", MaxSupply: dec("-1")},
	}
	for _, details := range bad {
		_, _, err := store.CreateAsset(ctx, "x", "coingecko", "x", details)
		assert.Error(t, err, "%+v", details)
	}
	_, _, err := store.CreateAsset(ctx, "x", "Coin Gecko", "x", AssetDetails{Symbol: "X", Name: "X"})
	assert.Error(t, err, "an invalid source name")
	_, _, err = store.CreateAsset(ctx, "!!!", "coingecko", "x", AssetDetails{Symbol: "X", Name: "X"})
	assert.Error(t, err, "an empty slug")
}

func TestUpdateAsset_WritesOnlyChanges(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	id, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", bitcoin())
	require.NoError(t, err)

	changed, err := store.UpdateAsset(ctx, id, bitcoin())
	require.NoError(t, err)
	assert.False(t, changed, "the same details change nothing")

	details := bitcoin()
	details.CirculatingSupply = dec("19750001")
	changed, err = store.UpdateAsset(ctx, id, details)
	require.NoError(t, err)
	assert.True(t, changed)
	a, err := store.Asset(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "19750001", a.CirculatingSupply.String())

	// A supply that equals the stored one in value but not in text is the
	// same supply.
	details.CirculatingSupply = dec("19750001.000")
	changed, err = store.UpdateAsset(ctx, id, details)
	require.NoError(t, err)
	assert.False(t, changed)

	_, err = store.UpdateAsset(ctx, id+1000, details)
	assert.True(t, errors.Is(err, ErrNotFound))
}

func TestUpdateAsset_KeepsTheAdministratorsStatus(t *testing.T) {
	store, conn := newStore(t)
	ctx := context.Background()
	id, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", bitcoin())
	require.NoError(t, err)
	_, err = conn.Exec(`UPDATE market_assets SET status = 'hidden', active = FALSE WHERE id = $1`, id)
	require.NoError(t, err)

	changed, err := store.UpdateAsset(ctx, id, bitcoin())
	require.NoError(t, err)
	assert.True(t, changed, "the asset is listed again, so it is active")
	a, err := store.Asset(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, StatusHidden, a.Status)
	assert.True(t, a.Active)
}

func TestAssets_LinkedStatusNeedsACurrency(t *testing.T) {
	_, conn := newStore(t)
	_, err := conn.Exec(`INSERT INTO market_assets (slug, symbol, name, status) VALUES ('x', 'X', 'X', 'linked')`)
	assert.Error(t, err, "a linked asset names its currency")
	_, err = conn.Exec(`INSERT INTO market_assets (slug, symbol, name, currency_hk) VALUES ('y', 'Y', 'Y', gen_random_uuid())`)
	assert.Error(t, err, "only a linked asset names a currency")
}

func TestAddContract_ReportsTheOwner(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	usdt, _, err := store.CreateAsset(ctx, "tether", "coingecko", "tether", AssetDetails{Symbol: "USDT", Name: "Tether", Kind: KindToken})
	require.NoError(t, err)
	other, _, err := store.CreateAsset(ctx, "fake-tether", "coingecko", "fake-tether", AssetDetails{Symbol: "USDT", Name: "Fake Tether", Kind: KindToken})
	require.NoError(t, err)

	const checksummed = "0xdAC17F958D2ee523a2206206994597C13D831ec7"
	owner, err := store.AddContract(ctx, usdt, "Ethereum", checksummed, "coingecko")
	require.NoError(t, err)
	assert.Equal(t, usdt, owner)

	// The same address in another case is the same contract.
	owner, err = store.AddContract(ctx, other, "ethereum", "0xdac17f958d2ee523a2206206994597c13d831ec7", "coinmarketcap")
	require.NoError(t, err)
	assert.Equal(t, usdt, owner, "the contract stays with its first asset; the caller reports the conflict")

	// A TRON address keeps its case, which matters there.
	const tron = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	_, err = store.AddContract(ctx, usdt, "tron", tron, "coingecko")
	require.NoError(t, err)
	found, err := store.AssetByContract(ctx, "tron", tron)
	require.NoError(t, err)
	assert.Equal(t, usdt, found)
	_, err = store.AssetByContract(ctx, "tron", "tr7nhqjekqxgtci8q8zy4pl8otszgjlj6t")
	assert.True(t, errors.Is(err, ErrNotFound))
}

func TestLinkSource_ReportsTheOwner(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	btc, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", bitcoin())
	require.NoError(t, err)
	eth, _, err := store.CreateAsset(ctx, "ethereum", "coingecko", "ethereum", AssetDetails{Symbol: "ETH", Name: "Ethereum"})
	require.NoError(t, err)

	owner, err := store.LinkSource(ctx, btc, "coinmarketcap", "1")
	require.NoError(t, err)
	assert.Equal(t, btc, owner)
	owner, err = store.LinkSource(ctx, eth, "coinmarketcap", "1")
	require.NoError(t, err)
	assert.Equal(t, btc, owner)
}

func TestMarkAssetsSeen_NeverGoesBack(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	id, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", bitcoin())
	require.NoError(t, err)

	later := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	require.NoError(t, store.MarkAssetsSeen(ctx, []int64{id}, later))
	require.NoError(t, store.MarkAssetsSeen(ctx, []int64{id}, later.Add(-2*time.Hour)))
	a, err := store.Asset(ctx, id)
	require.NoError(t, err)
	assert.True(t, a.LastSeenAt.Equal(later))
	require.NoError(t, store.MarkAssetsSeen(ctx, nil, later))
}

func TestUpsertVenue_KeepsTheEnabledSetting(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	v := Venue{ID: "nobitex", Name: "Nobitex", Kind: VenueCEX, Region: RegionIran, Country: "ir", Website: "https://nobitex.ir", Enabled: true}
	require.NoError(t, store.UpsertVenue(ctx, v))

	v.Name, v.Enabled = "Nobitex Exchange", false
	require.NoError(t, store.UpsertVenue(ctx, v))
	venues, err := store.Venues(ctx)
	require.NoError(t, err)
	require.Len(t, venues, 1)
	assert.Equal(t, "Nobitex Exchange", venues[0].Name)
	assert.Equal(t, "IR", venues[0].Country)
	assert.Equal(t, RegionIran, venues[0].Region)
	assert.True(t, venues[0].Enabled, "an administrator's setting is kept")

	assert.Error(t, store.UpsertVenue(ctx, Venue{ID: "x", Name: "X", Kind: "otc", Region: RegionGlobal}))
	assert.Error(t, store.UpsertVenue(ctx, Venue{ID: "x", Name: "X", Kind: VenueCEX, Region: "eu"}))
	assert.Error(t, store.UpsertVenue(ctx, Venue{ID: "X Y", Name: "X", Kind: VenueCEX, Region: RegionGlobal}))
}

func newPair(t *testing.T, store *Store) int64 {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, store.UpsertVenue(ctx, Venue{ID: "binance", Name: "Binance", Kind: VenueCEX, Region: RegionGlobal}))
	id, err := store.UpsertVenuePair(ctx, VenuePair{VenueID: "binance", VenueSymbol: "BTCUSDT", BaseSymbol: "btc", QuoteSymbol: "usdt"})
	require.NoError(t, err)
	return id
}

func TestUpsertVenuePair_KeepsTheMappingAndTheID(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	btc, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", bitcoin())
	require.NoError(t, err)
	id := newPair(t, store)

	again, err := store.UpsertVenuePair(ctx, VenuePair{VenueID: "binance", VenueSymbol: "BTCUSDT", BaseSymbol: "BTC", QuoteSymbol: "USDT", BaseAssetID: &btc})
	require.NoError(t, err)
	assert.Equal(t, id, again)
	_, err = store.UpsertVenuePair(ctx, VenuePair{VenueID: "binance", VenueSymbol: "BTCUSDT", BaseSymbol: "BTC", QuoteSymbol: "USDT"})
	require.NoError(t, err)

	var base sql.NullInt64
	require.NoError(t, store.db.QueryRow(`SELECT base_asset_id FROM market_venue_pairs WHERE id = $1`, id).Scan(&base))
	assert.Equal(t, btc, base.Int64, "a pair without a mapping keeps the one it has")

	_, err = store.UpsertVenuePair(ctx, VenuePair{VenueID: "binance", VenueSymbol: "", BaseSymbol: "BTC", QuoteSymbol: "USDT"})
	assert.Error(t, err)
}

func TestUpsertTicker_NeverPutsAnOlderTickerBack(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	pair := newPair(t, store)
	now := time.Now().UTC().Truncate(time.Microsecond)

	newer := Ticker{PairID: pair, LastPrice: shared.MustNewDecimalFromString("68000.12345678"),
		Bid: dec("68000"), Ask: dec("68000.5"), VolumeBase: dec("0.000000012345678901"),
		ObservedAt: now, FetchedAt: now}
	written, err := store.UpsertTicker(ctx, newer)
	require.NoError(t, err)
	assert.True(t, written)

	older := newer
	older.LastPrice = shared.MustNewDecimalFromString("1")
	older.ObservedAt = now.Add(-time.Minute)
	written, err = store.UpsertTicker(ctx, older)
	require.NoError(t, err)
	assert.False(t, written, "a late, older ticker is ignored")
	written, err = store.UpsertTicker(ctx, newer)
	require.NoError(t, err)
	assert.False(t, written, "the same observation is not written twice")

	got, err := store.TickerOf(ctx, pair)
	require.NoError(t, err)
	assert.Equal(t, "68000.12345678", got.LastPrice.String())
	assert.Equal(t, "0.000000012345678901", got.VolumeBase.String())
	assert.Nil(t, got.High)

	_, err = store.UpsertTicker(ctx, Ticker{PairID: pair, LastPrice: shared.MustNewDecimalFromString("-1"), ObservedAt: now, FetchedAt: now})
	assert.Error(t, err)
	_, err = store.TickerOf(ctx, pair+1000)
	assert.True(t, errors.Is(err, ErrNotFound))
}

func TestRuns_RecordStartAndEnd(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	ok, err := store.StartRun(ctx, "catalog", "market-worker/a", ModeDryRun)
	require.NoError(t, err)
	require.NoError(t, store.FinishRun(ctx, ok, 10, 0, nil))
	failed, err := store.StartRun(ctx, "catalog", "market-worker/b", ModeLive)
	require.NoError(t, err)
	require.NoError(t, store.FinishRun(ctx, failed, 3, 1, errors.New("source answered 429")))
	// A finished run keeps its record.
	require.NoError(t, store.FinishRun(ctx, failed, 0, 0, nil))

	runs, err := store.Runs(ctx, "catalog", 10)
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, failed, runs[0].ID)
	assert.Equal(t, "failed", runs[0].Status)
	assert.Equal(t, "source answered 429", runs[0].Error)
	assert.Equal(t, 1, runs[0].ItemsWritten)
	assert.NotNil(t, runs[0].FinishedAt)
	assert.Equal(t, "succeeded", runs[1].Status)
	assert.Equal(t, ModeDryRun, runs[1].Mode)

	_, err = store.StartRun(ctx, "catalog", "n", "shadow")
	assert.Error(t, err)
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Bitcoin":             "bitcoin",
		"Wrapped Bitcoin":     "wrapped-bitcoin",
		"  --USD Coin (USDC)": "usd-coin-usdc",
		"!!!":                 "",
		"Ethereum 2.0":        "ethereum-2-0",
	} {
		assert.Equal(t, want, Slugify(in), in)
	}
}

func TestNormalizeAddress(t *testing.T) {
	assert.Equal(t, "0xdac17f958d2ee523a2206206994597c13d831ec7", NormalizeAddress(" 0xdAC17F958D2ee523a2206206994597C13D831ec7 "))
	assert.Equal(t, "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", NormalizeAddress("TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"))
}
