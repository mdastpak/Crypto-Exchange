package exchange

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source/sourcetest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fixtures in testdata follow the documented answers of each API; the
// tests cannot reach the venues.

var readAt = time.Date(2026, 11, 8, 19, 0, 0, 0, time.UTC)

// venue serves the fixture of each path and returns the named source on it
// and the paths it was asked for.
func venue(t *testing.T, name string, files map[string]string) (source.VenueSource, *[]string) {
	t.Helper()
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RequestURI())
		file, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		body, err := os.ReadFile("testdata/" + file)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	client, err := source.NewClient(source.ClientOptions{Source: name, BaseURL: srv.URL, Timeout: 5 * time.Second})
	require.NoError(t, err)
	src, err := New(name, client)
	require.NoError(t, err)
	setNow(src, func() time.Time { return readAt })
	return src, &asked
}

func setNow(src source.VenueSource, now func() time.Time) {
	switch v := src.(type) {
	case *binance:
		v.now = now
	case *okx:
		v.now = now
	case *kraken:
		v.now = now
	case *coinbase:
		v.now = now
	case *nobitex:
		v.now = now
	case *wallex:
		v.now = now
	case *ramzinex:
		v.now = now
	case *tabdeal:
		v.now = now
	}
}

func bySymbol(t *testing.T, src source.VenueSource) map[string]source.Ticker {
	t.Helper()
	tickers, err := src.Tickers(context.Background())
	require.NoError(t, err)
	out := map[string]source.Ticker{}
	for _, tk := range tickers {
		out[tk.VenueSymbol] = tk
	}
	return out
}

func keys(m map[string]source.Ticker) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestBinance(t *testing.T) {
	src, asked := venue(t, Binance, map[string]string{
		"/api/v3/exchangeInfo": "binance_exchangeinfo.json", "/api/v3/ticker/24hr": "binance_ticker24hr.json"})
	sourcetest.CheckVenue(t, src)
	got := bySymbol(t, src)
	assert.Equal(t, []string{"BTCUSDT", "ETHBTC", "SHIBUSDT"}, keys(got), "only trading pairs with a price")
	btc := got["BTCUSDT"]
	assert.Equal(t, "BTC", btc.Base)
	assert.Equal(t, "USDT", btc.Quote)
	assert.Equal(t, "68012.1", btc.Last.String())
	assert.Equal(t, "1035123456.12345678", btc.VolumeQuote.String(), "every digit")
	assert.Equal(t, time.UnixMilli(1762628399999).UTC(), btc.ObservedAt)
	assert.Nil(t, got["SHIBUSDT"].Bid, "a crossed book is left out")
	assert.Equal(t, "0.00001234", got["SHIBUSDT"].Last.String())

	// The pairs are read once an hour.
	exchangeInfo := 0
	for _, p := range *asked {
		if p == "/api/v3/exchangeInfo?permissions=SPOT" {
			exchangeInfo++
		}
	}
	assert.Equal(t, 1, exchangeInfo)
}

func TestOKX(t *testing.T) {
	src, asked := venue(t, OKX, map[string]string{"/api/v5/market/tickers": "okx_tickers.json"})
	sourcetest.CheckVenue(t, src)
	got := bySymbol(t, src)
	assert.Equal(t, []string{"BTC-USDT", "ETH-USDC"}, keys(got))
	btc := got["BTC-USDT"]
	assert.Equal(t, "BTC", btc.Base)
	assert.Equal(t, "8842.12", btc.VolumeBase.String())
	assert.Equal(t, "601234567.891", btc.VolumeQuote.String())
	assert.Equal(t, "67490.1", btc.Open.String())
	assert.Nil(t, got["ETH-USDC"].Bid, "an empty bid is missing")
	assert.Contains(t, (*asked)[0], "instType=SPOT")
}

func TestKraken(t *testing.T) {
	src, _ := venue(t, Kraken, map[string]string{
		"/0/public/AssetPairs": "kraken_assetpairs.json", "/0/public/Ticker": "kraken_ticker.json"})
	sourcetest.CheckVenue(t, src)
	got := bySymbol(t, src)
	assert.Equal(t, []string{"ETHEUR", "XDGUSD", "XXBTZUSD"}, keys(got), "not a delisted pair")
	btc := got["XXBTZUSD"]
	assert.Equal(t, "BTC", btc.Base, "XBT is BTC")
	assert.Equal(t, "USD", btc.Quote)
	assert.Equal(t, "2412.73601799", btc.VolumeBase.String(), "the last 24 hours")
	assert.Equal(t, "68450", btc.High.String())
	assert.Nil(t, btc.Open, "the open of the day is not that of 24 hours")
	assert.Equal(t, readAt, btc.ObservedAt)
	assert.Equal(t, "DOGE", got["XDGUSD"].Base)
	assert.Nil(t, got["ETHEUR"].Bid, "a crossed book is left out")
}

func TestCoinbase(t *testing.T) {
	src, asked := venue(t, Coinbase, map[string]string{"/api/v3/brokerage/market/products": "coinbase_products.json"})
	sourcetest.CheckVenue(t, src)
	got := bySymbol(t, src)
	assert.Equal(t, []string{"BTC-USD", "ETH-USDC"}, keys(got), "not a disabled product")
	assert.Equal(t, "68001.23", got["BTC-USD"].Last.String())
	assert.Equal(t, "12001.123456", got["BTC-USD"].VolumeBase.String())
	assert.Contains(t, (*asked)[0], "product_type=SPOT")
}

