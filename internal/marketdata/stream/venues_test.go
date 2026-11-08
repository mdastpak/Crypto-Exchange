package stream

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
)

var now = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

var pairs = []marketdata.StreamPair{
	{PairID: 1, VenueSymbol: "BTCUSDT", BaseSymbol: "BTC", QuoteSymbol: "USDT"},
	{PairID: 2, VenueSymbol: "BTC-USD", BaseSymbol: "BTC", QuoteSymbol: "USD"},
}

func subscription(t *testing.T, p Protocol) []map[string]any {
	t.Helper()
	messages, err := p.Subscribe(pairs)
	require.NoError(t, err)
	var out []map[string]any
	for _, m := range messages {
		var v map[string]any
		require.NoError(t, json.Unmarshal(m, &v))
		out = append(out, v)
	}
	return out
}

func TestProtocols_EveryGlobalVenue(t *testing.T) {
	assert.ElementsMatch(t, []string{"binance", "okx", "kraken", "coinbase"}, func() []string {
		var names []string
		for name, p := range Protocols() {
			assert.Equal(t, name, p.Venue())
			assert.Regexp(t, `^wss://`, p.URL())
			names = append(names, name)
		}
		return names
	}())
}

func TestBinance(t *testing.T) {
	p := binance{}
	assert.Equal(t, []any{"btcusdt@ticker", "btc-usd@ticker"}, subscription(t, p)[0]["params"])
	assert.Equal(t, "BTCUSDT", p.Key(pairs[0]))
	got, err := p.Parse([]byte(`{"e":"24hrTicker","E":1762603200000,"s":"BTCUSDT","p":"100","o":"67900.1","h":"68500","l":"67000",
		"c":"68000.123456789012345678","b":"67999.9","a":"68000.2","v":"1234.5","q":"83950000.25"}`), now)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "BTCUSDT", got[0].VenueSymbol)
	assert.Equal(t, "68000.123456789012345678", got[0].Last.String(), "every digit")
	assert.Equal(t, "67999.9", got[0].Bid.String())
	assert.Equal(t, "67900.1", got[0].Open.String())
	assert.Equal(t, "83950000.25", got[0].VolumeQuote.String())
	assert.Equal(t, time.UnixMilli(1762603200000).UTC(), got[0].ObservedAt.UTC())

	none, err := p.Parse([]byte(`{"result":null,"id":1}`), now)
	require.NoError(t, err)
	assert.Empty(t, none, "the answer to the subscription")
	_, err = p.Parse([]byte(`not json`), now)
	assert.Error(t, err)
}

func TestOKX(t *testing.T) {
	p := okx{}
	args := subscription(t, p)[0]["args"].([]any)
	assert.Equal(t, map[string]any{"channel": "tickers", "instId": "BTCUSDT"}, args[0])
	assert.Equal(t, "ping", string(p.Ping()))
	got, err := p.Parse([]byte(`{"arg":{"channel":"tickers","instId":"BTC-USDT"},"data":[{"instType":"SPOT","instId":"BTC-USDT",
		"last":"68000.5","bidPx":"68000.4","askPx":"68000.6","open24h":"67000","high24h":"68500","low24h":"66900",
		"vol24h":"5000.5","volCcy24h":"340000000","ts":"1762603200000"}]}`), now)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "BTC-USDT", got[0].VenueSymbol)
	assert.Equal(t, "5000.5", got[0].VolumeBase.String())
	assert.Equal(t, "340000000", got[0].VolumeQuote.String())

	for _, quiet := range []string{"pong", `{"event":"subscribe","arg":{"channel":"tickers","instId":"BTC-USDT"}}`} {
		none, err := p.Parse([]byte(quiet), now)
		require.NoError(t, err, quiet)
		assert.Empty(t, none)
	}
	_, err = p.Parse([]byte(`{"event":"error","code":"60012","msg":"Invalid request"}`), now)
	assert.ErrorContains(t, err, "Invalid request")
}

func TestKraken(t *testing.T) {
	p := kraken{}
	params := subscription(t, p)[0]["params"].(map[string]any)
	assert.Equal(t, "ticker", params["channel"])
	assert.Equal(t, []any{"BTC/USDT", "BTC/USD"}, params["symbol"], "BTC, as the pairs' symbols are stored")
	got, err := p.Parse([]byte(`{"channel":"ticker","type":"update","data":[{"symbol":"BTC/USD","bid":67999.9,"bid_qty":1.2,
		"ask":68000.1,"ask_qty":0.5,"last":68000.0,"volume":1500.25,"vwap":67800.0,"low":66900.0,"high":68500.0,
		"change":1000.0,"change_pct":1.49}]}`), now)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "BTC/USD", got[0].VenueSymbol)
	assert.Equal(t, "68000", got[0].Last.String())
	assert.Equal(t, "67000", got[0].Open.Decimal().String(), "the last price less the change")
	assert.Equal(t, "1500.25", got[0].VolumeBase.String())
	assert.Equal(t, now, got[0].ObservedAt, "observed when received")

	for _, quiet := range []string{`{"channel":"heartbeat"}`, `{"method":"subscribe","success":false,"error":"Currency pair not supported XYZ/USD"}`} {
		none, err := p.Parse([]byte(quiet), now)
		require.NoError(t, err, quiet)
		assert.Empty(t, none)
	}
}

func TestCoinbase(t *testing.T) {
	p := coinbase{}
	subs := subscription(t, p)
	assert.Equal(t, "ticker", subs[0]["channel"])
	assert.Equal(t, []any{"BTCUSDT", "BTC-USD"}, subs[0]["product_ids"])
	assert.Equal(t, "heartbeats", subs[1]["channel"], "keeps quiet pairs connected")
	got, err := p.Parse([]byte(`{"channel":"ticker","client_id":"","timestamp":"2026-11-08T11:59:59.123456789Z","sequence_num":1,
		"events":[{"type":"update","tickers":[{"type":"ticker","product_id":"BTC-USD","price":"68000.12","volume_24_h":"9000.5",
		"low_24_h":"66900","high_24_h":"68500","best_bid":"68000.11","best_ask":"68000.13"}]}]}`), now)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "BTC-USD", got[0].VenueSymbol)
	assert.Equal(t, "68000.13", got[0].Ask.String())
	assert.Equal(t, time.Date(2026, 11, 8, 11, 59, 59, 123456789, time.UTC), got[0].ObservedAt)

	none, err := p.Parse([]byte(`{"channel":"heartbeats","timestamp":"2026-11-08T11:59:59Z","events":[]}`), now)
	require.NoError(t, err)
	assert.Empty(t, none)
	_, err = p.Parse([]byte(`{"type":"error","message":"failure to subscribe"}`), now)
	assert.ErrorContains(t, err, "failure to subscribe")
}

func TestNewDialer_RefusesAnInvalidProxy(t *testing.T) {
	_, err := NewDialer("ftp://proxy.example:21", 0)
	assert.Error(t, err)
	_, err = NewDialer("socks5://user:secret@gw.example:1080", 0)
	assert.NoError(t, err)
}
