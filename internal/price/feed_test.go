package price

import (
	"context"
	"errors"
	"io"
	"log"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source/exchange"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/stream"
)

func TestProtocols_Binance(t *testing.T) {
	p := binance{}
	assert.Equal(t, "BTCUSDT", p.Native("BTC/USDT"))
	sub, err := p.Subscribe([]string{"BTCUSDT"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"method":"SUBSCRIBE","params":["btcusdt@bookTicker","btcusdt@trade"],"id":1}`, string(sub[0]))

	now := t0
	ticks, err := p.Parse([]byte(`{"u":400900217,"s":"BTCUSDT","b":"25.35190000","B":"31.21","a":"25.36520000","A":"40.66"}`), now)
	require.NoError(t, err)
	require.Len(t, ticks, 1)
	assert.Equal(t, Tick{Market: "BTCUSDT", Kind: KindBook, Bid: sd("25.3519"), Ask: sd("25.3652"), Received: now}, normalize(ticks[0]))

	ticks, err = p.Parse([]byte(`{"e":"trade","E":1700000000123,"s":"BTCUSDT","t":12345,"p":"0.001","q":"100","T":1700000000120,"m":true}`), now)
	require.NoError(t, err)
	require.Len(t, ticks, 1)
	assert.Equal(t, KindTrade, ticks[0].Kind)
	assert.Equal(t, "0.001", ticks[0].Price.String())
	assert.Equal(t, "100", ticks[0].Size.String())
	assert.Equal(t, time.UnixMilli(1700000000120).UTC(), ticks[0].At.UTC())

	ticks, err = p.Parse([]byte(`{"result":null,"id":1}`), now)
	require.NoError(t, err)
	assert.Empty(t, ticks)
	_, err = p.Parse([]byte(`{"code":2,"msg":"Invalid request"}`), now)
	assert.Error(t, err)
	_, err = p.Parse([]byte(`not json`), now)
	assert.Error(t, err)
}

func TestProtocols_Coinbase(t *testing.T) {
	p := coinbase{}
	assert.Equal(t, "BTC-USDT", p.Native("BTC/USDT"))
	sub, err := p.Subscribe([]string{"BTC-USDT"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"subscribe","product_ids":["BTC-USDT"],"channels":["ticker","heartbeat"]}`, string(sub[0]))

	ticks, err := p.Parse([]byte(`{"type":"ticker","sequence":1,"product_id":"BTC-USDT","price":"20153.56","best_bid":"20153.55","best_ask":"20153.57","side":"buy","time":"2022-10-19T23:28:22.061769Z","trade_id":370843401,"last_size":"0.00007232"}`), t0)
	require.NoError(t, err)
	require.Len(t, ticks, 2)
	at := time.Date(2022, 10, 19, 23, 28, 22, 61769000, time.UTC)
	assert.Equal(t, Tick{Market: "BTC-USDT", Kind: KindBook, Bid: sd("20153.55"), Ask: sd("20153.57"), At: at, Received: t0}, normalize(ticks[0]))
	assert.Equal(t, Tick{Market: "BTC-USDT", Kind: KindTrade, Price: sd("20153.56"), Size: sd("0.00007232"), At: at, Received: t0}, normalize(ticks[1]))

	ticks, err = p.Parse([]byte(`{"type":"heartbeat","product_id":"BTC-USDT"}`), t0)
	require.NoError(t, err)
	assert.Empty(t, ticks)
	_, err = p.Parse([]byte(`{"type":"error","message":"Failed to subscribe","reason":"BTC-XYZ is not a valid product"}`), t0)
	assert.Error(t, err)
}

