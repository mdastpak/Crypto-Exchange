package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMarketData_EndToEnd reads the market data catalog (#269) through the
// routes of the server, with the runtime role and the Redis cache that
// main sets up, and checks every answer against the OpenAPI document.
func TestMarketData_EndToEnd(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	store := marketdata.NewStore(env.db)
	rank := 1
	btc, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", marketdata.AssetDetails{
		Symbol: "BTC", Name: "Bitcoin", Kind: marketdata.KindCoin, MarketCapRank: &rank,
		Links: map[string]string{"website": "https://bitcoin.org"}, Attribution: "Data from CoinGecko"})
	require.NoError(t, err)
	_, err = store.AddContract(ctx, btc, "ethereum", "0x2260fac5e5542a773aa44fbcfedf7c193bc2c599", "coingecko")
	require.NoError(t, err)
	require.NoError(t, store.UpsertVenue(ctx, marketdata.Venue{ID: "binance", Name: "Binance", Kind: marketdata.VenueCEX, Region: marketdata.RegionGlobal, Enabled: true}))
	ids, err := store.UpsertVenuePairs(ctx, "binance", []marketdata.VenuePair{{VenueSymbol: "BTCUSDT", BaseSymbol: "BTC", QuoteSymbol: "USDT", BaseAssetID: &btc}})
	require.NoError(t, err)
	now := time.Now().UTC()
	price := shared.MustNewDecimalFromString("68000.5")
	_, err = store.UpsertTickers(ctx, []marketdata.Ticker{{PairID: ids["BTCUSDT"], LastPrice: price, Bid: &price, ObservedAt: now, FetchedAt: now}})
	require.NoError(t, err)
	hour := now.Truncate(time.Hour).Add(-time.Hour)
	_, err = store.WriteCandles(ctx, ids["BTCUSDT"], marketdata.Resolution1h, hour, hour.Add(time.Hour),
		[]marketdata.Candle{{OpenTime: hour, Open: price, High: price, Low: price, Close: price, Volume: price}})
	require.NoError(t, err)

	read := func(path string) (int, map[string]any) {
		status, raw := env.call(t, http.MethodGet, path, nil, "")
		var body map[string]any
		require.NoError(t, json.Unmarshal(raw, &body), string(raw))
		return status, body
	}
	status, body := read("/api/v1/market-data/assets")
	require.Equal(t, http.StatusOK, status, body)
	items := body["info"].(map[string]any)["items"].([]any)
	require.Len(t, items, 1)
	assert.Equal(t, "unverified", items[0].(map[string]any)["label"])

	status, body = read("/api/v1/market-data/assets/bitcoin")
	require.Equal(t, http.StatusOK, status, body)
	assert.Len(t, body["info"].(map[string]any)["contracts"], 1)

	status, body = read("/api/v1/market-data/assets/bitcoin/venues")
	require.Equal(t, http.StatusOK, status, body)
	quotes := body["info"].(map[string]any)["quotes"].([]any)
	require.Len(t, quotes, 1)
	assert.Equal(t, "68000.5", quotes[0].(map[string]any)["last"])

	status, body = read("/api/v1/market-data/assets/bitcoin/candles?venue=binance&quote=USDT&interval=1h")
	require.Equal(t, http.StatusOK, status, body)
	assert.Len(t, body["info"].(map[string]any)["candles"], 1)

	status, body = read("/api/v1/market-data/assets/nothing")
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, "market_asset_not_found", body["code"])
	status, body = read("/api/v1/market-data/assets/bitcoin/candles?venue=binance&quote=USDT&interval=2h")
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "validation_failed", body["code"])

	// The answer comes from Redis, with an ETag that a client sends back.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/market-data/assets/bitcoin", nil)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	etag := w.Header().Get("ETag")
	require.NotEmpty(t, etag)
	req = httptest.NewRequest(http.MethodGet, "/api/v1/market-data/assets/bitcoin", nil)
	req.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotModified, w.Code)

	// Hiding an asset takes effect once the answers are invalidated.
	_, err = env.db.Exec(`UPDATE market_assets SET status = 'hidden' WHERE id = $1`, btc)
	require.NoError(t, err)
	require.NoError(t, env.handlers.MarketData.Invalidate(ctx))
	status, _ = read("/api/v1/market-data/assets/bitcoin")
	assert.Equal(t, http.StatusNotFound, status)
}
