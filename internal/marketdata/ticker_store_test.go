package marketdata

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

func TestUpsertVenuePairs_BatchesAndKeepsTheMapping(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	require.NoError(t, store.UpsertVenue(ctx, Venue{ID: "binance", Name: "Binance", Kind: VenueCEX, Region: RegionGlobal, Enabled: true}))
	btc, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", bitcoin())
	require.NoError(t, err)

	ids, err := store.UpsertVenuePairs(ctx, "binance", []VenuePair{
		{VenueSymbol: "BTCUSDT", BaseSymbol: "btc", QuoteSymbol: "usdt", BaseAssetID: &btc},
		{VenueSymbol: "ETHUSDT", BaseSymbol: "ETH", QuoteSymbol: "USDT"},
	})
	require.NoError(t, err)
	require.Len(t, ids, 2)

	// A later run does not move a mapping, and fills a missing one only.
	eth, _, err := store.CreateAsset(ctx, "ethereum", "coingecko", "ethereum", AssetDetails{Symbol: "ETH", Name: "Ethereum", Kind: KindCoin})
	require.NoError(t, err)
	again, err := store.UpsertVenuePairs(ctx, "binance", []VenuePair{
		{VenueSymbol: "BTCUSDT", BaseSymbol: "BTC", QuoteSymbol: "USDT", BaseAssetID: &eth},
		{VenueSymbol: "ETHUSDT", BaseSymbol: "ETH", QuoteSymbol: "USDT", BaseAssetID: &eth},
	})
	require.NoError(t, err)
	assert.Equal(t, ids, again, "the same ids")
	pairs, err := store.VenuePairs(ctx, "binance")
	require.NoError(t, err)
	assert.Equal(t, btc, *pairs["BTCUSDT"].BaseAssetID)
	assert.Equal(t, eth, *pairs["ETHUSDT"].BaseAssetID)

	enabled, err := store.VenueEnabled(ctx, "binance")
	require.NoError(t, err)
	assert.True(t, enabled)
	_, err = store.VenueEnabled(ctx, "nowhere")
	assert.ErrorIs(t, err, ErrNotFound)

	n, err := store.DeactivateVenuePairs(ctx, "binance", time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	pairs, err = store.VenuePairs(ctx, "binance")
	require.NoError(t, err)
	assert.False(t, pairs["BTCUSDT"].Active)
}

func TestSymbolCandidates_BestRankedFirstWithoutHidden(t *testing.T) {
	store, db := newStore(t)
	ctx := context.Background()
	rank := func(n int) *int { return &n }
	uni, _, err := store.CreateAsset(ctx, "uniswap", "coingecko", "uniswap", AssetDetails{Symbol: "UNI", Name: "Uniswap", Kind: KindToken, MarketCapRank: rank(25)})
	require.NoError(t, err)
	unicorn, _, err := store.CreateAsset(ctx, "unicorn", "coingecko", "unicorn", AssetDetails{Symbol: "UNI", Name: "Unicorn", Kind: KindToken})
	require.NoError(t, err)
	hidden, _, err := store.CreateAsset(ctx, "uni-scam", "coingecko", "uni-scam", AssetDetails{Symbol: "UNI", Name: "Uni Scam", Kind: KindToken, MarketCapRank: rank(1)})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE market_assets SET status = 'hidden' WHERE id = $1`, hidden)
	require.NoError(t, err)

	got, err := store.SymbolCandidates(ctx, []string{"uni", "NONE"})
	require.NoError(t, err)
	require.Len(t, got["UNI"], 2)
	assert.Equal(t, uni, got["UNI"][0].ID)
	assert.Equal(t, unicorn, got["UNI"][1].ID)
	assert.Nil(t, got["UNI"][1].Rank)
	assert.Empty(t, got["NONE"])
}

func TestUpsertTickers_BatchesAndNeverPutsAnOlderTickerBack(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	pair := newPair(t, store)
	now := time.Now().UTC().Truncate(time.Microsecond)

	n, err := store.UpsertTickers(ctx, []Ticker{{PairID: pair, LastPrice: *dec("68000.123456789012345678"), Bid: dec("67999"),
		VolumeBase: dec("12.5"), ObservedAt: now, FetchedAt: now}})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got, err := store.TickerOf(ctx, pair)
	require.NoError(t, err)
	assert.Equal(t, "68000.123456789012345678", got.LastPrice.String(), "every digit")
	assert.Nil(t, got.Ask)
	assert.Equal(t, "12.5", got.VolumeBase.String())

	n, err = store.UpsertTickers(ctx, []Ticker{{PairID: pair, LastPrice: *dec("1"), ObservedAt: now.Add(-time.Second), FetchedAt: now}})
	require.NoError(t, err)
	assert.Equal(t, 0, n, "an older observation is not written")

	_, err = store.UpsertTickers(ctx, []Ticker{{PairID: pair, LastPrice: *dec("-1"), ObservedAt: now, FetchedAt: now}})
	assert.Error(t, err)
}

func TestQuotesOf_OnlyEnabledVenuesOfTheRegions(t *testing.T) {
	store, db := newStore(t)
	ctx := context.Background()
	btc, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", bitcoin())
	require.NoError(t, err)
	now := time.Now().UTC()
	for _, v := range []Venue{
		{ID: "binance", Name: "Binance", Kind: VenueCEX, Region: RegionGlobal, Enabled: true},
		{ID: "nobitex", Name: "Nobitex", Kind: VenueCEX, Region: RegionIran, Enabled: true},
		{ID: "closed", Name: "Closed", Kind: VenueCEX, Region: RegionGlobal},
	} {
		require.NoError(t, store.UpsertVenue(ctx, v))
		ids, err := store.UpsertVenuePairs(ctx, v.ID, []VenuePair{{VenueSymbol: "BTC-X", BaseSymbol: "BTC", QuoteSymbol: "USDT", BaseAssetID: &btc}})
		require.NoError(t, err)
		_, err = store.UpsertTickers(ctx, []Ticker{{PairID: ids["BTC-X"], LastPrice: *dec("68000"), ObservedAt: now, FetchedAt: now}})
		require.NoError(t, err)
	}
	all, err := store.QuotesOf(ctx, btc, nil)
	require.NoError(t, err)
	assert.Len(t, all, 2, "not the venue that is not enabled")
	global, err := store.QuotesOf(ctx, btc, []string{RegionGlobal})
	require.NoError(t, err)
	require.Len(t, global, 1)
	assert.Equal(t, "binance", global[0].VenueID)
	assert.Equal(t, "68000", global[0].Ticker.LastPrice.String())

	_, err = db.Exec(`UPDATE market_venue_pairs SET active = FALSE WHERE venue_id = 'binance'`)
	require.NoError(t, err)
	global, err = store.QuotesOf(ctx, btc, []string{RegionGlobal})
	require.NoError(t, err)
	assert.Empty(t, global, "not an inactive pair")
}

func quote(venue, quoteSymbol, price string, age time.Duration, now time.Time) Quote {
	return Quote{VenueID: venue, QuoteSymbol: quoteSymbol,
		Ticker: Ticker{LastPrice: shared.MustNewDecimalFromString(price), ObservedAt: now.Add(-age)}}
}

func TestFlag_StaleAndOutliers(t *testing.T) {
	now := time.Now()
	rules := QualityRules{StaleAfter: 10 * time.Minute, MaxDeviation: decimal.RequireFromString("0.05"), MinVenues: 3}
	quotes := []Quote{
		quote("a", "USDT", "100", time.Minute, now),
		quote("b", "USDT", "101", time.Minute, now),
		quote("c", "USDT", "99", time.Minute, now),
		quote("d", "USDT", "120", time.Minute, now),  // 20 % off: an outlier
		quote("e", "USDT", "500", time.Hour, now),    // stale: no outlier, no say in the median
		quote("a", "USDT", "1000", time.Minute, now), // a second pair of venue a does not count twice
		quote("f", "IRT", "6800000000", 0, now),      // too few venues of IRT for a median
		quote("g", "USDT", "105", time.Minute, now),  // exactly 5 % from the median: not an outlier
	}
	Flag(quotes, now, rules)

	// The fresh prices per venue: 99, 100, 101, 105, 120: the median is 101.
	assert.Equal(t, "101", quotes[0].Median.String())
	for i, want := range []bool{false, false, false, true, false, true, false, false} {
		assert.Equal(t, want, quotes[i].Outlier, "quote %d", i)
	}
	assert.True(t, quotes[4].Stale)
	assert.False(t, quotes[0].Stale)
	assert.Nil(t, quotes[6].Median)

	// An even count takes the mean of the middle two.
	even := []Quote{quote("a", "USD", "1", 0, now), quote("b", "USD", "2", 0, now), quote("c", "USD", "3", 0, now), quote("d", "USD", "4", 0, now)}
	Flag(even, now, rules)
	assert.Equal(t, "2.5", even[0].Median.String())
}