func TestProtocols_Kraken(t *testing.T) {
	p := kraken{}
	assert.Equal(t, "BTC/USDT", p.Native("BTC/USDT"))
	sub, err := p.Subscribe([]string{"BTC/USDT"})
	require.NoError(t, err)
	require.Len(t, sub, 2)
	assert.JSONEq(t, `{"method":"subscribe","params":{"channel":"ticker","symbol":["BTC/USDT"],"event_trigger":"bbo"}}`, string(sub[0]))
	assert.JSONEq(t, `{"method":"subscribe","params":{"channel":"trade","symbol":["BTC/USDT"],"snapshot":false}}`, string(sub[1]))

	ticks, err := p.Parse([]byte(`{"channel":"ticker","type":"update","data":[{"symbol":"XBT/USDT","bid":64000.1,"bid_qty":0.5,"ask":64000.2,"ask_qty":1.2,"last":64000.1}]}`), t0)
	require.NoError(t, err)
	require.Len(t, ticks, 1)
	assert.Equal(t, Tick{Market: "BTC/USDT", Kind: KindBook, Bid: sd("64000.1"), Ask: sd("64000.2"), Received: t0}, normalize(ticks[0]))

	ticks, err = p.Parse([]byte(`{"channel":"trade","type":"update","data":[{"symbol":"BTC/USDT","side":"buy","price":64000.15,"qty":0.0123,"ord_type":"market","trade_id":1,"timestamp":"2023-09-25T07:48:36.925533Z"}]}`), t0)
	require.NoError(t, err)
	require.Len(t, ticks, 1)
	assert.Equal(t, "64000.15", ticks[0].Price.String())
	assert.Equal(t, "0.0123", ticks[0].Size.String())
	assert.Equal(t, time.Date(2023, 9, 25, 7, 48, 36, 925533000, time.UTC), ticks[0].At)

	for _, m := range []string{`{"channel":"heartbeat"}`, `{"method":"subscribe","success":false,"error":"Currency pair not supported"}`, `{"channel":"status","data":[{"system":"online"}]}`} {
		ticks, err = p.Parse([]byte(m), t0)
		require.NoError(t, err)
		assert.Empty(t, ticks, m)
	}
}

// normalize compares decimals by value.
func normalize(t Tick) Tick {
	for _, v := range []*shared.Decimal{&t.Bid, &t.Ask, &t.Price, &t.Size} {
		if !v.IsZero() {
			*v = sd(v.String())
		}
	}
	return t
}

// The lists of the configuration agree with the protocols and with the
// venue sources.
func TestProviders_AgreeWithTheConfiguration(t *testing.T) {
	assert.ElementsMatch(t, config.PriceStreamProviders, slices.Collect(maps.Keys(Protocols())))
	assert.ElementsMatch(t, config.PriceRESTProviders, exchange.Names)
}

// fakeConn is a connection that serves the messages it is given, and fails
// when told.
type fakeConn struct {
	messages chan []byte
	closed   chan struct{}
	once     sync.Once
	mu       sync.Mutex
	written  [][]byte
}

func newFakeConn() *fakeConn {
	return &fakeConn{messages: make(chan []byte, 16), closed: make(chan struct{})}
}

func (c *fakeConn) ReadMessage() ([]byte, error) {
	select {
	case m, ok := <-c.messages:
		if !ok {
			return nil, io.EOF
		}
		return m, nil
	case <-c.closed:
		return nil, errors.New("closed")
	}
}

func (c *fakeConn) WriteMessage(m []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.written = append(c.written, m)
	return nil
}

func (c *fakeConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

type fakeTickers struct {
	mu      sync.Mutex
	tickers []source.Ticker
	calls   int
}

func (f *fakeTickers) Tickers(context.Context) ([]source.Ticker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.tickers, nil
}

func (f *fakeTickers) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type ticks struct {
	mu   sync.Mutex
	list []Tick
}

func (s *ticks) add(t Tick) {
	s.mu.Lock()
	s.list = append(s.list, t)
	s.mu.Unlock()
}

func (s *ticks) all() []Tick {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.list)
}

func ptr(s string) *shared.Decimal { v := sd(s); return &v }

