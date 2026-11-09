package marketdata

import (
	"context"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTiers_AutomaticAndOverrides(t *testing.T) {
	store, db := newStore(t)
	ctx := context.Background()
	btc, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", bitcoin())
	require.NoError(t, err)
	doge, _, err := store.CreateAsset(ctx, "dogecoin", "coingecko", "dogecoin", AssetDetails{Symbol: "DOGE", Name: "Dogecoin", Kind: KindCoin})
	require.NoError(t, err)
	require.NoError(t, store.UpsertVenue(ctx, Venue{ID: "binance", Name: "Binance", Kind: VenueCEX, Region: RegionGlobal, Enabled: true}))
	ids, err := store.UpsertVenuePairs(ctx, "binance", []VenuePair{
		{VenueSymbol: "BTCUSDT", BaseSymbol: "BTC", QuoteSymbol: "USDT", BaseAssetID: &btc},
		{VenueSymbol: "DOGEUSDT", BaseSymbol: "DOGE", QuoteSymbol: "USDT", BaseAssetID: &doge},
		{VenueSymbol: "XYZUSDT", BaseSymbol: "XYZ", QuoteSymbol: "USDT"},
	})
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = store.UpsertTickers(ctx, []Ticker{{PairID: ids["BTCUSDT"], LastPrice: *dec("68000"), VolumeQuote: dec("1000000"), ObservedAt: now, FetchedAt: now}})
	require.NoError(t, err)

	pairs, err := store.TierPairs(ctx)
	require.NoError(t, err)
	require.Len(t, pairs, 2, "not an unmapped pair")
	assert.Equal(t, "bitcoin", pairs[0].BaseSlug)
	assert.Equal(t, TierOther, pairs[0].Tier, "every pair starts as other")
	assert.Equal(t, "1000000", pairs[0].VolumeQuote.String())
	assert.Nil(t, pairs[1].Last, "no ticker yet")

	changed, err := store.SetTiers(ctx, map[int64]string{ids["BTCUSDT"]: TierHot, ids["DOGEUSDT"]: TierOther})
	require.NoError(t, err)
	assert.Equal(t, 1, changed, "a pair already in its tier is not written")
	_, err = store.SetTiers(ctx, map[int64]string{ids["BTCUSDT"]: "warm"})
	assert.Error(t, err)

	tierOf := func(pair int64) string {
		tier, err := store.PairTier(ctx, pair)
		require.NoError(t, err)
		return tier
	}
	assert.Equal(t, TierHot, tierOf(ids["BTCUSDT"]))

	// An administrator's tier on the asset, then on the pair, wins.
	active := TierActive
	require.NoError(t, store.SetAssetTierOverride(ctx, doge, &active, audit.Event{}))
	assert.Equal(t, TierActive, tierOf(ids["DOGEUSDT"]))
	other := TierOther
	require.NoError(t, store.SetPairTierOverride(ctx, ids["DOGEUSDT"], &other, audit.Event{}))
	assert.Equal(t, TierOther, tierOf(ids["DOGEUSDT"]))
	require.NoError(t, store.SetPairTierOverride(ctx, ids["DOGEUSDT"], nil, audit.Event{}))
	assert.Equal(t, TierActive, tierOf(ids["DOGEUSDT"]))
	pairs, err = store.TierPairs(ctx)
	require.NoError(t, err)
	assert.Equal(t, TierActive, *pairs[1].Override)
	assert.ErrorIs(t, store.SetPairTierOverride(ctx, 999999, &other, audit.Event{}), ErrNotFound)
	assert.Error(t, store.SetAssetTierOverride(ctx, doge, new(string), audit.Event{}))

	// The candle jobs read the tier in which a pair is collected.
	fine, err := store.CandlePairs(ctx, "binance", Resolution1m, []string{"USDT"}, []string{TierHot}, 10)
	require.NoError(t, err)
	require.Len(t, fine, 1)
	assert.Equal(t, "BTCUSDT", fine[0].VenueSymbol)
	medium, err := store.CandlePairs(ctx, "binance", Resolution5m, []string{"USDT"}, []string{TierHot, TierActive}, 10)
	require.NoError(t, err)
	assert.Len(t, medium, 2, "dogecoin by its asset's tier")
	all, err := store.CandlePairs(ctx, "binance", Resolution1h, []string{"USDT"}, nil, 10)
	require.NoError(t, err)
	assert.Len(t, all, 2)

	_, err = db.Exec(`UPDATE market_venue_pairs SET tier = 'cold'`)
	assert.Error(t, err, "the tiers are checked")
}
