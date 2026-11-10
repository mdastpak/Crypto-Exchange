package marketdata

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/redistest"
)

// now is the time of the tests, which the handlers see.
var now = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

var rules = marketdata.QualityRules{StaleAfter: 10 * time.Minute, MaxDeviation: decimal.RequireFromString("0.05"), MinVenues: 3}

// fixture is a catalog of a few assets, venues, tickers, and candles.
type fixture struct {
	store *marketdata.Store
	db    *sql.DB
	btc   int64
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	conn := pgtest.Migrated(t, "testdb_marketdata_api")
	store := marketdata.NewStore(conn)
	ctx := context.Background()
	rank := func(n int) *int { return &n }
	btc, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", marketdata.AssetDetails{
		Symbol: "BTC", Name: "Bitcoin", Kind: marketdata.KindCoin, MarketCapRank: rank(1),
		Description: "Peer-to-peer electronic cash.", Links: map[string]string{"website": "https://bitcoin.org"},
		MaxSupply: dec("21000000"), Attribution: "Data from CoinGecko"})
	require.NoError(t, err)
	usdt, _, err := store.CreateAsset(ctx, "tether", "coingecko", "tether", marketdata.AssetDetails{
		Symbol: "USDT", Name: "Tether", Kind: marketdata.KindToken, MarketCapRank: rank(3)})
	require.NoError(t, err)
	_, err = store.AddContract(ctx, usdt, "tron", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "coingecko")
	require.NoError(t, err)
	_, _, err = store.CreateAsset(ctx, "ethereum", "coingecko", "ethereum", marketdata.AssetDetails{
		Symbol: "ETH", Name: "Ethereum", Kind: marketdata.KindCoin, MarketCapRank: rank(2)})
	require.NoError(t, err)
	_, _, err = store.CreateAsset(ctx, "scam", "coingecko", "scam", marketdata.AssetDetails{
		Symbol: "SCM", Name: "Scam", Kind: marketdata.KindToken})
	require.NoError(t, err)
	_, err = conn.Exec(`UPDATE market_assets SET status = 'hidden' WHERE slug = 'scam'`)
	require.NoError(t, err)

	// Four venues: three agree, one is far off, and Nobitex quotes tomans.
	for _, v := range []struct {
		venue, region, symbol, quote, price string
		age                                 time.Duration
	}{
		{"binance", marketdata.RegionGlobal, "BTCUSDT", "USDT", "68000", time.Minute},
		{"okx", marketdata.RegionGlobal, "BTC-USDT", "USDT", "68100", time.Minute},
		{"kraken", marketdata.RegionGlobal, "XBTUSDT", "USDT", "67900", time.Minute},
		{"coinbase", marketdata.RegionGlobal, "BTC-USDT", "USDT", "80000", time.Minute},
		{"nobitex", marketdata.RegionIran, "BTCIRT", "IRT", "7000000000", time.Hour},
	} {
		require.NoError(t, store.UpsertVenue(ctx, marketdata.Venue{ID: v.venue, Name: v.venue, Kind: marketdata.VenueCEX, Region: v.region, Enabled: true}))
		ids, err := store.UpsertVenuePairs(ctx, v.venue, []marketdata.VenuePair{{VenueSymbol: v.symbol, BaseSymbol: "BTC", QuoteSymbol: v.quote, BaseAssetID: &btc}})
		require.NoError(t, err)
		observed := now.Add(-v.age)
		_, err = store.UpsertTickers(ctx, []marketdata.Ticker{{PairID: ids[v.symbol], LastPrice: *dec(v.price), Bid: dec(v.price), ObservedAt: observed, FetchedAt: observed}})
		require.NoError(t, err)
		if v.venue == "binance" {
			from := now.Truncate(time.Hour).Add(-3 * time.Hour)
			var candles []marketdata.Candle
			for i, price := range []string{"67000", "67500", "68000.123456789012345678"} {
				p := shared.MustNewDecimalFromString(price)
				candles = append(candles, marketdata.Candle{OpenTime: from.Add(time.Duration(i) * time.Hour), Open: p, High: p, Low: p, Close: p, Volume: p})
			}
			_, err = store.WriteCandles(ctx, ids[v.symbol], marketdata.Resolution1h, from, now.Truncate(time.Hour), candles)
			require.NoError(t, err)
		}
	}
	return fixture{store: store, db: conn, btc: btc}
}