// The feed passes the stream's ticks with the market's name, reconnects
// when the connection drops, and reads the REST tickers while the stream is
// down.
func TestFeed_StreamReconnectAndRESTFallback(t *testing.T) {
	conns := make(chan *fakeConn, 4)
	dialed := 0
	var mu sync.Mutex
	dial := stream.Dialer(func(ctx context.Context, url string) (stream.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		dialed++
		assert.Equal(t, "wss://stream.binance.com:9443/ws", url)
		select {
		case c := <-conns:
			return c, nil
		default:
			return nil, errors.New("connection refused")
		}
	})
	rest := &fakeTickers{tickers: []source.Ticker{
		{Base: "BTC", Quote: "USDT", Bid: ptr("99"), Ask: ptr("100"), ObservedAt: t0},
		{Base: "ETH", Quote: "USDT", Bid: ptr("1"), Ask: ptr("2")},     // not followed
		{Base: "BTC", Quote: "USDT", Last: sd("99.5"), ObservedAt: t0}, // no book
	}}
	f, err := NewFeed(FeedOptions{
		Name: "binance", Markets: []string{"BTC/USDT"}, Protocol: binance{}, Dial: dial, REST: rest,
		PollEvery: 20 * time.Millisecond, FallbackAfter: 50 * time.Millisecond,
		BackoffMin: 10 * time.Millisecond, BackoffMax: 20 * time.Millisecond,
		Logger: log.New(io.Discard, "", 0),
	})
	require.NoError(t, err)

	first := newFakeConn()
	conns <- first
	got := &ticks{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.Run(ctx, got.add); close(done) }()

	first.messages <- []byte(`{"u":1,"s":"BTCUSDT","b":"100","B":"1","a":"101","A":"1"}`)
	first.messages <- []byte(`{"u":2,"s":"ETHUSDT","b":"1","B":"1","a":"2","A":"1"}`) // not followed
	// The REST API may answer once before the stream opens.
	fromStream := func() []Tick {
		var out []Tick
		for _, tk := range got.all() {
			if tk.Bid.String() == "100" {
				out = append(out, tk)
			}
		}
		return out
	}
	require.Eventually(t, func() bool { return len(fromStream()) == 1 }, time.Second, 5*time.Millisecond)
	tk := fromStream()[0]
	assert.Equal(t, "binance", tk.Provider)
	assert.Equal(t, "BTC/USDT", tk.Market)
	assert.True(t, f.Health().Connected)
	first.mu.Lock()
	assert.Len(t, first.written, 1, "subscribed")
	first.mu.Unlock()

	// The connection drops, and no new one opens: REST takes over.
	close(first.messages)
	require.Eventually(t, func() bool { return rest.Calls() > 0 && !f.Health().Connected }, time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		for _, tk := range got.all() {
			if tk.Kind == KindBook && tk.Bid.String() == "99" {
				return true
			}
		}
		return false
	}, time.Second, 5*time.Millisecond)
	h := f.Health()
	assert.True(t, h.Fallback)
	assert.GreaterOrEqual(t, h.Reconnect, 1)
	for _, tk := range got.all() {
		assert.Equal(t, "BTC/USDT", tk.Market, "only the followed market, and only with its book")
	}

	// The stream comes back: REST stops.
	second := newFakeConn()
	conns <- second
	require.Eventually(t, func() bool { return f.Health().Connected }, 2*time.Second, 5*time.Millisecond)
	stopFeeding := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopFeeding:
				return
			case second.messages <- []byte(`{"u":3,"s":"BTCUSDT","b":"100","B":"1","a":"101","A":"1"}`):
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()
	require.Eventually(t, func() bool { return !f.Health().Fallback }, time.Second, 5*time.Millisecond)
	calls := rest.Calls()
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, calls, rest.Calls(), "no REST read while the stream sends")
	close(stopFeeding)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the feed did not stop with its context")
	}
}

