package candles

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/jobs"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var now = time.Date(2026, 11, 8, 19, 30, 0, 0, time.UTC)

type request struct {
	symbol   string
	from, to time.Time
}

// fakeVenue gives a candle for every hour of a known pair since listedAt,
// at most max per request, except in its gaps, and records the requests.
type fakeVenue struct {
	mu       sync.Mutex
	max      int
	listedAt time.Time
	gaps     map[time.Time]bool
	failOn   string
	asked    []request
}

func (f *fakeVenue) Name() string    { return "binance" }
func (f *fakeVenue) MaxCandles() int { return f.max }
func (f *fakeVenue) Candles(_ context.Context, symbol string, interval source.Interval, from, to time.Time) ([]source.Candle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, request{symbol, from, to})
	if symbol == f.failOn {
		return nil, errors.New("connection reset")
	}
	if symbol == "GONEUSDT" {
		return nil, source.ErrNotFound
	}
	var out []source.Candle
	for t := from; t.Before(to) && len(out) < f.max; t = t.Add(interval.Duration()) {
		if t.Before(f.listedAt) || f.gaps[t] || t.Add(interval.Duration()).After(now) {
			continue
		}
		p := shared.NewDecimalFromInt64(t.Unix() / 3600 % 1000)
		out = append(out, source.Candle{OpenTime: t, Open: p, High: p, Low: p, Close: p, Volume: shared.NewDecimalFromInt64(1)})
	}
	return out, nil
}

func (f *fakeVenue) requests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.asked...)
}

// setup adds an enabled venue with the pairs, mapped to ranked assets.
func setup(t *testing.T, symbols ...string) (*sql.DB, *marketdata.Store, map[string]int64) {
	t.Helper()
	db := pgtest.Migrated(t, "testdb_marketdata_candles")
	store := marketdata.NewStore(db)
	ctx := context.Background()
	require.NoError(t, store.UpsertVenue(ctx, marketdata.Venue{ID: "binance", Name: "Binance", Kind: marketdata.VenueCEX,
		Region: marketdata.RegionGlobal, Enabled: true}))
	var pairs []marketdata.VenuePair
	for i, sym := range symbols {
		rank := i + 1
		base := sym[:len(sym)-4]
		id, _, err := store.CreateAsset(ctx, base, "coingecko", base, marketdata.AssetDetails{Symbol: base, Name: base,
			Kind: marketdata.KindCoin, MarketCapRank: &rank})
		require.NoError(t, err)
		pairs = append(pairs, marketdata.VenuePair{VenueSymbol: sym, BaseSymbol: base, QuoteSymbol: "USDT", BaseAssetID: &id})
	}
	ids, err := store.UpsertVenuePairs(ctx, "binance", pairs)
	require.NoError(t, err)
	return db, store, ids
}

func newJob(t *testing.T, store *marketdata.Store, src source.CandleSource, requests int, backfill time.Duration) *Job {
	t.Helper()
	j, err := New(src, store, Options{Resolution: marketdata.Resolution1h, Quotes: []string{"USDT"}, MaxPairs: 10,
		RequestsPerRun: requests, Backfill: backfill, Now: func() time.Time { return now }})
	require.NoError(t, err)
	return j
}

func live() *jobs.Run { return jobs.NewRun(marketdata.ModeLive, nil) }

func stored(t *testing.T, store *marketdata.Store, pair int64) []time.Time {
	t.Helper()
	cs, err := store.Candles(context.Background(), pair, marketdata.Resolution1h, now.Add(-1000*time.Hour), now)
	require.NoError(t, err)
	var out []time.Time
	for _, c := range cs {
		out = append(out, c.OpenTime)
	}
	return out
}

func hours(from time.Time, n int) []time.Time {
	var out []time.Time
	for i := range n {
		out = append(out, from.Add(time.Duration(i)*time.Hour))
	}
	return out
}

// end is the open time after the last closed candle at now: 19:00.
var end = time.Date(2026, 11, 8, 19, 0, 0, 0, time.UTC)

