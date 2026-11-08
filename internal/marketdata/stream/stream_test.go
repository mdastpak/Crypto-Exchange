package stream

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/jobs"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
)

// fakeVenue is a WebSocket server that speaks Binance's stream: it records
// the subscriptions of each connection, and plays script on it.
type fakeVenue struct {
	server        *httptest.Server
	connections   atomic.Int32
	mu            sync.Mutex
	subscriptions [][]string
	script        func(n int, conn *websocket.Conn)
}

func newFakeVenue(t *testing.T, script func(n int, conn *websocket.Conn)) *fakeVenue {
	t.Helper()
	f := &fakeVenue{script: script}
	upgrader := websocket.Upgrader{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		n := int(f.connections.Add(1))
		var sub struct {
			Params []string `json:"params"`
		}
		if err := conn.ReadJSON(&sub); err != nil {
			return
		}
		f.mu.Lock()
		f.subscriptions = append(f.subscriptions, sub.Params)
		f.mu.Unlock()
		f.script(n, conn)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeVenue) subscribed() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.subscriptions...)
}

// tickerMessage is a 24-hour ticker of Binance's stream.
func tickerMessage(symbol, last string, at time.Time) []byte {
	m, _ := json.Marshal(map[string]any{"e": "24hrTicker", "E": at.UnixMilli(), "s": symbol, "c": last, "b": last, "a": last,
		"o": "60000", "h": "70000", "l": "59000", "v": "100.5", "q": "6800000"})
	return m
}

// at is a protocol on a test server.
type at struct {
	Protocol
	url string
}

func (a at) URL() string { return a.url }

type fixture struct {
	db    *sql.DB
	store *marketdata.Store
	ids   map[string]int64
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	conn := pgtest.Migrated(t, "testdb_marketdata_stream")
	store := marketdata.NewStore(conn)
	ctx := context.Background()
	btc, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", marketdata.AssetDetails{Symbol: "BTC", Name: "Bitcoin", Kind: marketdata.KindCoin})
	require.NoError(t, err)
	eth, _, err := store.CreateAsset(ctx, "ethereum", "coingecko", "ethereum", marketdata.AssetDetails{Symbol: "ETH", Name: "Ethereum", Kind: marketdata.KindCoin})
	require.NoError(t, err)
	require.NoError(t, store.UpsertVenue(ctx, marketdata.Venue{ID: "binance", Name: "Binance", Kind: marketdata.VenueCEX, Region: marketdata.RegionGlobal, Enabled: true}))
	ids, err := store.UpsertVenuePairs(ctx, "binance", []marketdata.VenuePair{
		{VenueSymbol: "BTCUSDT", BaseSymbol: "BTC", QuoteSymbol: "USDT", BaseAssetID: &btc},
		{VenueSymbol: "ETHUSDT", BaseSymbol: "ETH", QuoteSymbol: "USDT", BaseAssetID: &eth},
	})
	require.NoError(t, err)
	_, err = store.SetTiers(ctx, map[int64]string{ids["BTCUSDT"]: marketdata.TierHot})
	require.NoError(t, err)
	return fixture{db: conn, store: store, ids: ids}
}

func (f fixture) last(t *testing.T, symbol string) string {
	t.Helper()
	ticker, err := f.store.TickerOf(context.Background(), f.ids[symbol])
	if err != nil {
		return ""
	}
	return ticker.LastPrice.String()
}

func quiet() *log.Logger { return log.New(io.Discard, "", 0) }

func newJob(t *testing.T, f fixture, venue *fakeVenue, opts Options) *Job {
	t.Helper()
	dial, err := NewDialer("", 5*time.Second)
	require.NoError(t, err)
	opts.Logger = quiet()
	if opts.BackoffMin == 0 {
		opts.BackoffMin, opts.BackoffMax = 10*time.Millisecond, 20*time.Millisecond
	}
	url := "ws" + strings.TrimPrefix(venue.server.URL, "http")
	return New(at{Protocol: binance{}, url: url}, dial, f.store, opts)
}