func dec(s string) *shared.Decimal {
	d := shared.MustNewDecimalFromString(s)
	return &d
}

// memoryCache is a Cache in memory, which several handlers may share, as
// instances share Redis.
type memoryCache struct {
	values     map[string][]byte
	generation int64
	fail       bool
}

func newMemoryCache() *memoryCache { return &memoryCache{values: map[string][]byte{}} }

func (m *memoryCache) Get(_ context.Context, key string) ([]byte, bool, error) {
	if m.fail {
		return nil, false, assert.AnError
	}
	v, ok := m.values[key]
	return v, ok, nil
}

func (m *memoryCache) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	if m.fail {
		return assert.AnError
	}
	m.values[key] = value
	return nil
}

func (m *memoryCache) Generation(context.Context) (int64, error) {
	if m.fail {
		return 0, assert.AnError
	}
	return m.generation, nil
}

func (m *memoryCache) Invalidate(context.Context) error {
	m.generation++
	return nil
}

func newRouter(h *Handler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.Register(r.Group("/api/v1"))
	return r
}

type response struct {
	Code int
	Body map[string]any
	Raw  string
	ETag string
	Hdr  http.Header
}

func get(t *testing.T, r http.Handler, path string, header ...string) response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	out := response{Code: w.Code, Raw: w.Body.String(), ETag: w.Header().Get("ETag"), Hdr: w.Header()}
	if w.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out.Body), w.Body.String())
	}
	return out
}

func info(r response) map[string]any { return r.Body["info"].(map[string]any) }

func newHandler(f fixture, cache Cache, regions ...string) *Handler {
	return New(f.store, cache, Options{Regions: regions, CacheTTL: 30 * time.Second, Rules: rules, Now: func() time.Time { return now }})
}

func TestListAssets_UnverifiedByRankWithoutHidden(t *testing.T) {
	f := newFixture(t)
	r := newRouter(newHandler(f, nil))

	res := get(t, r, "/api/v1/market-data/assets?limit=2")
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	items := info(res)["items"].([]any)
	require.Len(t, items, 2)
	first := items[0].(map[string]any)
	assert.Equal(t, "bitcoin", first["slug"])
	assert.Equal(t, false, first["verified"], "every asset is unverified until linked")
	assert.Equal(t, "unverified", first["label"])
	assert.Equal(t, "ethereum", items[1].(map[string]any)["slug"])
	assert.Equal(t, "21000000", first["max_supply"], "a decimal as a string")
	assert.NotContains(t, first, "description", "only the asset itself has its description")

	next := info(res)["next_cursor"].(string)
	res = get(t, r, "/api/v1/market-data/assets?limit=2&cursor="+url.QueryEscape(next))
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	items = info(res)["items"].([]any)
	require.Len(t, items, 1, "never the hidden asset")
	assert.Equal(t, "tether", items[0].(map[string]any)["slug"])
	assert.NotContains(t, info(res), "next_cursor")

	res = get(t, r, "/api/v1/market-data/assets?q=usdt")
	require.Equal(t, http.StatusOK, res.Code)
	assert.Len(t, info(res)["items"], 1)
	res = get(t, r, "/api/v1/market-data/assets?q=scam")
	assert.Empty(t, info(res)["items"], "a search never finds a hidden asset")

	for _, bad := range []string{"limit=0", "limit=201", "cursor=nonsense", "q=" + url.QueryEscape(string(make([]byte, 65)))} {
		res = get(t, r, "/api/v1/market-data/assets?"+bad)
		assert.Equal(t, http.StatusBadRequest, res.Code, bad)
		assert.Equal(t, "validation_failed", res.Body["code"], bad)
	}
}