// A provider with no stream is read through REST only.
func TestFeed_RESTOnly(t *testing.T) {
	rest := &fakeTickers{tickers: []source.Ticker{{Base: "USDT", Quote: "IRT", Bid: ptr("90000"), Ask: ptr("90100"), ObservedAt: t0}}}
	f, err := NewFeed(FeedOptions{Name: "nobitex", Markets: []string{"USDT/IRT"}, REST: rest, PollEvery: 10 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	require.NoError(t, err)
	got := &ticks{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.Run(ctx, got.add)
	require.Eventually(t, func() bool { return len(got.all()) >= 2 }, time.Second, 5*time.Millisecond)
	tk := got.all()[0]
	assert.Equal(t, Tick{Provider: "nobitex", Market: "USDT/IRT", Kind: KindBook, Bid: sd("90000"), Ask: sd("90100"), At: t0, Received: tk.Received}, tk)
	assert.True(t, f.Health().Fallback)

	_, err = NewFeed(FeedOptions{Name: "none"})
	assert.Error(t, err, "neither a stream nor REST")
	_, err = NewFeed(FeedOptions{Name: "binance", Protocol: binance{}})
	assert.Error(t, err, "a stream without a dialer")
}

// pairTickers serves only the pairs asked for, and records them.
type pairTickers struct {
	fakeTickers
	mu    sync.Mutex
	asked [][2]string
}

func (p *pairTickers) PairTickers(_ context.Context, pairs [][2]string) ([]source.Ticker, error) {
	p.mu.Lock()
	p.asked = pairs
	p.mu.Unlock()
	return p.tickers, nil
}

// A provider that reads a request per pair is asked for the feed's markets
// only (#443).
func TestFeed_ReadsOnlyItsPairsWhenTheProviderCan(t *testing.T) {
	rest := &pairTickers{fakeTickers: fakeTickers{tickers: []source.Ticker{{Base: "USDT", Quote: "IRT", Bid: ptr("90000"), Ask: ptr("90100"), ObservedAt: t0}}}}
	f, err := NewFeed(FeedOptions{Name: "tabdeal", Markets: []string{"USDT/IRT"}, REST: rest, PollEvery: 10 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	require.NoError(t, err)
	got := &ticks{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.Run(ctx, got.add)
	require.Eventually(t, func() bool { return len(got.all()) >= 1 }, time.Second, 5*time.Millisecond)
	rest.mu.Lock()
	defer rest.mu.Unlock()
	assert.Equal(t, [][2]string{{"USDT", "IRT"}}, rest.asked)
	assert.Zero(t, rest.Calls(), "never every ticker")
}

type recordingSink struct {
	mu     sync.Mutex
	prices [][]Price
}

func (s *recordingSink) Prices(_ context.Context, p []Price) {
	s.mu.Lock()
	s.prices = append(s.prices, p)
	s.mu.Unlock()
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prices)
}

// The service aggregates the feeds' ticks every interval, and passes the
// prices to its sinks.
func TestService_AggregatesTheFeeds(t *testing.T) {
	agg := newTestAggregator(t, Options{Markets: []string{"USDT/IRT"}, MinProviders: 2, Stale: time.Minute})
	var feeds []*Feed
	for _, name := range []string{"nobitex", "wallex"} {
		rest := &fakeTickers{tickers: []source.Ticker{{Base: "USDT", Quote: "IRT", Bid: ptr("90000"), Ask: ptr("90200")}}}
		f, err := NewFeed(FeedOptions{Name: name, Markets: []string{"USDT/IRT"}, REST: rest, PollEvery: 10 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
		require.NoError(t, err)
		feeds = append(feeds, f)
	}
	sink := &recordingSink{}
	s := NewService(agg, feeds, 10*time.Millisecond, sink)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	require.Eventually(t, func() bool {
		p := s.Prices()
		return len(p) == 1 && p[0].Live()
	}, 2*time.Second, 5*time.Millisecond)
	p := s.Prices()[0]
	assert.Equal(t, "90100", p.Mid.String())
	assert.Equal(t, []string{"nobitex", "wallex"}, p.Sources)
	assert.Positive(t, sink.count())
	assert.Len(t, s.Health(), 2)
	cancel()
	<-done
}

// The configuration builds a feed per enabled provider, with the markets
// normalized.
func TestFromConfig(t *testing.T) {
	c := &config.Configuration{}
	a := &c.PriceAggregator
	a.Enabled = true
	a.VWAPWindows = []int{1, 60}
	a.OutlierThreshold = 3
	a.MinProviders = 2
	a.StaleThreshold = 5000
	a.Providers = []config.PriceProviderConfig{
		{Name: "binance", Enabled: true, Type: "websocket", Symbols: []string{"BTC/USDT", "eth-usdt"}},
		{Name: "kraken", Enabled: true, Type: "websocket", Symbols: []string{"XBT/USDT"}},
		{Name: "nobitex", Enabled: true, Type: "rest", Symbols: []string{"USDT/IRT"}, StaleThresholdMs: 30_000},
		{Name: "coinbase", Enabled: false, Type: "websocket", Symbols: []string{"BTC-USD"}},
	}
	agg, feeds, err := FromConfig(c, log.New(io.Discard, "", 0))
	require.NoError(t, err)
	require.Len(t, feeds, 3)
	assert.Equal(t, []string{"binance", "kraken", "nobitex"}, []string{feeds[0].Name(), feeds[1].Name(), feeds[2].Name()})
	assert.ElementsMatch(t, []string{"BTC/USDT", "ETH/USDT", "USDT/IRT"}, slices.Collect(maps.Keys(agg.markets)))
	assert.NotNil(t, feeds[0].opts.Protocol)
	assert.NotNil(t, feeds[0].opts.REST, "the REST fallback")
	assert.Nil(t, feeds[2].opts.Protocol)
	assert.Equal(t, 30*time.Second, agg.stale("nobitex"))
	assert.Equal(t, 5*time.Second, agg.stale("binance"))
	assert.Equal(t, []string{"1s", "60s"}, agg.labels)

	a.Providers[0].Symbols = []string{"btcusdt"}
	_, _, err = FromConfig(c, log.New(io.Discard, "", 0))
	assert.Error(t, err)
}
