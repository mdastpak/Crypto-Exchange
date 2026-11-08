package tickers

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
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

type fakeVenue struct {
	id      string
	tickers []source.Ticker
	err     error
	calls   atomic.Int32
}

func (f *fakeVenue) Name() string { return f.id }
func (f *fakeVenue) Venue() marketdata.Venue {
	return marketdata.Venue{ID: f.id, Name: "Venue " + f.id, Kind: marketdata.VenueCEX, Region: marketdata.RegionGlobal,
		Attribution: "Fake", Enabled: true}
}
func (f *fakeVenue) Tickers(context.Context) ([]source.Ticker, error) {
	f.calls.Add(1)
	return f.tickers, f.err
}

var at = time.Date(2026, 11, 8, 19, 0, 0, 0, time.UTC)

func tk(symbol, base, quote, price string) source.Ticker {
	return source.Ticker{VenueSymbol: symbol, Base: base, Quote: quote, Last: shared.MustNewDecimalFromString(price), ObservedAt: at}
}

func rank(n int) *int { return &n }

// catalog adds BTC, USDT, and two assets with the symbol UNI, of which only
// Uniswap is ranked.
func catalog(t *testing.T, store *marketdata.Store) map[string]int64 {
	t.Helper()
	ids := map[string]int64{}
	for _, a := range []struct {
		slug string
		d    marketdata.AssetDetails
	}{
		{"bitcoin", marketdata.AssetDetails{Symbol: "BTC", Name: "Bitcoin", Kind: marketdata.KindCoin, MarketCapRank: rank(1)}},
		{"tether", marketdata.AssetDetails{Symbol: "USDT", Name: "Tether", Kind: marketdata.KindToken, MarketCapRank: rank(3)}},
		{"uniswap", marketdata.AssetDetails{Symbol: "UNI", Name: "Uniswap", Kind: marketdata.KindToken, MarketCapRank: rank(25)}},
		{"unicorn", marketdata.AssetDetails{Symbol: "UNI", Name: "Unicorn", Kind: marketdata.KindToken}},
		{"wrapped-a", marketdata.AssetDetails{Symbol: "WX", Name: "Wrapped A", Kind: marketdata.KindToken, MarketCapRank: rank(100)}},
		{"wrapped-b", marketdata.AssetDetails{Symbol: "WX", Name: "Wrapped B", Kind: marketdata.KindToken, MarketCapRank: rank(200)}},
	} {
		id, _, err := store.CreateAsset(context.Background(), a.slug, "coingecko", a.slug, a.d)
		require.NoError(t, err)
		ids[a.slug] = id
	}
	return ids
}

func live() *jobs.Run { return jobs.NewRun(marketdata.ModeLive, nil) }

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(query, args...).Scan(&n))
	return n
}