func TestListAssets_Newest(t *testing.T) {
	f := newFixture(t)
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i, slug := range []string{"bitcoin", "tether", "ethereum", "scam"} {
		_, err := f.db.Exec(`UPDATE market_assets SET created_at = $1 WHERE slug = $2`, base.Add(time.Duration(i)*time.Hour), slug)
		require.NoError(t, err)
	}
	r := newRouter(newHandler(f, nil))

	res := get(t, r, "/api/v1/market-data/assets?sort=newest&limit=2")
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	items := info(res)["items"].([]any)
	require.Len(t, items, 2)
	assert.Equal(t, "ethereum", items[0].(map[string]any)["slug"], "the newest first, never the hidden")
	assert.Equal(t, "2026-11-01T02:00:00Z", items[0].(map[string]any)["created_at"])
	assert.Equal(t, "tether", items[1].(map[string]any)["slug"])

	next := info(res)["next_cursor"].(string)
	res = get(t, r, "/api/v1/market-data/assets?sort=newest&limit=2&cursor="+url.QueryEscape(next))
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	items = info(res)["items"].([]any)
	require.Len(t, items, 1)
	assert.Equal(t, "bitcoin", items[0].(map[string]any)["slug"])

	// A cursor holds its order: the other order refuses it.
	res = get(t, r, "/api/v1/market-data/assets?limit=2&cursor="+url.QueryEscape(next))
	assert.Equal(t, http.StatusBadRequest, res.Code)
	rank := info(get(t, r, "/api/v1/market-data/assets?sort=rank&limit=1"))["next_cursor"].(string)
	res = get(t, r, "/api/v1/market-data/assets?sort=newest&cursor="+url.QueryEscape(rank))
	assert.Equal(t, http.StatusBadRequest, res.Code)
	assert.Equal(t, "validation_failed", res.Body["code"])

	res = get(t, r, "/api/v1/market-data/assets?sort=newest&q=tether")
	require.Equal(t, http.StatusOK, res.Code)
	assert.Len(t, info(res)["items"], 1, "the query works with this order too")

	res = get(t, r, "/api/v1/market-data/assets?sort=volume")
	assert.Equal(t, http.StatusBadRequest, res.Code)
	assert.Equal(t, "validation_failed", res.Body["code"])
}

func TestListAssets_LinkedIsVerified(t *testing.T) {
	f := newFixture(t)
	currency, err := json.Marshal(models.CurrencyInfo{Symbol: "BTC", EnName: "Bitcoin", FaName: "Bitcoin"})
	require.NoError(t, err)
	var hk string
	require.NoError(t, f.db.QueryRow(`INSERT INTO currencies (status, info) VALUES ('approved', $1::jsonb) RETURNING hk`, currency).Scan(&hk))
	_, err = f.db.Exec(`UPDATE market_assets SET status = 'linked', currency_hk = $1 WHERE id = $2`, hk, f.btc)
	require.NoError(t, err)

	res := get(t, newRouter(newHandler(f, nil)), "/api/v1/market-data/assets/bitcoin")
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Equal(t, true, info(res)["verified"])
	assert.Equal(t, "verified", info(res)["label"])
}

func TestGetAsset_DetailsAndContracts(t *testing.T) {
	f := newFixture(t)
	r := newRouter(newHandler(f, nil))

	res := get(t, r, "/api/v1/market-data/assets/bitcoin")
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Equal(t, "Peer-to-peer electronic cash.", info(res)["description"])
	assert.Equal(t, "https://bitcoin.org", info(res)["links"].(map[string]any)["website"])
	assert.Equal(t, "Data from CoinGecko", info(res)["attribution"])
	assert.Equal(t, "unverified", info(res)["label"])

	res = get(t, r, "/api/v1/market-data/assets/tether")
	require.Equal(t, http.StatusOK, res.Code)
	contracts := info(res)["contracts"].([]any)
	require.Len(t, contracts, 1)
	assert.Equal(t, "tron", contracts[0].(map[string]any)["network"])

	for _, slug := range []string{"scam", "nothing"} {
		res = get(t, r, "/api/v1/market-data/assets/"+slug)
		assert.Equal(t, http.StatusNotFound, res.Code, slug)
		assert.Equal(t, CodeAssetNotFound, res.Body["code"], "a hidden asset is not found either")
	}
}