func TestRun_LatestFirstThenBackfillResumesAcrossRuns(t *testing.T) {
	_, store, ids := setup(t, "BTCUSDT")
	venue := &fakeVenue{max: 10}
	// 48 hours of history, 10 candles per request, 2 requests per run.
	job := newJob(t, store, venue, 2, 48*time.Hour)

	require.NoError(t, job.Run(context.Background(), live()))
	assert.Equal(t, []request{
		{"BTCUSDT", end.Add(-10 * time.Hour), end},
		{"BTCUSDT", end.Add(-20 * time.Hour), end.Add(-10 * time.Hour)},
	}, venue.requests(), "the latest candles, then backward")
	assert.Equal(t, hours(end.Add(-20*time.Hour), 20), stored(t, store, ids["BTCUSDT"]))

	// A restart resumes from the cursor in the database.
	require.NoError(t, newJob(t, store, venue, 2, 48*time.Hour).Run(context.Background(), live()))
	require.NoError(t, newJob(t, store, venue, 2, 48*time.Hour).Run(context.Background(), live()))
	assert.Equal(t, hours(end.Add(-48*time.Hour), 48), stored(t, store, ids["BTCUSDT"]), "48 hours, no more")
	asked := len(venue.requests())
	require.NoError(t, newJob(t, store, venue, 2, 48*time.Hour).Run(context.Background(), live()))
	assert.Len(t, venue.requests(), asked, "nothing left to ask")
}

func TestRun_ForwardFillsTheGapSinceTheLastRun(t *testing.T) {
	_, store, ids := setup(t, "BTCUSDT")
	venue := &fakeVenue{max: 10}
	require.NoError(t, newJob(t, store, venue, 1, 10*time.Hour).Run(context.Background(), live()))

	// Five hours later the job is behind by five candles.
	now = now.Add(5 * time.Hour)
	defer func() { now = now.Add(-5 * time.Hour) }()
	venue.asked = nil
	require.NoError(t, newJob(t, store, venue, 1, 10*time.Hour).Run(context.Background(), live()))
	assert.Equal(t, []request{{"BTCUSDT", end, end.Add(5 * time.Hour)}}, venue.requests(), "forward first")
	assert.Equal(t, hours(end.Add(-10*time.Hour), 15), stored(t, store, ids["BTCUSDT"]))
}

func TestRun_AnEmptyRangeIsNotAskedAgain(t *testing.T) {
	_, store, ids := setup(t, "NEWUSDT")
	// Listed 5 hours ago, with a gap at the venue.
	venue := &fakeVenue{max: 10, listedAt: end.Add(-5 * time.Hour), gaps: map[time.Time]bool{end.Add(-2 * time.Hour): true}}
	require.NoError(t, newJob(t, store, venue, 5, 30*time.Hour).Run(context.Background(), live()))
	assert.Len(t, venue.requests(), 3, "10 + 10 + 10 hours down to the depth")
	got := stored(t, store, ids["NEWUSDT"])
	assert.Len(t, got, 4, "five hours less the gap")
	venue.asked = nil
	require.NoError(t, newJob(t, store, venue, 5, 30*time.Hour).Run(context.Background(), live()))
	assert.Empty(t, venue.requests(), "the empty history and the gap are not asked again")
}

func TestRun_SharesTheBudgetAndSkipsAPairTheVenueDropped(t *testing.T) {
	_, store, ids := setup(t, "BTCUSDT", "GONEUSDT", "ETHUSDT")
	venue := &fakeVenue{max: 10}
	require.NoError(t, newJob(t, store, venue, 3, 100*time.Hour).Run(context.Background(), live()))
	var symbols []string
	for _, r := range venue.requests() {
		symbols = append(symbols, r.symbol)
	}
	assert.Equal(t, []string{"BTCUSDT", "GONEUSDT", "ETHUSDT"}, symbols, "the latest candles of every pair first")
	assert.Len(t, stored(t, store, ids["ETHUSDT"]), 10)
}