func TestRun_WritesPairsTickersAndMapsTheUnambiguous(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_marketdata_tickers")
	store := marketdata.NewStore(db)
	ctx := context.Background()
	ids := catalog(t, store)
	venue := &fakeVenue{id: "binance", tickers: []source.Ticker{
		tk("BTCUSDT", "BTC", "USDT", "68000.123456789012345678"),
		tk("UNIUSDT", "UNI", "USDT", "7.5"),
		tk("WXUSDT", "WX", "USDT", "1"),
		tk("NEWUSDT", "NEW", "USDT", "0.1"),
		tk("BTCEUR", "BTC", "EUR", "63000"),
	}}
	run := live()
	require.NoError(t, New(venue, store, Options{}).Run(ctx, run))
	assert.Equal(t, 5, run.Read())
	assert.Equal(t, 5, run.Written())

	enabled, err := store.VenueEnabled(ctx, "binance")
	require.NoError(t, err)
	assert.True(t, enabled, "a new venue takes the source's default")
	pairs, err := store.VenuePairs(ctx, "binance")
	require.NoError(t, err)
	require.Len(t, pairs, 5)
	assert.Equal(t, ids["bitcoin"], *pairs["BTCUSDT"].BaseAssetID)
	assert.Equal(t, ids["tether"], *pairs["BTCUSDT"].QuoteAssetID)
	assert.Equal(t, ids["uniswap"], *pairs["UNIUSDT"].BaseAssetID, "the only UNI ranked within the limit")
	assert.Nil(t, pairs["WXUSDT"].BaseAssetID, "two ranked WX: left for review")
	assert.Nil(t, pairs["NEWUSDT"].BaseAssetID, "not in the catalog")
	assert.Nil(t, pairs["BTCEUR"].QuoteAssetID, "a fiat currency is not an asset")

	ticker, err := store.TickerOf(ctx, pairs["BTCUSDT"].ID)
	require.NoError(t, err)
	assert.Equal(t, "68000.123456789012345678", ticker.LastPrice.String())

	// An administrator maps WX; the next run keeps it, and a ticker that
	// did not change is not written again.
	_, err = db.Exec(`UPDATE market_venue_pairs SET base_asset_id = $1 WHERE venue_symbol = 'WXUSDT'`, ids["wrapped-b"])
	require.NoError(t, err)
	venue.tickers[0].ObservedAt = at.Add(time.Minute)
	run = live()
	require.NoError(t, New(venue, store, Options{}).Run(ctx, run))
	assert.Equal(t, 1, run.Written(), "only the newer ticker")
	pairs, err = store.VenuePairs(ctx, "binance")
	require.NoError(t, err)
	assert.Equal(t, ids["wrapped-b"], *pairs["WXUSDT"].BaseAssetID)
	assert.Equal(t, 5, count(t, db, `SELECT count(*) FROM market_venue_pairs`))
}

func TestRun_ADisabledVenueIsNotCollected(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_marketdata_tickers")
	store := marketdata.NewStore(db)
	ctx := context.Background()
	venue := &fakeVenue{id: "okx", tickers: []source.Ticker{tk("BTC-USDT", "BTC", "USDT", "1")}}
	require.NoError(t, New(venue, store, Options{}).Run(ctx, live()))
	_, err := db.Exec(`UPDATE market_venues SET enabled = FALSE WHERE id = 'okx'`)
	require.NoError(t, err)

	venue.calls.Store(0)
	require.NoError(t, New(venue, store, Options{}).Run(ctx, live()))
	assert.Equal(t, int32(0), venue.calls.Load(), "the venue is not called")
	enabled, err := store.VenueEnabled(ctx, "okx")
	require.NoError(t, err)
	assert.False(t, enabled, "the job never turns a venue back on")
}

func TestRun_DryRunWritesNothing(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_marketdata_tickers")
	store := marketdata.NewStore(db)
	venue := &fakeVenue{id: "kraken", tickers: []source.Ticker{tk("XBTUSD", "BTC", "USD", "1"), tk("XBTUSD", "BTC", "USD", "2")}}
	run := jobs.NewRun(marketdata.ModeDryRun, nil)
	require.NoError(t, New(venue, store, Options{}).Run(context.Background(), run))
	assert.Equal(t, 2, run.Read())
	assert.Equal(t, 1, run.Written(), "a pair listed twice is one")
	assert.Equal(t, 0, count(t, db, `SELECT count(*) FROM market_venues`))
}

func TestRun_APairListedTwiceKeepsTheLatest(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_marketdata_tickers")
	store := marketdata.NewStore(db)
	later := tk("XBTUSD", "BTC", "USD", "2")
	later.ObservedAt = at.Add(time.Second)
	venue := &fakeVenue{id: "kraken", tickers: []source.Ticker{later, tk("XBTUSD", "BTC", "USD", "1")}}
	require.NoError(t, New(venue, store, Options{}).Run(context.Background(), live()))
	pairs, err := store.VenuePairs(context.Background(), "kraken")
	require.NoError(t, err)
	ticker, err := store.TickerOf(context.Background(), pairs["XBTUSD"].ID)
	require.NoError(t, err)
	assert.Equal(t, "2", ticker.LastPrice.String())
}

