package currencies

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/redistest"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDBName is the PostgreSQL database of this package.
const testDBName = "testdb_currencies"

type fixture struct {
	db      *sql.DB
	redis   *redis.Client
	catalog *Catalog
}

// newFixture migrates the test database and empties the test Redis
// database. It skips the test when Redis is not reachable.
func newFixture(t testing.TB) *fixture {
	t.Helper()
	ctx := context.Background()
	client := redis.NewClient(&redis.Options{Addr: redistest.Addr, DB: redistest.Currencies})
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		t.Skipf("Redis is not reachable at %s: %v", redistest.Addr, err)
	}
	require.NoError(t, client.FlushDB(ctx).Err())
	t.Cleanup(func() {
		client.FlushDB(context.Background())
		client.Close()
	})
	db := pgtest.Migrated(t, testDBName)
	return &fixture{db: db, redis: client, catalog: New(db, client, time.Minute)}
}

// insert writes a currency to the database directly, as a change that
// bypasses the admin routes would.
func (f *fixture) insert(t testing.TB, symbol, status string, deleted bool) string {
	t.Helper()
	info := fmt.Sprintf(`{"en_name": %q, "fa_name": %q, "symbol": %q, "if_fiat": false, "is_token": false, "contract": ""}`,
		symbol, symbol, symbol)
	var hk string
	require.NoError(t, f.db.QueryRow(
		`INSERT INTO currencies (status, info, deleted_at) VALUES ($1, $2::jsonb, CASE WHEN $3 THEN NOW() END) RETURNING hk`,
		status, info, deleted,
	).Scan(&hk))
	return hk
}

func (f *fixture) cached(t *testing.T) bool {
	t.Helper()
	n, err := f.redis.Exists(context.Background(), listKey).Result()
	require.NoError(t, err)
	return n == 1
}

func symbols(list []models.Currency) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.Info.Symbol)
	}
	return out
}

func (f *fixture) list(t *testing.T) []string {
	t.Helper()
	list, err := f.catalog.List(context.Background())
	require.NoError(t, err)
	return symbols(list)
}

// countingHook counts the round trips to Redis.
type countingHook struct{ trips atomic.Int64 }

func (h *countingHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *countingHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.trips.Add(1)
		return next(ctx, cmd)
	}
}

func (h *countingHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.trips.Add(1)
		return next(ctx, cmds)
	}
}

func TestList_ShowsThePublicCurrenciesOfTheDatabase(t *testing.T) {
	f := newFixture(t)
	btc := f.insert(t, "BTC", models.CurrencyStatusApproved, false)
	f.insert(t, "ETH", models.CurrencyStatusPending, false)
	f.insert(t, "XRP", models.CurrencyStatusSuspend, false)
	f.insert(t, "LTC", models.CurrencyStatusApproved, true)
	f.insert(t, "USDT", models.CurrencyStatusApproved, false)

	list, err := f.catalog.List(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"BTC", "USDT"}, symbols(list), "approved and not deleted, oldest first")
	assert.Equal(t, btc, list[0].HK)
	assert.Equal(t, 1, list[0].ID)
	assert.Equal(t, models.CurrencyStatusApproved, list[0].Status)
	for _, field := range []string{list[0].CreatedAt, list[0].UpdatedAt} {
		_, err := time.Parse(time.RFC3339, field)
		assert.NoError(t, err, "timestamps are RFC 3339")
	}
	assert.True(t, f.cached(t), "a miss fills the cache")
}

func TestList_ServesTheCacheUntilAChangeInvalidatesIt(t *testing.T) {
	f := newFixture(t)
	f.insert(t, "BTC", models.CurrencyStatusApproved, false)
	require.Equal(t, []string{"BTC"}, f.list(t))

	f.insert(t, "ETH", models.CurrencyStatusApproved, false)
	assert.Equal(t, []string{"BTC"}, f.list(t), "the cached list is served until it is invalidated")

	require.NoError(t, f.catalog.Invalidate(context.Background()))
	assert.Equal(t, []string{"BTC", "ETH"}, f.list(t))
}

func TestList_ACacheHitIsOneRoundTrip(t *testing.T) {
	f := newFixture(t)
	f.insert(t, "BTC", models.CurrencyStatusApproved, false)
	require.Equal(t, []string{"BTC"}, f.list(t))

	hook := &countingHook{}
	f.redis.AddHook(hook)
	require.Equal(t, []string{"BTC"}, f.list(t))
	assert.Equal(t, int64(1), hook.trips.Load())
}

