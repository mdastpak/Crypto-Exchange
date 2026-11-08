package marketdata

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublicAssets_ByRankWithoutHiddenAndPaged(t *testing.T) {
	store, db := newStore(t)
	ctx := context.Background()
	rank := func(n int) *int { return &n }
	for _, a := range []struct {
		slug, symbol, name string
		rank               *int
	}{
		{"bitcoin", "BTC", "Bitcoin", rank(1)},
		{"ethereum", "ETH", "Ethereum", rank(2)},
		{"bitcoin-cash", "BCH", "Bitcoin Cash", rank(20)},
		{"wrapped-bitcoin", "WBTC", "Wrapped Bitcoin", nil},
		{"a_coin", "ACN", "100% Coin", nil},
		{"scam", "BTC", "Bitcoin Scam", rank(3)},
	} {
		_, _, err := store.CreateAsset(ctx, a.slug, "coingecko", a.slug, AssetDetails{Symbol: a.symbol, Name: a.name, Kind: KindCoin, MarketCapRank: a.rank})
		require.NoError(t, err)
	}
	_, err := db.Exec(`UPDATE market_assets SET status = 'hidden' WHERE slug = 'scam'`)
	require.NoError(t, err)

	slugs := func(assets []Asset) []string {
		out := []string{}
		for _, a := range assets {
			out = append(out, a.Slug)
		}
		return out
	}
	all, err := store.PublicAssets(ctx, "", nil, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"bitcoin", "ethereum", "bitcoin-cash", "a-coin", "wrapped-bitcoin"}, slugs(all),
		"by rank, the unranked last by slug, never the hidden")

	// Pages that follow one another hold every asset once.
	var paged []Asset
	var after *ListPosition
	for {
		page, err := store.PublicAssets(ctx, "", after, 2)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		paged = append(paged, page...)
		p := PositionOf(page[len(page)-1])
		after = &p
	}
	assert.Equal(t, slugs(all), slugs(paged))

	found, err := store.PublicAssets(ctx, "btc", nil, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"bitcoin"}, slugs(found), "the exact symbol, in any case")
	found, err = store.PublicAssets(ctx, "BITCOIN", nil, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"bitcoin", "bitcoin-cash", "wrapped-bitcoin"}, slugs(found), "a part of the name")
	found, err = store.PublicAssets(ctx, "100%", nil, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"a-coin"}, slugs(found), "a % in the query is literal")
	found, err = store.PublicAssets(ctx, "_", nil, 10)
	require.NoError(t, err)
	assert.Empty(t, found, "an _ in the query is literal")
}

func TestPublicAsset_NotAHiddenOne(t *testing.T) {
	store, db := newStore(t)
	ctx := context.Background()
	id, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", bitcoin())
	require.NoError(t, err)
	got, err := store.PublicAsset(ctx, "bitcoin")
	require.NoError(t, err)
	assert.Equal(t, id, got.ID)
	assert.Equal(t, "https://bitcoin.org", got.Links["website"])

	_, err = db.Exec(`UPDATE market_assets SET status = 'hidden' WHERE id = $1`, id)
	require.NoError(t, err)
	_, err = store.PublicAsset(ctx, "bitcoin")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = store.PublicAsset(ctx, "nothing")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestContractsOf_ByNetwork(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	id, _, err := store.CreateAsset(ctx, "tether", "coingecko", "tether", AssetDetails{Symbol: "USDT", Name: "Tether", Kind: KindToken})
	require.NoError(t, err)
	none, err := store.ContractsOf(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, none)
	assert.NotNil(t, none, "an empty list, not null")
	for _, c := range []Contract{{"tron", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"}, {"ethereum", "0xdAC17F958D2ee523a2206206994597C13D831ec7"}} {
		_, err := store.AddContract(ctx, id, c.Network, c.Address, "coingecko")
		require.NoError(t, err)
	}
	got, err := store.ContractsOf(ctx, id)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "ethereum", got[0].Network)
	assert.Equal(t, "tron", got[1].Network)
}

func TestPairOf_AnActivePairOfAnEnabledVenueOfTheRegions(t *testing.T) {
	store, db := newStore(t)
	ctx := context.Background()
	btc, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", bitcoin())
	require.NoError(t, err)
	require.NoError(t, store.UpsertVenue(ctx, Venue{ID: "nobitex", Name: "Nobitex", Kind: VenueCEX, Region: RegionIran, Enabled: true}))
	ids, err := store.UpsertVenuePairs(ctx, "nobitex", []VenuePair{{VenueSymbol: "BTCIRT", BaseSymbol: "BTC", QuoteSymbol: "IRT", BaseAssetID: &btc}})
	require.NoError(t, err)

	got, err := store.PairOf(ctx, btc, "nobitex", "irt", nil)
	require.NoError(t, err)
	assert.Equal(t, ids["BTCIRT"], got)
	_, err = store.PairOf(ctx, btc, "nobitex", "IRT", []string{RegionGlobal})
	assert.ErrorIs(t, err, ErrNotFound, "not a venue of another region")
	_, err = store.PairOf(ctx, btc, "nobitex", "USDT", nil)
	assert.ErrorIs(t, err, ErrNotFound)

	_, err = db.Exec(`UPDATE market_venues SET enabled = FALSE WHERE id = 'nobitex'`)
	require.NoError(t, err)
	_, err = store.PairOf(ctx, btc, "nobitex", "IRT", nil)
	assert.ErrorIs(t, err, ErrNotFound, "not a venue that is not enabled")
}