func TestStream_WritesTheHotPairsThrottled(t *testing.T) {
	f := newFixture(t)
	start := time.Now()
	venue := newFakeVenue(t, func(_ int, conn *websocket.Conn) {
		// Many updates within one flush: the pair is written once, with
		// the latest; a pair it does not follow is left alone.
		for i := 1; i <= 50; i++ {
			_ = conn.WriteMessage(websocket.TextMessage, tickerMessage("BTCUSDT", fmt.Sprintf("68%03d.5", i), start.Add(time.Duration(i)*time.Millisecond)))
		}
		_ = conn.WriteMessage(websocket.TextMessage, tickerMessage("ETHUSDT", "3000", start))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"result":null,"id":1}`))
		time.Sleep(2 * time.Second)
	})
	job := newJob(t, f, venue, Options{RunFor: 700 * time.Millisecond, Flush: 300 * time.Millisecond})
	assert.Equal(t, "stream:binance", job.Name())
	assert.Equal(t, []string{"binance"}, job.Sources())

	run := jobs.NewRun(marketdata.ModeLive, nil)
	require.NoError(t, job.Run(context.Background(), run), "the run ends after RunFor")
	assert.Equal(t, [][]string{{"btcusdt@ticker"}}, venue.subscribed(), "only the hot pairs")
	assert.Equal(t, 50, run.Read())
	assert.Equal(t, 1, run.Written(), "one write of the pair, not fifty")
	assert.Equal(t, "68050.5", f.last(t, "BTCUSDT"))
	assert.Empty(t, f.last(t, "ETHUSDT"))
	ticker, err := f.store.TickerOf(context.Background(), f.ids["BTCUSDT"])
	require.NoError(t, err)
	assert.Equal(t, "100.5", ticker.VolumeBase.String())
}

func TestStream_DryRunWritesNothing(t *testing.T) {
	f := newFixture(t)
	venue := newFakeVenue(t, func(_ int, conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.TextMessage, tickerMessage("BTCUSDT", "68000", time.Now()))
		time.Sleep(time.Second)
	})
	run := jobs.NewRun(marketdata.ModeDryRun, nil)
	require.NoError(t, newJob(t, f, venue, Options{RunFor: 400 * time.Millisecond, Flush: 100 * time.Millisecond}).Run(context.Background(), run))
	assert.Equal(t, 1, run.Written(), "what it would write")
	assert.Empty(t, f.last(t, "BTCUSDT"))
}

func TestStream_ReconnectsAndResubscribes(t *testing.T) {
	f := newFixture(t)
	start := time.Now()
	venue := newFakeVenue(t, func(n int, conn *websocket.Conn) {
		if n == 1 {
			// The venue drops the first connection.
			_ = conn.WriteMessage(websocket.TextMessage, tickerMessage("BTCUSDT", "68000", start))
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, tickerMessage("BTCUSDT", "68100", start.Add(time.Second)))
		time.Sleep(2 * time.Second)
	})
	run := jobs.NewRun(marketdata.ModeLive, nil)
	require.NoError(t, newJob(t, f, venue, Options{RunFor: time.Second, Flush: 100 * time.Millisecond}).Run(context.Background(), run))
	assert.GreaterOrEqual(t, venue.connections.Load(), int32(2))
	assert.Equal(t, []string{"btcusdt@ticker"}, venue.subscribed()[1], "subscribed again")
	assert.Equal(t, "68100", f.last(t, "BTCUSDT"))
}

func TestStream_FollowsTheTiers(t *testing.T) {
	f := newFixture(t)
	venue := newFakeVenue(t, func(n int, conn *websocket.Conn) {
		if n == 1 {
			// ETH becomes hot while the first connection streams.
			_, err := f.store.SetTiers(context.Background(), map[int64]string{f.ids["ETHUSDT"]: marketdata.TierHot})
			if err != nil {
				return
			}
		}
		_ = conn.WriteMessage(websocket.TextMessage, tickerMessage("ETHUSDT", "3000", time.Now()))
		time.Sleep(2 * time.Second)
	})
	run := jobs.NewRun(marketdata.ModeLive, nil)
	require.NoError(t, newJob(t, f, venue, Options{RunFor: time.Second, Flush: 100 * time.Millisecond, Refresh: 300 * time.Millisecond}).Run(context.Background(), run))
	subs := venue.subscribed()
	require.GreaterOrEqual(t, len(subs), 2)
	assert.ElementsMatch(t, []string{"btcusdt@ticker", "ethusdt@ticker"}, subs[1], "resubscribed with the new hot pair")
	assert.Equal(t, "3000", f.last(t, "ETHUSDT"))
}

func TestStream_FailsAfterConnectionsInARowFail(t *testing.T) {
	f := newFixture(t)
	venue := newFakeVenue(t, func(int, *websocket.Conn) {})
	venue.server.Close()
	run := jobs.NewRun(marketdata.ModeLive, nil)
	err := newJob(t, f, venue, Options{RunFor: time.Minute, MaxFailures: 3}).Run(context.Background(), run)
	require.Error(t, err, "the runner backs off, and another worker may try")
	assert.Contains(t, err.Error(), "3 connections in a row failed")
}

func TestStream_ALostLeaseStopsTheStream(t *testing.T) {
	f := newFixture(t)
	venue := newFakeVenue(t, func(_ int, conn *websocket.Conn) {
		for i := 0; i < 100; i++ {
			if conn.WriteMessage(websocket.TextMessage, tickerMessage("BTCUSDT", "68000", time.Now())) != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	run := jobs.NewRun(marketdata.ModeLive, func(context.Context) error { return jobs.ErrLeaseLost })
	err := newJob(t, f, venue, Options{RunFor: time.Minute, Flush: 100 * time.Millisecond}).Run(context.Background(), run)
	assert.ErrorIs(t, err, jobs.ErrLeaseLost)
	assert.Empty(t, f.last(t, "BTCUSDT"), "nothing is written without the lease")
}

// TestStream_AnotherWorkerTakesOver runs the stream on two workers: one
// holds it; when its lease passes, as when that worker dies or loses the
// database, it stops writing, and the other worker takes the stream over.
func TestStream_AnotherWorkerTakesOver(t *testing.T) {
	f := newFixture(t)
	venue := newFakeVenue(t, func(n int, conn *websocket.Conn) {
		for i := 0; ; i++ {
			price := fmt.Sprintf("%d%04d", n, i)
			if conn.WriteMessage(websocket.TextMessage, tickerMessage("BTCUSDT", price, time.Now())) != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	runner := func(node string) *jobs.Runner {
		job := newJob(t, f, venue, Options{RunFor: time.Minute, Flush: 100 * time.Millisecond})
		r, err := jobs.NewRunner(f.db, jobs.Options{Node: node, Mode: marketdata.ModeLive, Poll: 100 * time.Millisecond,
			Lease: 3 * time.Second, MaxConcurrent: 1, RetryBase: time.Second, Logger: quiet()},
			jobs.Spec{Job: job, Interval: time.Second, Timeout: job.Timeout()})
		require.NoError(t, err)
		r.Start()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = r.Stop(ctx)
		})
		return r
	}
	runner("a")
	require.Eventually(t, func() bool { return f.last(t, "BTCUSDT") != "" }, 5*time.Second, 50*time.Millisecond)
	runner("b")
	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, int32(1), venue.connections.Load(), "one worker at a time holds the stream")

	var holder string
	require.NoError(t, f.db.QueryRow(`SELECT holder FROM market_job_leases WHERE job = 'stream:binance'`).Scan(&holder))
	// The holder's lease passes; the other worker claims it.
	_, err := f.db.Exec(`UPDATE market_job_leases SET lease_until = NOW() - interval '1 second', next_run_at = NOW() WHERE job = 'stream:binance'`)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		var now string
		return f.db.QueryRow(`SELECT holder FROM market_job_leases WHERE job = 'stream:binance'`).Scan(&now) == nil && now != holder
	}, 5*time.Second, 50*time.Millisecond, "the other worker takes the stream")
	require.Eventually(t, func() bool { return venue.connections.Load() >= 2 }, 5*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool { return strings.HasPrefix(f.last(t, "BTCUSDT"), "2") }, 5*time.Second, 50*time.Millisecond,
		"the tickers of the new connection are written")
}

func TestStreamPairs_TheTiersOfAnEnabledVenue(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	hot, err := f.store.StreamPairs(ctx, "binance", []string{marketdata.TierHot}, 10)
	require.NoError(t, err)
	require.Len(t, hot, 1)
	assert.Equal(t, "BTCUSDT", hot[0].VenueSymbol)
	assert.Equal(t, "BTC", hot[0].BaseSymbol)
	both, err := f.store.StreamPairs(ctx, "binance", []string{marketdata.TierHot, marketdata.TierOther}, 10)
	require.NoError(t, err)
	assert.Len(t, both, 2)
	_, err = f.db.Exec(`UPDATE market_venues SET enabled = FALSE`)
	require.NoError(t, err)
	none, err := f.store.StreamPairs(ctx, "binance", []string{marketdata.TierHot}, 10)
	require.NoError(t, err)
	assert.Empty(t, none)
}
