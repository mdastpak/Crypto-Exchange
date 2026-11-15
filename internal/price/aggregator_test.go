package price

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

var t0 = time.Unix(1_700_000_000, 0).UTC()

func sd(s string) shared.Decimal { return shared.MustNewDecimalFromString(s) }

func newTestAggregator(t *testing.T, opts Options) *Aggregator {
	t.Helper()
	if opts.Markets == nil {
		opts.Markets = []string{"BTC/USDT"}
	}
	if opts.Windows == nil {
		opts.Windows = []time.Duration{time.Second, 5 * time.Second}
	}
	if opts.OutlierFloor.IsZero() {
		opts.OutlierFloor = DefaultOutlierFloor
	}
	a, err := NewAggregator(opts)
	require.NoError(t, err)
	return a
}

func book(provider, bid, ask string, at time.Time) Tick {
	return Tick{Provider: provider, Market: "BTC/USDT", Kind: KindBook, Bid: sd(bid), Ask: sd(ask), Received: at}
}

func trade(provider, price, size string, at time.Time) Tick {
	return Tick{Provider: provider, Market: "BTC/USDT", Kind: KindTrade, Price: sd(price), Size: sd(size), At: at, Received: at}
}

func only(t *testing.T, prices []Price) Price {
	t.Helper()
	require.Len(t, prices, 1)
	return prices[0]
}

func TestAggregator_BestBidAskAndMedianMid(t *testing.T) {
	a := newTestAggregator(t, Options{})
	for _, tk := range []Tick{
		book("binance", "100", "101", t0),
		book("coinbase", "100.4", "101.2", t0),
		book("kraken", "99.8", "100.6", t0),
	} {
		require.Empty(t, a.Ingest(tk))
	}
	p := only(t, a.Compute(t0.Add(10*time.Millisecond)))
	assert.Equal(t, StatusLive, p.Status)
	assert.Equal(t, "100.4", p.Bid.String(), "the best bid")
	assert.Equal(t, "100.6", p.Ask.String(), "the best ask")
	// Mids 100.5, 100.8, 100.2: the median.
	assert.Equal(t, "100.5", p.Mid.String())
	assert.Equal(t, []string{"binance", "coinbase", "kraken"}, p.Sources)
	assert.Empty(t, p.Outliers)
	assert.Equal(t, t0, p.Updated)
}

func TestAggregator_RejectsTheOutlier(t *testing.T) {
	a := newTestAggregator(t, Options{})
	a.Ingest(book("binance", "100", "101", t0))
	a.Ingest(book("coinbase", "100.2", "101", t0))
	a.Ingest(book("kraken", "100.1", "100.9", t0))
	a.Ingest(book("bad", "150", "151", t0))
	p := only(t, a.Compute(t0))
	assert.Equal(t, StatusLive, p.Status)
	assert.Equal(t, []string{"bad"}, p.Outliers)
	assert.Equal(t, []string{"binance", "coinbase", "kraken"}, p.Sources)
	assert.Equal(t, "100.2", p.Bid.String(), "the outlier's bid is not the best bid")
	assert.Equal(t, "100.5", p.Mid.String())
}

// A provider that disconnects goes stale; the price stays live from the
// others while they make the quorum, and goes stale below it, with the
// figures of its last live price.
func TestAggregator_DegradesToStaleWhenTheQuorumIsLost(t *testing.T) {
	a := newTestAggregator(t, Options{MinProviders: 2, Stale: 5 * time.Second})
	a.Ingest(book("binance", "100", "101", t0))
	a.Ingest(book("coinbase", "100", "102", t0))
	a.Ingest(book("kraken", "100", "100.5", t0))
	assert.Equal(t, StatusLive, only(t, a.Compute(t0)).Status)

	// binance disconnects; the others keep sending.
	at := t0.Add(6 * time.Second)
	a.Ingest(book("coinbase", "100", "102", at))
	a.Ingest(book("kraken", "100", "100.5", at))
	p := only(t, a.Compute(at))
	assert.Equal(t, StatusLive, p.Status)
	assert.Equal(t, []string{"coinbase", "kraken"}, p.Sources)
	assert.Equal(t, "100.625", p.Mid.String())

	// kraken disconnects too: one provider is below the quorum.
	at2 := at.Add(6 * time.Second)
	a.Ingest(book("coinbase", "200", "202", at2))
	p = only(t, a.Compute(at2))
	assert.Equal(t, StatusStale, p.Status)
	assert.False(t, p.Live())
	assert.Equal(t, []string{"coinbase"}, p.Sources)
	assert.Equal(t, "100.625", p.Mid.String(), "the last live mid, marked stale")
	assert.Equal(t, at, p.Updated)
	assert.Equal(t, at2, p.At)

	// kraken comes back: live again, at the new prices.
	a.Ingest(book("kraken", "200", "201", at2))
	p = only(t, a.Compute(at2))
	assert.Equal(t, StatusLive, p.Status)
	assert.Equal(t, "200.75", p.Mid.String())
}

