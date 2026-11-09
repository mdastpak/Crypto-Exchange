package marketdata

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFeedAssets_InTheOrderTheyChangedAfterTheDelay(t *testing.T) {
	store, db := newStore(t)
	ctx := context.Background()
	ids := map[string]int64{}
	for _, slug := range []string{"bitcoin", "ethereum", "tether", "scam", "fresh"} {
		id, _, err := store.CreateAsset(ctx, slug, "coingecko", slug, AssetDetails{Symbol: "X", Name: slug, Kind: KindCoin})
		require.NoError(t, err)
		ids[slug] = id
	}
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	set := func(slug string, at time.Time) {
		t.Helper()
		_, err := db.Exec(`UPDATE market_assets SET updated_at = $2 WHERE slug = $1`, slug, at)
		require.NoError(t, err)
	}
	set("tether", base)
	set("ethereum", base.Add(time.Minute))
	set("bitcoin", base.Add(time.Minute))
	set("scam", base.Add(2*time.Minute))
	_, err := db.Exec(`UPDATE market_assets SET status = 'hidden' WHERE slug = 'scam'`)
	require.NoError(t, err)
	set("scam", base.Add(2*time.Minute))
	// fresh changed now, within the delay.

	slugs := func(assets []Asset) []string {
		out := []string{}
		for _, a := range assets {
			out = append(out, a.Slug)
		}
		return out
	}
	all, err := store.FeedAssets(ctx, FeedPosition{}, time.Minute, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"tether", "bitcoin", "ethereum", "scam"}, slugs(all),
		"by the time of the change, then the slug; hidden ones too; not one changed within the delay")

	// Pages that follow one another hold every asset once.
	var paged []Asset
	after := FeedPosition{}
	for {
		page, err := store.FeedAssets(ctx, after, time.Minute, 1)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		paged = append(paged, page...)
		after = FeedPositionOf(page[len(page)-1])
	}
	assert.Equal(t, slugs(all), slugs(paged))

	since, err := store.FeedAssets(ctx, FeedPosition{UpdatedAt: base.Add(time.Minute)}, time.Minute, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"bitcoin", "ethereum", "scam"}, slugs(since), "the changes at the time itself are included")

	withFresh, err := store.FeedAssets(ctx, after, 0, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"fresh"}, slugs(withFresh))

	// A new contract changes the asset; a known one does not.
	_, err = store.AddContract(ctx, ids["tether"], "tron", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "coingecko")
	require.NoError(t, err)
	var changed time.Time
	require.NoError(t, db.QueryRow(`SELECT updated_at FROM market_assets WHERE slug = 'tether'`).Scan(&changed))
	assert.True(t, changed.After(base.Add(time.Hour)), "a new contract moves the asset to the end of the feed")
	set("tether", base)
	_, err = store.AddContract(ctx, ids["tether"], "tron", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "coingecko")
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(`SELECT updated_at FROM market_assets WHERE slug = 'tether'`).Scan(&changed))
	assert.True(t, changed.Equal(base), "a known contract changes nothing")

	contracts, err := store.ContractsOfAssets(ctx, []int64{ids["tether"], ids["bitcoin"]})
	require.NoError(t, err)
	assert.Equal(t, map[int64][]Contract{ids["tether"]: {{Network: "tron", Address: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"}}}, contracts)

	_, err = store.FeedAssets(ctx, FeedPosition{}, -time.Second, 10)
	assert.Error(t, err)
}