func TestNobitex_RialsAreTomans(t *testing.T) {
	src, _ := venue(t, Nobitex, map[string]string{"/market/stats": "nobitex_stats.json"})
	sourcetest.CheckVenue(t, src)
	got := bySymbol(t, src)
	assert.Equal(t, []string{"btc-rls", "btc-usdt", "usdt-rls"}, keys(got), "not a closed market")
	btc := got["btc-rls"]
	assert.Equal(t, "BTC", btc.Base)
	assert.Equal(t, Toman, btc.Quote)
	assert.Equal(t, "6799500000", btc.Last.String())
	assert.Equal(t, "6799000000", btc.Bid.String())
	assert.Equal(t, "85000000000", btc.VolumeQuote.String())
	assert.Equal(t, "12.5", btc.VolumeBase.String(), "the base volume is not in rials")
	assert.Equal(t, "100000.5", got["usdt-rls"].Last.String(), "exact in decimals")
	assert.Equal(t, "USDT", got["btc-usdt"].Quote)
	assert.Equal(t, "68015.5", got["btc-usdt"].Last.String())
}

func TestWallex(t *testing.T) {
	src, _ := venue(t, Wallex, map[string]string{"/v1/markets": "wallex_markets.json"})
	sourcetest.CheckVenue(t, src)
	got := bySymbol(t, src)
	assert.Equal(t, []string{"BTCTMN", "ETHUSDT", "USDTTMN"}, keys(got))
	assert.Equal(t, Toman, got["BTCTMN"].Quote, "TMN is IRT")
	assert.Equal(t, "6800000000", got["BTCTMN"].Last.String())
	assert.Equal(t, "99990", got["USDTTMN"].Bid.String(), "numbers too")
	assert.Nil(t, got["ETHUSDT"].Bid, "a dash is missing")
}

func TestRamzinex_RialsAreTomans(t *testing.T) {
	src, asked := venue(t, Ramzinex, map[string]string{"/exchange/api/v1.0/exchange/pairs": "ramzinex_pairs.json"})
	sourcetest.CheckVenue(t, src)
	got := bySymbol(t, src)
	assert.Equal(t, []string{"11", "2", "272"}, keys(got), "not a pair without a price")
	btc := got["2"]
	assert.Equal(t, "BTC", btc.Base)
	assert.Equal(t, Toman, btc.Quote)
	assert.Equal(t, "6800000000", btc.Last.String())
	assert.Equal(t, "6799000000", btc.Bid.String(), "buy is the bid")
	assert.Equal(t, "6801000000", btc.Ask.String(), "sell is the ask")
	assert.Equal(t, "85000000000", btc.VolumeQuote.String())
	assert.Equal(t, "12.5", btc.VolumeBase.String(), "the base volume is not in rials")
	usdt := got["11"]
	assert.Equal(t, "100001", usdt.Last.String(), "the middle of the book without a close, in tomans")
	assert.Equal(t, "100000.5", usdt.Bid.String(), "strings too, exact")
	eth := got["272"]
	assert.Equal(t, "USDT", eth.Quote, "a symbol as a string")
	assert.Nil(t, eth.Bid, "crossed sides are dropped")
	assert.Equal(t, "/exchange/api/v1.0/exchange/pairs", (*asked)[0])
}

func TestTabdeal_ReadsTheDepths(t *testing.T) {
	files := map[string]string{"/api/v1/exchangeInfo": "tabdeal_exchangeinfo.json"}
	var (
		mu     sync.Mutex
		depths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, ok := files[r.URL.Path]
		if r.URL.Path == "/api/v1/depth" {
			mu.Lock()
			depths = append(depths, r.URL.Query().Get("symbol"))
			mu.Unlock()
			file, ok = "tabdeal_depth_"+strings.ToLower(r.URL.Query().Get("symbol"))+".json", true
		}
		body, err := os.ReadFile("testdata/" + file)
		if !ok || err != nil {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	client, err := source.NewClient(source.ClientOptions{Source: Tabdeal, BaseURL: srv.URL, Timeout: 5 * time.Second})
	require.NoError(t, err)
	src, err := New(Tabdeal, client)
	require.NoError(t, err)
	setNow(src, func() time.Time { return readAt })
	sourcetest.CheckVenue(t, src)

	got := bySymbol(t, src)
	assert.Equal(t, []string{"BTCIRT", "USDTIRT"}, keys(got), "not a pair out of trading, nor one without a bid")
	btc := got["BTCIRT"]
	assert.Equal(t, Toman, btc.Quote)
	assert.Equal(t, "6799000000", btc.Bid.String())
	assert.Equal(t, "6801000000", btc.Ask.String())
	assert.Equal(t, "6800000000", btc.Last.String(), "the middle of the book")
	assert.Equal(t, readAt, btc.ObservedAt)

	mu.Lock()
	depths = nil
	mu.Unlock()
	pairs, err := src.(interface {
		PairTickers(context.Context, [][2]string) ([]source.Ticker, error)
	}).PairTickers(context.Background(), [][2]string{{"USDT", "IRT"}})
	require.NoError(t, err)
	require.Len(t, pairs, 1)
	assert.Equal(t, "99990", pairs[0].Bid.String())
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"USDTIRT"}, depths, "only the pairs asked for")
}

func TestVenueErrors(t *testing.T) {
	_, err := New("nowhere", nil)
	assert.Error(t, err)
	for _, name := range Names {
		s, ok := Settings(name)
		assert.True(t, ok, name)
		assert.Regexp(t, `^https://`, s.DefaultBaseURL)
		assert.Empty(t, s.KeyHeader, "public endpoints")
	}

	src, _ := venue(t, OKX, map[string]string{})
	_, err = src.Tickers(context.Background())
	assert.ErrorIs(t, err, source.ErrNotFound)
}