func TestAggregator_StaleWithoutAnyPrice(t *testing.T) {
	a := newTestAggregator(t, Options{})
	p := only(t, a.Compute(t0))
	assert.Equal(t, StatusStale, p.Status)
	assert.True(t, p.Mid.IsZero())
	assert.Equal(t, []string{}, p.Sources)
}

// The staleness cutoff is exact: a book as old as the cutoff counts, one a
// nanosecond older does not.
func TestAggregator_StalenessCutoff(t *testing.T) {
	a := newTestAggregator(t, Options{MinProviders: 1, Stale: time.Second, ProviderStale: map[string]time.Duration{"nobitex": 30 * time.Second}})
	a.Ingest(book("binance", "100", "101", t0))
	assert.Equal(t, StatusLive, only(t, a.Compute(t0.Add(time.Second))).Status)
	assert.Equal(t, StatusStale, only(t, a.Compute(t0.Add(time.Second+1))).Status)

	// A provider read through REST has its own cutoff.
	b := newTestAggregator(t, Options{MinProviders: 1, Stale: time.Second, ProviderStale: map[string]time.Duration{"nobitex": 30 * time.Second}})
	b.Ingest(book("nobitex", "100", "101", t0))
	assert.Equal(t, StatusLive, only(t, b.Compute(t0.Add(29*time.Second))).Status)
}

// The time of a tick is the provider's, unless it is ahead of ours.
func TestAggregator_TickTime(t *testing.T) {
	a := newTestAggregator(t, Options{MinProviders: 1, Stale: time.Second})
	old := book("binance", "100", "101", t0.Add(10*time.Second))
	old.At = t0
	a.Ingest(old)
	assert.Equal(t, StatusStale, only(t, a.Compute(t0.Add(10*time.Second))).Status, "observed at the provider 10 s ago")

	b := newTestAggregator(t, Options{MinProviders: 1, Stale: time.Second})
	ahead := book("binance", "100", "101", t0)
	ahead.At = t0.Add(time.Hour)
	b.Ingest(ahead)
	assert.Equal(t, t0, only(t, b.Compute(t0)).Updated, "a time ahead of ours is read as the receipt")
}

func TestAggregator_RejectsBadTicks(t *testing.T) {
	a := newTestAggregator(t, Options{})
	other := book("binance", "1", "2", t0)
	other.Market = "ETH/USDT"
	assert.Equal(t, RejectUnknownMarket, a.Ingest(other))
	assert.Equal(t, RejectInvalid, a.Ingest(book("binance", "0", "1", t0)))
	assert.Equal(t, RejectInvalid, a.Ingest(book("binance", "1", "-1", t0)))
	assert.Equal(t, RejectCrossed, a.Ingest(book("binance", "102", "101", t0)))
	assert.Equal(t, RejectInvalid, a.Ingest(trade("binance", "100", "0", t0)))
	assert.Equal(t, RejectInvalid, a.Ingest(Tick{Provider: "binance", Market: "BTC/USDT", Received: t0}))

	require.Empty(t, a.Ingest(book("binance", "100", "101", t0.Add(time.Second))))
	assert.Equal(t, RejectOutOfOrder, a.Ingest(book("binance", "90", "91", t0)), "an older book never replaces a newer one")

	late := trade("binance", "100", "1", t0)
	late.Received = t0.Add(time.Minute)
	assert.Equal(t, RejectTooOld, a.Ingest(late), "older than the longest window")
}

func TestAggregator_VWAPAndTWAP(t *testing.T) {
	a := newTestAggregator(t, Options{MinProviders: 2})
	a.Ingest(book("binance", "100", "100", t0))
	a.Ingest(book("coinbase", "100", "100", t0))
	a.Ingest(trade("binance", "100", "1", t0))
	a.Ingest(trade("coinbase", "103", "2", t0.Add(4*time.Second)))
	a.Ingest(trade("binance", "106", "1", t0.Add(4500*time.Millisecond)))
	// A trade of an outlier or stale provider does not count.
	a.Ingest(trade("kraken", "1000", "50", t0.Add(4500*time.Millisecond)))

	now := t0.Add(4900 * time.Millisecond)
	a.Ingest(book("binance", "100", "100", now))
	a.Ingest(book("coinbase", "100", "100", now))
	p := only(t, a.Compute(now))
	require.Equal(t, StatusLive, p.Status)
	// 1 s: (206 + 106) / 3 = 104; 5 s: (100 + 206 + 106) / 4 = 103.
	assert.Equal(t, "104", p.VWAP["1s"].String())
	assert.Equal(t, "103", p.VWAP["5s"].String())
	assert.Empty(t, p.TWAP, "a TWAP needs a second live interval")

	// The mid moves to 110 for a second, then 120 for a second.
	at := now.Add(time.Second)
	a.Ingest(book("binance", "110", "110", now))
	a.Ingest(book("coinbase", "110", "110", now))
	a.Compute(at)
	at2 := at.Add(time.Second)
	a.Ingest(book("binance", "120", "120", at))
	a.Ingest(book("coinbase", "120", "120", at))
	p = only(t, a.Compute(at2))
	// The TWAP weighs each mid by the time until the next: 110 over the
	// second before at, 120 over the one before at2.
	assert.Equal(t, "120", p.TWAP["1s"].String())
	assert.Equal(t, "115", p.TWAP["5s"].String())
}