func TestGetVenues_ComparesByQuoteWithFlags(t *testing.T) {
	f := newFixture(t)
	res := get(t, newRouter(newHandler(f, nil)), "/api/v1/market-data/assets/bitcoin/venues")
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	body := info(res)
	assert.Equal(t, "unverified", body["label"])
	assert.Equal(t, float64(600), body["stale_after_seconds"])
	assert.Equal(t, "5", body["outlier_percent"])
	quotes := map[string]map[string]any{}
	for _, q := range body["quotes"].([]any) {
		quotes[q.(map[string]any)["venue"].(string)] = q.(map[string]any)
	}
	require.Len(t, quotes, 5)
	assert.Equal(t, true, quotes["coinbase"]["outlier"], "far from the median of the venues")
	assert.Equal(t, false, quotes["binance"]["outlier"])
	assert.Equal(t, false, quotes["binance"]["stale"])
	assert.NotEmpty(t, quotes["binance"]["median"])
	assert.Equal(t, true, quotes["nobitex"]["stale"], "an hour old")
	assert.Equal(t, "IRT", quotes["nobitex"]["quote"])
	assert.Equal(t, "ir", quotes["nobitex"]["region"])
	assert.NotContains(t, quotes["nobitex"], "median", "one venue quotes IRT, too few for a median")

	global := get(t, newRouter(newHandler(f, nil, marketdata.RegionGlobal)), "/api/v1/market-data/assets/bitcoin/venues")
	require.Equal(t, http.StatusOK, global.Code)
	assert.Len(t, info(global)["quotes"], 4, "an international release shows no Iranian venue")

	res = get(t, newRouter(newHandler(f, nil)), "/api/v1/market-data/assets/scam/venues")
	assert.Equal(t, http.StatusNotFound, res.Code)
}

func TestGetCandles_ChartOfAPair(t *testing.T) {
	f := newFixture(t)
	r := newRouter(newHandler(f, nil))

	res := get(t, r, "/api/v1/market-data/assets/bitcoin/candles?venue=Binance&quote=usdt&interval=1h")
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	body := info(res)
	assert.Equal(t, "binance", body["venue"])
	assert.Equal(t, "USDT", body["quote"])
	candles := body["candles"].([]any)
	require.Len(t, candles, 3, "the last 500 hours by default")
	last := candles[2].(map[string]any)
	assert.Equal(t, "68000.123456789012345678", last["c"], "every digit")
	assert.Equal(t, now.Truncate(time.Hour).Add(-time.Hour).Format(time.RFC3339), last["t"])

	from := now.Truncate(time.Hour).Add(-2 * time.Hour).Format(time.RFC3339)
	res = get(t, r, "/api/v1/market-data/assets/bitcoin/candles?venue=binance&quote=USDT&interval=1h&from="+url.QueryEscape(from))
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Len(t, info(res)["candles"], 2)

	res = get(t, r, "/api/v1/market-data/assets/bitcoin/candles?venue=binance&quote=USDT&interval=1d")
	require.Equal(t, http.StatusOK, res.Code)
	assert.Empty(t, info(res)["candles"], "an empty chart is a list")

	for _, bad := range []string{
		"venue=binance&quote=USDT&interval=2h",
		"quote=USDT&interval=1h",
		"venue=binance&interval=1h",
		"venue=binance&quote=USDT&interval=1h&from=yesterday",
		"venue=binance&quote=USDT&interval=1h&from=2026-11-08T00:00:00Z&to=2026-11-07T00:00:00Z",
		"venue=binance&quote=USDT&interval=5m&from=2026-11-01T00:00:00Z&to=2026-11-08T00:00:00Z",
	} {
		res = get(t, r, "/api/v1/market-data/assets/bitcoin/candles?"+bad)
		assert.Equal(t, http.StatusBadRequest, res.Code, bad)
	}
	res = get(t, r, "/api/v1/market-data/assets/bitcoin/candles?venue=binance&quote=IRT&interval=1h")
	assert.Equal(t, http.StatusNotFound, res.Code)
	assert.Equal(t, CodePairNotFound, res.Body["code"])
	res = get(t, newRouter(newHandler(f, nil, marketdata.RegionGlobal)), "/api/v1/market-data/assets/bitcoin/candles?venue=nobitex&quote=IRT&interval=1h")
	assert.Equal(t, http.StatusNotFound, res.Code, "not a venue of a region this release hides")
}

