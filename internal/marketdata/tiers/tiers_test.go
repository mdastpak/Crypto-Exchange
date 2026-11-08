package tiers

import (
	"context"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/jobs"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dec(s string) *shared.Decimal {
	if s == "" {
		return nil
	}
	d := shared.MustNewDecimalFromString(s)
	return &d
}

// pair is a pair of a venue with its last price and quote volume.
func pair(id int64, venue, base, quote, slug, last, volumeQuote string) marketdata.TierPair {
	return marketdata.TierPair{PairID: id, VenueID: venue, BaseSymbol: base, QuoteSymbol: quote, BaseSlug: slug,
		Tier: marketdata.TierOther, Last: dec(last), VolumeQuote: dec(volumeQuote)}
}

var options = Options{HotAssets: []string{"bitcoin", "tether"}, HotListed: true, Quotes: []string{"usdt", "IRT"},
	ActiveTop: 2, HysteresisPercent: 50}

func TestAssign_HotByAssetOrListingWithAKeptQuote(t *testing.T) {
	listed := pair(3, "binance", "SOL", "USDT", "solana", "150", "")
	listed.Listed = true
	got := Assign([]marketdata.TierPair{
		pair(1, "binance", "BTC", "USDT", "bitcoin", "68000", ""),
		pair(2, "nobitex", "USDT", "IRT", "tether", "100000", ""),
		listed,
		pair(4, "binance", "BTC", "EUR", "bitcoin", "63000", "1000000000"),
	}, options)
	assert.Equal(t, marketdata.TierHot, got[1])
	assert.Equal(t, marketdata.TierHot, got[2], "tether in tomans")
	assert.Equal(t, marketdata.TierHot, got[3], "listed on this exchange")
	assert.Equal(t, marketdata.TierOther, got[4], "a quote whose candles are not kept")

	off := options
	off.HotListed = false
	assert.Equal(t, marketdata.TierOther, Assign([]marketdata.TierPair{listed}, off)[3])
}

func TestAssign_ActiveByVolumeInUSD(t *testing.T) {
	pairs := []marketdata.TierPair{
		// USDT is 100,000 tomans at nobitex: 50 billion tomans is 500,000 USD.
		pair(1, "nobitex", "USDT", "IRT", "tether", "100000", ""),
		pair(2, "nobitex", "ETH", "IRT", "ethereum", "300000000", "50000000000"),
		pair(3, "binance", "SOL", "USDT", "solana", "150", "400000"),
		pair(4, "binance", "DOGE", "USDT", "dogecoin", "0.1", "600000"),
		// Wallex has no USDT/IRT: the median of the venues prices IRT,
		// so 30 billion tomans is 300,000 USD.
		pair(5, "wallex", "ADA", "IRT", "cardano", "50000", "30000000000"),
		// No volume: never active.
		pair(6, "binance", "XRP", "USDT", "ripple", "0.5", ""),
	}
	pairs[5].VolumeBase = nil
	got := Assign(pairs, options)
	assert.Equal(t, marketdata.TierActive, got[4], "600,000 USD")
	assert.Equal(t, marketdata.TierActive, got[2], "500,000 USD")
	assert.Equal(t, marketdata.TierOther, got[3], "third, past the top 2")
	assert.Equal(t, marketdata.TierOther, got[5])
	assert.Equal(t, marketdata.TierOther, got[6])

	// Volume in the base, at the last price, when the venue gives no quote
	// volume.
	base := pair(7, "binance", "LINK", "USDT", "chainlink", "20", "")
	base.VolumeBase = dec("100000")
	got = Assign(append(pairs, base), options)
	assert.Equal(t, marketdata.TierActive, got[7], "2,000,000 USD")
	assert.Equal(t, marketdata.TierOther, got[2], "pushed out")
}