func TestList_StaysCorrectAfterAFlushAndAfterExpiry(t *testing.T) {
	f := newFixture(t)
	f.insert(t, "BTC", models.CurrencyStatusApproved, false)
	require.Equal(t, []string{"BTC"}, f.list(t))

	require.NoError(t, f.redis.FlushDB(context.Background()).Err())
	assert.Equal(t, []string{"BTC"}, f.list(t), "a flush only costs a load")

	f.catalog = New(f.db, f.redis, 50*time.Millisecond)
	require.NoError(t, f.catalog.Invalidate(context.Background()))
	require.Equal(t, []string{"BTC"}, f.list(t))
	require.Eventually(t, func() bool { return !f.cached(t) }, 2*time.Second, 10*time.Millisecond, "the list expires")
	assert.Equal(t, []string{"BTC"}, f.list(t), "an expired list is loaded again")
}

func TestList_LoadsAnUnreadableCacheAgain(t *testing.T) {
	f := newFixture(t)
	f.insert(t, "BTC", models.CurrencyStatusApproved, false)
	require.NoError(t, f.redis.Set(context.Background(), listKey, "not json", 0).Err())

	assert.Equal(t, []string{"BTC"}, f.list(t))
	data, err := f.redis.Get(context.Background(), listKey).Bytes()
	require.NoError(t, err)
	assert.True(t, json.Valid(data), "the list is cached again")
}

func TestList_ReadsTheDatabaseWhileRedisIsDown(t *testing.T) {
	f := newFixture(t)
	btc := f.insert(t, "BTC", models.CurrencyStatusApproved, false)

	// A port that nothing listens on.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())
	down := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 200 * time.Millisecond, MaxRetries: -1})
	defer down.Close()
	catalog := New(f.db, down, time.Minute)

	list, err := catalog.List(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"BTC"}, symbols(list))
	currency, found, err := catalog.Get(context.Background(), btc)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "BTC", currency.Info.Symbol)
}

// gatedLoad replaces the load of catalogs with one that counts the loads
// and blocks until release is closed.
func (f *fixture) gatedLoad(release <-chan struct{}, loads *atomic.Int32, catalogs ...*Catalog) {
	for _, c := range catalogs {
		c.load = func(ctx context.Context) ([]models.Currency, error) {
			loads.Add(1)
			<-release
			return f.catalog.loadFromDatabase(ctx)
		}
	}
}

// TestList_NeverCachesAListLoadedBeforeAChange holds a load from the
// database while a change commits and invalidates the list. The load must
// not write what it read, and a caller after the change must get the new
// list, from a load of its own once the lock is free.
func TestList_NeverCachesAListLoadedBeforeAChange(t *testing.T) {
	f := newFixture(t)
	f.insert(t, "BTC", models.CurrencyStatusApproved, false)
	loading := make(chan struct{})
	release := make(chan struct{})
	var loads atomic.Int32
	f.catalog.load = func(ctx context.Context) ([]models.Currency, error) {
		list, err := f.catalog.loadFromDatabase(ctx)
		if loads.Add(1) == 1 {
			close(loading)
			<-release
		}
		return list, err
	}
	early := make(chan []models.Currency, 1)
	go func() {
		list, err := f.catalog.List(context.Background())
		assert.NoError(t, err)
		early <- list
	}()
	<-loading

	f.insert(t, "ETH", models.CurrencyStatusApproved, false)
	require.NoError(t, f.catalog.Invalidate(context.Background()))
	late := make(chan []models.Currency, 1)
	go func() {
		list, err := f.catalog.List(context.Background())
		assert.NoError(t, err)
		late <- list
	}()

	close(release)
	assert.Equal(t, []string{"BTC"}, symbols(<-early), "the caller from before the change gets the list of its time")
	assert.Equal(t, []string{"BTC", "ETH"}, symbols(<-late), "the caller after the change gets the new list")
	assert.Equal(t, []string{"BTC", "ETH"}, f.list(t), "the earlier load did not write the old list")
	assert.Equal(t, int32(2), loads.Load())
}

// TestList_InstancesShareOneLoad runs two catalogs, as two server instances,
// on one Redis and one database. Concurrent misses on both cause one load
// from the database and one write of the list.
func TestList_InstancesShareOneLoad(t *testing.T) {
	f := newFixture(t)
	f.insert(t, "BTC", models.CurrencyStatusApproved, false)
	other := New(f.db, f.redis, time.Minute)
	release := make(chan struct{})
	var loads atomic.Int32
	f.gatedLoad(release, &loads, f.catalog, other)

	var wg sync.WaitGroup
	for i := range 20 {
		catalog := f.catalog
		if i%2 == 1 {
			catalog = other
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			list, err := catalog.List(context.Background())
			assert.NoError(t, err)
			assert.Equal(t, []string{"BTC"}, symbols(list))
		}()
	}
	// Gives the callers time to wait for the load; one that comes after it
	// finds the list cached, so the count does not depend on this.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	assert.Equal(t, int32(1), loads.Load(), "one instance loads the list, the other waits for it")
	n, err := f.redis.Exists(context.Background(), lockKey).Result()
	require.NoError(t, err)
	assert.Zero(t, n, "the lock is released")
}