func TestCache_SharedAcrossInstancesUntilInvalidated(t *testing.T) {
	f := newFixture(t)
	cache := newMemoryCache()
	first, second := newHandler(f, cache), newHandler(f, cache)
	path := "/api/v1/market-data/assets/bitcoin"

	res := get(t, newRouter(first), path)
	require.Equal(t, http.StatusOK, res.Code)
	assert.Equal(t, "public, max-age=30", res.Hdr.Get("Cache-Control"))
	require.NotEmpty(t, res.ETag)
	assert.Len(t, cache.values, 1)

	// Another instance answers from the cache: a change in the database is
	// not seen until the answers are invalidated.
	_, err := f.db.Exec(`UPDATE market_assets SET name = 'Bitcoin Renamed' WHERE id = $1`, f.btc)
	require.NoError(t, err)
	cached := get(t, newRouter(second), path)
	assert.Equal(t, "Bitcoin", info(cached)["name"])
	assert.Equal(t, res.ETag, cached.ETag, "the same answer has the same ETag on every instance")

	notModified := get(t, newRouter(second), path, "If-None-Match", `W/`+res.ETag)
	assert.Equal(t, http.StatusNotModified, notModified.Code)
	assert.Empty(t, notModified.Raw)

	require.NoError(t, first.Invalidate(context.Background()))
	fresh := get(t, newRouter(second), path, "If-None-Match", res.ETag)
	require.Equal(t, http.StatusOK, fresh.Code)
	assert.Equal(t, "Bitcoin Renamed", info(fresh)["name"])
	assert.NotEqual(t, res.ETag, fresh.ETag)

	// Errors are not cached; the order of the query does not matter.
	assert.Equal(t, http.StatusNotFound, get(t, newRouter(first), "/api/v1/market-data/assets/nothing").Code)
	before := len(cache.values)
	get(t, newRouter(first), "/api/v1/market-data/assets?limit=2&q=bit")
	get(t, newRouter(first), "/api/v1/market-data/assets?q=bit&limit=2")
	assert.Len(t, cache.values, before+1)
}

func TestCache_AnswersWithoutTheCache(t *testing.T) {
	f := newFixture(t)
	cache := newMemoryCache()
	cache.fail = true
	res := get(t, newRouter(newHandler(f, cache)), "/api/v1/market-data/assets/bitcoin")
	require.Equal(t, http.StatusOK, res.Code, "a failing cache costs speed, not answers")
	assert.NotEmpty(t, res.ETag)
}

func TestRedisCache_GenerationAndInvalidate(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: redistest.Addr, DB: redistest.MarketData})
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis is not reachable: %v", err)
	}
	require.NoError(t, client.FlushDB(ctx).Err())
	t.Cleanup(func() { client.FlushDB(ctx); client.Close() })
	cache := RedisCache{Client: client}

	gen, err := cache.Generation(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), gen)
	_, ok, err := cache.Get(ctx, "k")
	require.NoError(t, err)
	assert.False(t, ok)
	require.NoError(t, cache.Set(ctx, "k", []byte("v"), time.Minute))
	got, ok, err := cache.Get(ctx, "k")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "v", string(got))
	ttl, err := client.TTL(ctx, "k").Result()
	require.NoError(t, err)
	assert.Positive(t, ttl, "every answer expires")

	require.NoError(t, cache.Invalidate(ctx))
	gen, err = cache.Generation(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), gen)
}

func TestMatches(t *testing.T) {
	assert.True(t, matches(`"a"`, `"a"`))
	assert.True(t, matches(`W/"a"`, `"a"`))
	assert.True(t, matches(`"b", W/"a"`, `"a"`))
	assert.True(t, matches(`*`, `"a"`))
	assert.False(t, matches(``, `"a"`))
	assert.False(t, matches(`"b"`, `"a"`))
}
