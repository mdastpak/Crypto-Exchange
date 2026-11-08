package exchange

import (
	"context"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source/sourcetest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hour(h int) time.Time { return time.Date(2026, 11, 8, h, 0, 0, 0, time.UTC) }

// candles reads the 1h candles in [15:00, 20:00) at 19:00: those of 16, 17,
// and 18; the candle of 19 is still open.
func candles(t *testing.T, src source.VenueSource, symbol string) []source.Candle {
	t.Helper()
	cs, ok := src.(source.CandleSource)
	require.True(t, ok, "every venue is a candle source")
	got, err := cs.Candles(context.Background(), symbol, source.Interval1h, hour(15), hour(20))
	require.NoError(t, err)
	sourcetest.CheckCandles(t, cs, symbol, source.Interval1h, readAt)
	return got
}

func opens(cs []source.Candle) []time.Time {
	var out []time.Time
	for _, c := range cs {
		out = append(out, c.OpenTime)
	}
	return out
}

var closedHours = []time.Time{hour(16), hour(17), hour(18)}

func TestBinanceCandles(t *testing.T) {
	src, asked := venue(t, Binance, map[string]string{"/api/v3/klines": "binance_klines.json"})
	got := candles(t, src, "BTCUSDT")
	assert.Equal(t, closedHours, opens(got))
	assert.Equal(t, "68200.123456789012345678", got[2].Close.String(), "every digit")
	assert.Equal(t, "120.5", got[0].Volume.String())
	assert.Contains(t, (*asked)[0], "interval=1h")
	assert.Contains(t, (*asked)[0], "symbol=BTCUSDT")
	assert.Equal(t, 1000, src.(source.CandleSource).MaxCandles())
}

func TestOKXCandles(t *testing.T) {
	src, asked := venue(t, OKX, map[string]string{"/api/v5/market/history-candles": "okx_candles.json"})
	got := candles(t, src, "BTC-USDT")
	assert.Equal(t, closedHours, opens(got), "oldest first, the open candle left out")
	assert.Equal(t, "68200.5", got[2].Open.String())
	assert.Contains(t, (*asked)[0], "bar=1H")
}

func TestKrakenCandles(t *testing.T) {
	src, _ := venue(t, Kraken, map[string]string{"/0/public/OHLC": "kraken_ohlc.json"})
	got := candles(t, src, "XXBTZUSD")
	assert.Equal(t, closedHours, opens(got))
	assert.Equal(t, "42.12345678", got[0].Volume.String(), "the volume, not the VWAP")
}

func TestCoinbaseCandles(t *testing.T) {
	src, asked := venue(t, Coinbase, map[string]string{"/api/v3/brokerage/market/products/BTC-USD/candles": "coinbase_candles.json"})
	got := candles(t, src, "BTC-USD")
	assert.Equal(t, closedHours, opens(got))
	assert.Equal(t, "68200.12", got[2].Open.String())
	assert.Contains(t, (*asked)[0], "granularity=ONE_HOUR")
}

func TestNobitexCandles_RialsAreTomans(t *testing.T) {
	src, asked := venue(t, Nobitex, map[string]string{"/market/udf/history": "nobitex_udf.json"})
	got := candles(t, src, "btc-rls")
	assert.Equal(t, closedHours, opens(got))
	assert.Equal(t, "68000000001.6", got[0].Open.String(), "680000000016 rials")
	assert.Equal(t, "68000000000.5", got[0].Close.String())
	assert.Equal(t, "1.5", got[0].Volume.String(), "the volume is in the base asset")
	assert.Contains(t, (*asked)[0], "symbol=BTCIRT")
	assert.Contains(t, (*asked)[0], "resolution=60")
	assert.False(t, src.(interface{ Supports(source.Interval) bool }).Supports(source.Interval1d), "Tehran's days")
}

func TestWallexCandles(t *testing.T) {
	src, asked := venue(t, Wallex, map[string]string{"/v1/udf/history": "wallex_udf.json"})
	got := candles(t, src, "BTCTMN")
	assert.Equal(t, closedHours, opens(got))
	assert.Equal(t, "2.25", got[0].Volume.String())
	assert.Contains(t, (*asked)[0], "symbol=BTCTMN")

	empty, _ := venue(t, Wallex, map[string]string{"/v1/udf/history": "udf_nodata.json"})
	none, err := empty.(source.CandleSource).Candles(context.Background(), "BTCTMN", source.Interval1h, hour(15), hour(20))
	require.NoError(t, err)
	assert.Empty(t, none, "no data is no candles")
	_, err = empty.(source.CandleSource).Candles(context.Background(), "BTCTMN", source.Interval1d, hour(0), hour(20))
	assert.ErrorIs(t, err, source.ErrNotFound)
}

// TestCandleIntervals_EveryVenueGivesTheFineResolutions checks the 1m and
// 15m candles of the collection tiers (#279), as each venue names them.
func TestCandleIntervals_EveryVenueGivesTheFineResolutions(t *testing.T) {
	for _, tc := range []struct {
		venue, path, file, symbol, minute, quarter string
	}{
		{Binance, "/api/v3/klines", "binance_klines.json", "BTCUSDT", "interval=1m", "interval=15m"},
		{OKX, "/api/v5/market/history-candles", "okx_candles.json", "BTC-USDT", "bar=1m", "bar=15m"},
		{Kraken, "/0/public/OHLC", "kraken_ohlc.json", "XXBTZUSD", "interval=1&", "interval=15&"},
		{Coinbase, "/api/v3/brokerage/market/products/BTC-USD/candles", "coinbase_candles.json", "BTC-USD", "granularity=ONE_MINUTE", "granularity=FIFTEEN_MINUTE"},
		{Nobitex, "/market/udf/history", "nobitex_udf.json", "btc-rls", "resolution=1&", "resolution=15&"},
		{Wallex, "/v1/udf/history", "wallex_udf.json", "BTCTMN", "resolution=1&", "resolution=15&"},
	} {
		t.Run(tc.venue, func(t *testing.T) {
			src, asked := venue(t, tc.venue, map[string]string{tc.path: tc.file})
			cs := src.(source.CandleSource)
			for _, interval := range []source.Interval{source.Interval1m, source.Interval15m} {
				assert.True(t, src.(interface{ Supports(source.Interval) bool }).Supports(interval), interval)
				_, err := cs.Candles(context.Background(), tc.symbol, interval, hour(15), hour(16))
				require.NoError(t, err)
			}
			require.Len(t, *asked, 2)
			assert.Contains(t, (*asked)[0]+"&", tc.minute)
			assert.Contains(t, (*asked)[1]+"&", tc.quarter)
		})
	}
}