func TestAssign_Hysteresis(t *testing.T) {
	pairs := []marketdata.TierPair{
		pair(1, "binance", "A", "USDT", "a", "1", "400"),
		pair(2, "binance", "B", "USDT", "b", "1", "300"),
		pair(3, "binance", "C", "USDT", "c", "1", "200"),
		pair(4, "binance", "D", "USDT", "d", "1", "100"),
	}
	// C was active and is third: within 2 plus 50 %, so it stays; D was
	// active and is fourth, past it.
	pairs[2].Tier, pairs[3].Tier = marketdata.TierActive, marketdata.TierActive
	got := Assign(pairs, options)
	assert.Equal(t, marketdata.TierActive, got[1])
	assert.Equal(t, marketdata.TierActive, got[2])
	assert.Equal(t, marketdata.TierActive, got[3], "kept by the hysteresis")
	assert.Equal(t, marketdata.TierOther, got[4])

	// A pair that was not active needs the top 2 to enter.
	pairs[2].Tier = marketdata.TierOther
	assert.Equal(t, marketdata.TierOther, Assign(pairs, options)[3])
}

func TestJob_WritesTheChangedTiers(t *testing.T) {
	store := marketdata.NewStore(pgtest.Migrated(t, "testdb_marketdata_tiers"))
	ctx := context.Background()
	btc, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", marketdata.AssetDetails{Symbol: "BTC", Name: "Bitcoin", Kind: marketdata.KindCoin})
	require.NoError(t, err)
	sol, _, err := store.CreateAsset(ctx, "solana", "coingecko", "solana", marketdata.AssetDetails{Symbol: "SOL", Name: "Solana", Kind: marketdata.KindCoin})
	require.NoError(t, err)
	require.NoError(t, store.UpsertVenue(ctx, marketdata.Venue{ID: "binance", Name: "Binance", Kind: marketdata.VenueCEX, Region: marketdata.RegionGlobal, Enabled: true}))
	ids, err := store.UpsertVenuePairs(ctx, "binance", []marketdata.VenuePair{
		{VenueSymbol: "BTCUSDT", BaseSymbol: "BTC", QuoteSymbol: "USDT", BaseAssetID: &btc},
		{VenueSymbol: "SOLUSDT", BaseSymbol: "SOL", QuoteSymbol: "USDT", BaseAssetID: &sol},
	})
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = store.UpsertTickers(ctx, []marketdata.Ticker{{PairID: ids["SOLUSDT"], LastPrice: *dec("150"), VolumeQuote: dec("1000000"), ObservedAt: now, FetchedAt: now}})
	require.NoError(t, err)

	job, err := New(store, options)
	require.NoError(t, err)
	assert.Equal(t, "tiers", job.Name())
	assert.Empty(t, job.Sources(), "any worker may run it")

	dry := jobs.NewRun(marketdata.ModeDryRun, nil)
	require.NoError(t, job.Run(ctx, dry))
	assert.Equal(t, 2, dry.Written(), "what it would change")
	tier, err := store.PairTier(ctx, ids["BTCUSDT"])
	require.NoError(t, err)
	assert.Equal(t, marketdata.TierOther, tier, "a dry run writes nothing")

	live := jobs.NewRun(marketdata.ModeLive, nil)
	require.NoError(t, job.Run(ctx, live))
	assert.Equal(t, 2, live.Written())
	for symbol, want := range map[string]string{"BTCUSDT": marketdata.TierHot, "SOLUSDT": marketdata.TierActive} {
		tier, err := store.PairTier(ctx, ids[symbol])
		require.NoError(t, err)
		assert.Equal(t, want, tier, symbol)
	}
	again := jobs.NewRun(marketdata.ModeLive, nil)
	require.NoError(t, job.Run(ctx, again))
	assert.Equal(t, 0, again.Written(), "nothing changed")

	lost := jobs.NewRun(marketdata.ModeLive, func(context.Context) error { return jobs.ErrLeaseLost })
	_, err = store.SetTiers(ctx, map[int64]string{ids["BTCUSDT"]: marketdata.TierOther})
	require.NoError(t, err)
	assert.ErrorIs(t, job.Run(ctx, lost), jobs.ErrLeaseLost, "a worker that lost the job writes nothing")
}