// TestList_ReadsTheDatabaseWhenTheLoadingInstanceStops holds the lock as an
// instance would that was killed during a load. The others wait for the
// list, then read the database without writing the list or taking the lock.
func TestList_ReadsTheDatabaseWhenTheLoadingInstanceStops(t *testing.T) {
	f := newFixture(t)
	f.insert(t, "BTC", models.CurrencyStatusApproved, false)
	ctx := context.Background()
	require.NoError(t, f.redis.Set(ctx, lockKey, "a stopped instance", lockTTL).Err())
	f.catalog.wait = 100 * time.Millisecond

	start := time.Now()
	assert.Equal(t, []string{"BTC"}, f.list(t))
	assert.GreaterOrEqual(t, time.Since(start), f.catalog.wait, "the instance waited for the list first")
	assert.False(t, f.cached(t), "an instance without the lock does not write the list")
	holder, err := f.redis.Get(ctx, lockKey).Result()
	require.NoError(t, err)
	assert.Equal(t, "a stopped instance", holder, "the lock is left to expire")
}

// TestList_RefreshesAnOldListOnceAcrossInstances lets the list age past its
// time on two instances. One request refreshes it, while every other
// request, on both instances, is served the old list at once.
func TestList_RefreshesAnOldListOnceAcrossInstances(t *testing.T) {
	f := newFixture(t)
	f.insert(t, "BTC", models.CurrencyStatusApproved, false)
	const ttl = 300 * time.Millisecond
	f.catalog = New(f.db, f.redis, ttl)
	other := New(f.db, f.redis, ttl)
	require.Equal(t, []string{"BTC"}, f.list(t))
	f.insert(t, "ETH", models.CurrencyStatusApproved, false)
	time.Sleep(ttl + 50*time.Millisecond)

	release := make(chan struct{})
	var loads atomic.Int32
	f.gatedLoad(release, &loads, f.catalog, other)
	var served atomic.Int32
	results := make(chan []string, 20)
	for i := range 20 {
		catalog := f.catalog
		if i%2 == 1 {
			catalog = other
		}
		go func() {
			list, err := catalog.List(context.Background())
			assert.NoError(t, err)
			served.Add(1)
			results <- symbols(list)
		}()
	}
	require.Eventually(t, func() bool { return served.Load() == 19 }, 2*time.Second, 5*time.Millisecond,
		"every request but the refreshing one is served at once")
	close(release)
	require.Eventually(t, func() bool { return served.Load() == 20 }, 2*time.Second, 5*time.Millisecond)

	refreshed := 0
	for range 20 {
		if list := <-results; len(list) == 2 {
			refreshed++
		} else {
			assert.Equal(t, []string{"BTC"}, list, "the old list")
		}
	}
	assert.Equal(t, 1, refreshed, "the refreshing request gets the new list")
	assert.Equal(t, int32(1), loads.Load(), "one refresh for both instances")
	f.catalog.load = f.catalog.loadFromDatabase
	assert.Equal(t, []string{"BTC", "ETH"}, f.list(t), "the refreshed list is cached")
}

// TestList_BoundsTheWaitForAnUnresponsiveRedis reads through a Redis that
// accepts connections and never answers, as a stopped or partitioned one
// can look. The read falls back to the database within a fraction of the
// request's time instead of waiting for the client's timeouts.
func TestList_BoundsTheWaitForAnUnresponsiveRedis(t *testing.T) {
	f := newFixture(t)
	f.insert(t, "BTC", models.CurrencyStatusApproved, false)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	hanging := redis.NewClient(&redis.Options{
		Addr: listener.Addr().String(), DialTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second,
		ContextTimeoutEnabled: true, MaxRetries: -1,
	})
	defer hanging.Close()
	catalog := New(f.db, hanging, time.Minute)

	start := time.Now()
	list, err := catalog.List(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"BTC"}, symbols(list))
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestList_ConcurrentMissesShareOneLoad(t *testing.T) {
	f := newFixture(t)
	f.insert(t, "BTC", models.CurrencyStatusApproved, false)
	release := make(chan struct{})
	var loads atomic.Int32
	f.catalog.load = func(ctx context.Context) ([]models.Currency, error) {
		loads.Add(1)
		<-release
		return f.catalog.loadFromDatabase(ctx)
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			list, err := f.catalog.List(context.Background())
			assert.NoError(t, err)
			assert.Equal(t, []string{"BTC"}, symbols(list))
		}()
	}
	// Gives the callers time to join the load; a caller that comes after it
	// finds the list cached, so the count does not depend on this.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	assert.Equal(t, int32(1), loads.Load())
}