func TestRun_AFailureFailsTheRunAndKeepsWhatWasStored(t *testing.T) {
	_, store, ids := setup(t, "BTCUSDT", "ETHUSDT")
	venue := &fakeVenue{max: 10, failOn: "ETHUSDT"}
	err := newJob(t, store, venue, 4, 100*time.Hour).Run(context.Background(), live())
	assert.Error(t, err)
	assert.Len(t, stored(t, store, ids["BTCUSDT"]), 10, "kept, and not asked again")
}

func TestRun_DryRunWritesNothing(t *testing.T) {
	db, store, _ := setup(t, "BTCUSDT")
	venue := &fakeVenue{max: 10}
	run := jobs.NewRun(marketdata.ModeDryRun, nil)
	require.NoError(t, newJob(t, store, venue, 2, 48*time.Hour).Run(context.Background(), run))
	assert.Equal(t, 20, run.Written())
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM market_candle_cursors`).Scan(&n))
	assert.Zero(t, n)
}

func TestRun_TheBackfillStopsAtTheRetention(t *testing.T) {
	_, store, _ := setup(t, "BTCUSDT")
	venue := &fakeVenue{max: 10}
	j, err := New(venue, store, Options{Resolution: marketdata.Resolution1h, Quotes: []string{"USDT"}, MaxPairs: 10,
		RequestsPerRun: 10, Backfill: 100 * time.Hour, Retention: 15 * time.Hour, Now: func() time.Time { return now }})
	require.NoError(t, err)
	require.NoError(t, j.Run(context.Background(), live()))
	reqs := venue.requests()
	require.Len(t, reqs, 2)
	assert.Equal(t, end.Add(-15*time.Hour), reqs[1].from)
}

func TestNew_Checks(t *testing.T) {
	_, err := New(&fakeVenue{max: 10}, nil, Options{Resolution: "2h", Quotes: []string{"USDT"}, MaxPairs: 1, RequestsPerRun: 1, Backfill: time.Hour})
	assert.Error(t, err)
	_, err = New(&fakeVenue{max: 10}, nil, Options{Resolution: "1h", MaxPairs: 1, RequestsPerRun: 1, Backfill: time.Hour})
	assert.Error(t, err)
	_, err = New(&fakeVenue{max: 0}, nil, Options{Resolution: "1h", Quotes: []string{"USDT"}, MaxPairs: 1, RequestsPerRun: 1, Backfill: time.Hour})
	assert.Error(t, err)
}

func TestPartitions_EnsureAndDrop(t *testing.T) {
	_, store, _ := setup(t)
	ctx := context.Background()
	old := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, store.EnsureCandlePartitions(ctx, marketdata.Resolution1h, old))
	job := NewPartitions(store, []Retention{{Resolution: marketdata.Resolution1h, Keep: 180 * 24 * time.Hour, Ensure: true},
		{Resolution: marketdata.Resolution1d, Ensure: true}, {Resolution: marketdata.Resolution5m, Keep: time.Hour}}, func() time.Time { return now })
	run := live()
	require.NoError(t, job.Run(ctx, run))
	hourly, err := store.CandlePartitions(ctx, marketdata.Resolution1h)
	require.NoError(t, err)
	assert.Equal(t, []string{"market_candles_1h_2026_05", "market_candles_1h_2026_11", "market_candles_1h_2026_12"}, hourly,
		"April ends more than 180 days ago; May holds candles within them")
	daily, err := store.CandlePartitions(ctx, marketdata.Resolution1d)
	require.NoError(t, err)
	assert.Equal(t, []string{"market_candles_1d_2026", "market_candles_1d_2027"}, daily)
	assert.Equal(t, 1, run.Written())
	fiveMinutes, err := store.CandlePartitions(ctx, marketdata.Resolution5m)
	require.NoError(t, err)
	assert.Empty(t, fiveMinutes, "5m is not collected")
	assert.Empty(t, job.Sources())
}
