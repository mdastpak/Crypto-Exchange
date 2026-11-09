package marketdata

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/marketdata/source/coingecko"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ageFixture moves the changes of the fixture's assets an hour back, a
// minute apart, in the order of the slugs, so that the feed has them.
func ageFixture(t *testing.T, f fixture) {
	t.Helper()
	_, err := f.db.Exec(`UPDATE market_assets SET updated_at = $1::timestamptz + (CASE slug
		WHEN 'bitcoin' THEN 0 WHEN 'ethereum' THEN 1 WHEN 'scam' THEN 2 ELSE 3 END) * interval '1 minute'`,
		time.Now().Add(-time.Hour))
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE market_assets SET attribution = $1 WHERE slug = 'bitcoin'`, coingecko.Attribution)
	require.NoError(t, err)
}

func TestFeedAssets_FollowsTheChanges(t *testing.T) {
	f := newFixture(t)
	ageFixture(t, f)
	r := newRouter(newHandler(f, nil))

	res := get(t, r, "/api/v1/market-data/feed/assets?limit=3")
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	items := info(res)["items"].([]any)
	require.Len(t, items, 3)
	assert.Equal(t, true, info(res)["has_more"])

	btc := items[0].(map[string]any)
	assert.Equal(t, "bitcoin", btc["slug"])
	assert.Equal(t, false, btc["removed"])
	asset := btc["asset"].(map[string]any)
	assert.Equal(t, "Peer-to-peer electronic cash.", asset["description"])
	assert.Equal(t, "unverified", asset["label"])
	assert.Equal(t, map[string]any{"text": "Data provided by CoinGecko", "url": "https://www.coingecko.com"}, btc["attribution"])
	assert.Equal(t, map[string]any{"@context": "https://schema.org", "@type": "Thing", "name": "Bitcoin", "alternateName": "BTC",
		"description": "Peer-to-peer electronic cash.", "sameAs": []any{"https://bitcoin.org"}}, btc["structured_data"])

	scam := items[2].(map[string]any)
	assert.Equal(t, "scam", scam["slug"])
	assert.Equal(t, true, scam["removed"], "a hidden asset is listed as removed")
	assert.NotContains(t, scam, "asset", "and nothing of it is served")
	assert.NotContains(t, scam, "structured_data")

	next := info(res)["next_cursor"].(string)
	res = get(t, r, "/api/v1/market-data/feed/assets?limit=3&cursor="+url.QueryEscape(next))
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	items = info(res)["items"].([]any)
	require.Len(t, items, 1)
	usdt := items[0].(map[string]any)
	assert.Equal(t, "tether", usdt["slug"])
	assert.Equal(t, []any{map[string]any{"network": "tron", "address": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"}},
		usdt["asset"].(map[string]any)["contracts"])
	assert.Equal(t, []any{map[string]any{"@type": "PropertyValue", "propertyID": "tron", "value": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"}},
		usdt["structured_data"].(map[string]any)["identifier"])
	assert.NotContains(t, usdt, "attribution", "no attribution, no credit")
	assert.Equal(t, false, info(res)["has_more"])

	// The last page has a cursor too, which waits for the next change.
	last := info(res)["next_cursor"].(string)
	res = get(t, r, "/api/v1/market-data/feed/assets?cursor="+url.QueryEscape(last))
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Empty(t, info(res)["items"])
	assert.Equal(t, last, info(res)["next_cursor"])
	assert.Equal(t, false, info(res)["has_more"])

	_, err := f.db.Exec(`UPDATE market_assets SET name = 'Ether', updated_at = NOW() - interval '2 minutes' WHERE slug = 'ethereum'`)
	require.NoError(t, err)
	res = get(t, r, "/api/v1/market-data/feed/assets?cursor="+url.QueryEscape(last))
	items = info(res)["items"].([]any)
	require.Len(t, items, 1, "a changed asset comes again")
	assert.Equal(t, "Ether", items[0].(map[string]any)["asset"].(map[string]any)["name"])

	_, err = f.db.Exec(`UPDATE market_assets SET name = 'Tether USD', updated_at = NOW() WHERE slug = 'tether'`)
	require.NoError(t, err)
	res = get(t, r, "/api/v1/market-data/feed/assets?updated_since="+url.QueryEscape(time.Now().Add(-5*time.Minute).UTC().Format(time.RFC3339)))
	items = info(res)["items"].([]any)
	require.Len(t, items, 1, "a change within the last minute waits")
	assert.Equal(t, "ethereum", items[0].(map[string]any)["slug"])
}

func TestFeedAssets_RefusesABadRequest(t *testing.T) {
	f := newFixture(t)
	r := newRouter(newHandler(f, nil))
	for _, query := range []string{"updated_since=yesterday", "cursor=nothing", "limit=0"} {
		res := get(t, r, "/api/v1/market-data/feed/assets?"+query)
		assert.Equal(t, http.StatusBadRequest, res.Code, query)
		assert.Equal(t, "validation_failed", res.Body["code"], query)
	}
}