func TestList_ACallerStopsWaitingWhenItsContextEnds(t *testing.T) {
	f := newFixture(t)
	f.insert(t, "BTC", models.CurrencyStatusApproved, false)
	release := make(chan struct{})
	f.catalog.load = func(ctx context.Context) ([]models.Currency, error) {
		<-release
		return f.catalog.loadFromDatabase(ctx)
	}
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := f.catalog.List(ctx)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "got %v", err)
}

func TestList_ReportsADatabaseFailure(t *testing.T) {
	f := newFixture(t)
	f.catalog.load = func(context.Context) ([]models.Currency, error) {
		return nil, errors.New("connection refused")
	}
	_, err := f.catalog.List(context.Background())
	assert.ErrorContains(t, err, "connection refused")
	assert.False(t, f.cached(t))
}

func TestGet_FindsOnlyPublicCurrencies(t *testing.T) {
	f := newFixture(t)
	btc := f.insert(t, "BTC", models.CurrencyStatusApproved, false)
	eth := f.insert(t, "ETH", models.CurrencyStatusPending, false)

	currency, found, err := f.catalog.Get(context.Background(), btc)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, btc, currency.HK)
	for _, hk := range []string{eth, uuid.NewString()} {
		_, found, err := f.catalog.Get(context.Background(), hk)
		require.NoError(t, err)
		assert.False(t, found, hk)
	}
}

func TestInvalidate_DropsTheListAndCountsTheChange(t *testing.T) {
	f := newFixture(t)
	f.insert(t, "BTC", models.CurrencyStatusApproved, false)
	require.Equal(t, []string{"BTC"}, f.list(t))

	ctx := context.Background()
	require.NoError(t, f.catalog.Invalidate(ctx))
	require.NoError(t, f.catalog.Invalidate(ctx))
	assert.False(t, f.cached(t))
	generation, err := f.redis.Get(ctx, generationKey).Result()
	require.NoError(t, err)
	assert.Equal(t, "2", generation)
}

func TestForServices_NeedsPostgreSQLAndRedis(t *testing.T) {
	assert.Nil(t, ForServices(&models.InfraServices{}))
}

// benchmarkCurrencies is the size of the lists in the benchmarks.
const benchmarkCurrencies = 100

func benchmarkFixture(b *testing.B) *fixture {
	f := newFixture(b)
	for i := range benchmarkCurrencies {
		f.insert(b, fmt.Sprintf("C%03d", i), models.CurrencyStatusApproved, false)
	}
	return f
}

// BenchmarkList_CacheHit lists cached currencies: one MGET.
func BenchmarkList_CacheHit(b *testing.B) {
	f := benchmarkFixture(b)
	ctx := context.Background()
	_, err := f.catalog.List(ctx)
	require.NoError(b, err)
	for b.Loop() {
		if _, err := f.catalog.List(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkList_Database lists the currencies from PostgreSQL, as every miss
// does, and every request while Redis is down.
func BenchmarkList_Database(b *testing.B) {
	f := benchmarkFixture(b)
	ctx := context.Background()
	for b.Loop() {
		if _, err := f.catalog.shared(ctx, ""); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkList_ScanAndGet lists cached currencies the way the server did
// before: a SCAN of the keyspace and a GET per currency.
func BenchmarkList_ScanAndGet(b *testing.B) {
	f := benchmarkFixture(b)
	ctx := context.Background()
	list, err := f.catalog.loadFromDatabase(ctx)
	require.NoError(b, err)
	for _, currency := range list {
		data, err := json.Marshal(currency)
		require.NoError(b, err)
		require.NoError(b, f.redis.Set(ctx, "bench:currency:"+currency.HK, data, 0).Err())
	}
	for b.Loop() {
		var out []models.Currency
		iter := f.redis.Scan(ctx, 0, "bench:currency:*", 100).Iterator()
		for iter.Next(ctx) {
			data, err := f.redis.Get(ctx, iter.Val()).Bytes()
			if err != nil {
				b.Fatal(err)
			}
			var currency models.Currency
			if err := json.Unmarshal(data, &currency); err != nil {
				b.Fatal(err)
			}
			out = append(out, currency)
		}
		if err := iter.Err(); err != nil || len(out) != benchmarkCurrencies {
			b.Fatalf("listed %d currencies: %v", len(out), err)
		}
	}
}