// The figures are decimals: no float rounding creeps in.
func TestAggregator_ExactDecimals(t *testing.T) {
	a := newTestAggregator(t, Options{MinProviders: 1})
	a.Ingest(book("binance", "0.1", "0.2", t0))
	a.Ingest(trade("binance", "0.1", "0.3", t0))
	a.Ingest(trade("binance", "0.2", "0.3", t0))
	p := only(t, a.Compute(t0))
	assert.Equal(t, "0.15", p.Mid.String())
	assert.Equal(t, "0.15", p.VWAP["1s"].String())
}

func TestAggregator_Options(t *testing.T) {
	_, err := NewAggregator(Options{Windows: []time.Duration{time.Second}})
	assert.Error(t, err, "no market")
	_, err = NewAggregator(Options{Markets: []string{"BTC/USDT"}})
	assert.Error(t, err, "no window")
	_, err = NewAggregator(Options{Markets: []string{"BTC/USDT"}, Windows: []time.Duration{time.Second, 1050 * time.Millisecond}})
	assert.Error(t, err, "a window off the buckets")
	a, err := NewAggregator(Options{Markets: []string{"BTC/USDT"}, Windows: []time.Duration{time.Second}})
	require.NoError(t, err)
	assert.Equal(t, DefaultMinProviders, a.opts.MinProviders)
	assert.Equal(t, DefaultStale, a.opts.Stale)
	assert.True(t, a.opts.OutlierMADs.Equal(DefaultOutlierMADs))
}

func TestNormalizeMarket(t *testing.T) {
	for in, want := range map[string]string{
		"BTC/USDT": "BTC/USDT", "btc-usdt": "BTC/USDT", " eth/btc ": "ETH/BTC", "XBT/USD": "BTC/USD", "xdg-usd": "DOGE/USD",
	} {
		got, err := NormalizeMarket(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"btcusdt", "BTC/", "/USDT", "BTC/BTC", "BTC/US DT", "B$C/USDT", ""} {
		_, err := NormalizeMarket(in)
		assert.Error(t, err, in)
	}
}

// Load: three providers at 1,000 ticks per second each, over 20 markets,
// for the 300 s of the longest window; the p99 of an aggregation of every
// market stays under 10 ms (#58).
func TestAggregator_Load(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	markets := make([]string, 20)
	for i := range markets {
		markets[i] = fmt.Sprintf("C%d/USDT", i)
	}
	a, err := NewAggregator(Options{
		Markets: markets, Windows: []time.Duration{time.Second, 5 * time.Second, 60 * time.Second, 300 * time.Second},
		OutlierFloor: DefaultOutlierFloor, MinProviders: 2,
	})
	require.NoError(t, err)
	providers := []string{"binance", "coinbase", "kraken"}
	const (
		seconds       = 300
		ticksPerSec   = 1000
		interval      = 100 * time.Millisecond
		ticksPerCycle = ticksPerSec / 10
	)
	var durations []time.Duration
	var ingest time.Duration
	n := 0
	for cycle := range seconds * 10 {
		now := t0.Add(time.Duration(cycle+1) * interval)
		start := time.Now()
		for _, provider := range providers {
			for i := range ticksPerCycle {
				at := now.Add(-time.Duration(ticksPerCycle-i) * time.Millisecond)
				market := markets[n%len(markets)]
				price := decimal.NewFromInt(int64(10_000 + n%100))
				var tk Tick
				if n%2 == 0 {
					tk = Tick{Provider: provider, Market: market, Kind: KindBook, Bid: shared.Decimal(price), Ask: shared.Decimal(price.Add(decimal.NewFromInt(1))), Received: at}
				} else {
					tk = Tick{Provider: provider, Market: market, Kind: KindTrade, Price: shared.Decimal(price), Size: sd("0.01"), At: at, Received: at}
				}
				if reason := a.Ingest(tk); reason != "" {
					t.Fatalf("tick rejected: %s", reason)
				}
				n++
			}
		}
		ingest += time.Since(start)
		start = time.Now()
		prices := a.Compute(now)
		durations = append(durations, time.Since(start))
		if cycle > 10 && !prices[0].Live() {
			t.Fatalf("cycle %d: %s is stale", cycle, prices[0].Market)
		}
	}
	slices.Sort(durations)
	p99 := durations[len(durations)*99/100]
	perTick := ingest / time.Duration(n)
	t.Logf("%d ticks, %s per tick; aggregation of %d markets: p50 %s, p99 %s, max %s",
		n, perTick, len(markets), durations[len(durations)/2], p99, durations[len(durations)-1])
	// The budget is 10 ms. The race detector, with which CI runs the
	// tests, slows the code down up to 10 times, so the budget there is
	// scaled by as much: the check still catches an aggregation that grows
	// with the ticks instead of the markets.
	budget := 10 * time.Millisecond
	if raceEnabled {
		budget *= 10
	}
	assert.Less(t, p99, budget, "p99 of an aggregation")
	assert.Less(t, perTick, 300*time.Microsecond, "3,000 ticks a second must take well under a second")
}