func TestRun_PairsNoLongerListedBecomeInactive(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_marketdata_tickers")
	store := marketdata.NewStore(db)
	ctx := context.Background()
	venue := &fakeVenue{id: "coinbase", tickers: []source.Ticker{tk("BTC-USD", "BTC", "USD", "1"), tk("OLD-USD", "OLD", "USD", "1")}}
	require.NoError(t, New(venue, store, Options{}).Run(ctx, live()))
	_, err := db.Exec(`UPDATE market_venue_pairs SET last_seen_at = NOW() - interval '2 days'`)
	require.NoError(t, err)
	venue.tickers = venue.tickers[:1]
	require.NoError(t, New(venue, store, Options{}).Run(ctx, live()))
	pairs, err := store.VenuePairs(ctx, "coinbase")
	require.NoError(t, err)
	assert.True(t, pairs["BTC-USD"].Active)
	assert.False(t, pairs["OLD-USD"].Active)
}

func TestRun_StopsWhenTheLeaseIsLost(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_marketdata_tickers")
	store := marketdata.NewStore(db)
	venue := &fakeVenue{id: "wallex", tickers: []source.Ticker{tk("BTCTMN", "BTC", "IRT", "1")}}
	run := jobs.NewRun(marketdata.ModeLive, func(context.Context) error { return jobs.ErrLeaseLost })
	assert.ErrorIs(t, New(venue, store, Options{}).Run(context.Background(), run), jobs.ErrLeaseLost)
	assert.Equal(t, 0, count(t, db, `SELECT count(*) FROM market_tickers_latest`))
}

func TestRunner_AFailingVenueStopsNoOther(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_marketdata_tickers")
	store := marketdata.NewStore(db)
	good := &fakeVenue{id: "binance", tickers: []source.Ticker{tk("BTCUSDT", "BTC", "USDT", "1")}}
	bad := &fakeVenue{id: "nobitex", err: errors.New("connection refused")}
	runner, err := jobs.NewRunner(db, jobs.Options{Node: "test", Mode: marketdata.ModeLive, Poll: 20 * time.Millisecond,
		Lease: 10 * time.Second, MaxConcurrent: 2, RetryBase: time.Hour},
		jobs.Spec{Job: New(bad, store, Options{}), Interval: time.Hour, Timeout: time.Minute},
		jobs.Spec{Job: New(good, store, Options{}), Interval: time.Hour, Timeout: time.Minute})
	require.NoError(t, err)
	runner.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runner.Stop(ctx)
	}()
	require.Eventually(t, func() bool {
		return count(t, db, `SELECT count(*) FROM market_sync_runs WHERE status <> 'running'`) == 2
	}, 10*time.Second, 20*time.Millisecond)
	assert.Equal(t, 1, count(t, db, `SELECT count(*) FROM market_sync_runs WHERE job = 'tickers:nobitex' AND status = 'failed'`))
	assert.Equal(t, 1, count(t, db, `SELECT count(*) FROM market_sync_runs WHERE job = 'tickers:binance' AND status = 'succeeded'`))
	assert.Equal(t, 1, count(t, db, `SELECT count(*) FROM market_tickers_latest`))
}

func TestPick(t *testing.T) {
	id := func(n int64) *int64 { return &n }
	assert.Equal(t, id(1), pick([]marketdata.SymbolCandidate{{ID: 1}}, 1000), "the only one, ranked or not")
	assert.Equal(t, id(1), pick([]marketdata.SymbolCandidate{{ID: 1, Rank: rank(20)}, {ID: 2}}, 1000))
	assert.Nil(t, pick([]marketdata.SymbolCandidate{{ID: 1, Rank: rank(20)}, {ID: 2, Rank: rank(30)}}, 1000))
	assert.Equal(t, id(1), pick([]marketdata.SymbolCandidate{{ID: 1, Rank: rank(20)}, {ID: 2, Rank: rank(3000)}}, 1000))
	assert.Nil(t, pick([]marketdata.SymbolCandidate{{ID: 1}, {ID: 2}}, 1000))
	assert.Nil(t, pick(nil, 1000))
}
