// Package currencies serves the public currencies, the approved ones that
// are not deleted. PostgreSQL holds them. Redis holds one copy of the whole
// list, which every server instance shares and reads in one round trip.
//
// Several instances share the copy, so they must not all load and write it
// at the same time. One instance at a time, the one that holds a lock in
// Redis, loads the list from PostgreSQL and writes it; the others wait for
// the list, or keep serving the list they have while it is refreshed.
package currencies

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

const (
	// listKey holds the list of the public currencies and when it was
	// loaded. The hash tag keeps all the keys in one slot of a Redis
	// cluster, since scripts and MGET use several of them. The version
	// changes with the format of the value.
	listKey = "{currencies}:public:v1"
	// generationKey counts the invalidations. A list is written only if no
	// invalidation happened since its load began, so that a slow load
	// cannot write what a change replaced.
	generationKey = "{currencies}:public:v1:generation"
	// lockKey is held by the one instance that loads and writes the list.
	lockKey = "{currencies}:public:v1:loading"

	// DefaultTTL is how long a loaded list is fresh when the configuration
	// sets no time. Changes invalidate it at once; the time bounds how long
	// a failed invalidation, or a change made directly in the database, can
	// leave it out of date. After it, one instance refreshes the list while
	// every instance keeps serving it, and the key expires after twice the
	// time, if nothing reads it.
	DefaultTTL = 5 * time.Minute

	// loadTimeout bounds a load from the database, which several requests
	// may share, so that it does not end with the request that started it.
	loadTimeout = 5 * time.Second
	// lockTTL is how long the lock lasts if its holder stops, for example
	// when its instance is killed during a load.
	lockTTL = 2 * loadTimeout
	// defaultWait is how long an instance waits for another one that
	// loads the list. After it, the instance reads the database itself,
	// without writing the list.
	defaultWait = 2 * time.Second
	// pollInterval is how often a waiting instance looks for the list.
	pollInterval = 20 * time.Millisecond
	// defaultRedisTimeout bounds each Redis call of a read, so that an
	// unavailable Redis costs a read at most this long before it reads the
	// database instead. A Redis that answers takes well under a millisecond.
	defaultRedisTimeout = 250 * time.Millisecond

	// warnEvery bounds the warnings about the cache, which every read
	// would log while Redis is unavailable.
	warnEvery = 10 * time.Second
)

// writeIfGeneration writes the list only while the generation is the one
// read before the load. A missing generation counts as "0".
var writeIfGeneration = redis.NewScript(`
if (redis.call('GET', KEYS[2]) or '0') == ARGV[1] then
	redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
	return 1
end
return 0
`)

// unlock releases the lock only for the holder whose token it has.
var unlock = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('DEL', KEYS[1])
end
return 0
`)

// cachedList is the value of listKey.
type cachedList struct {
	// LoadedAt is when the list was loaded, in Unix milliseconds.
	LoadedAt   int64             `json:"loaded_at"`
	Currencies []models.Currency `json:"currencies"`
}

// Catalog lists the public currencies.
type Catalog struct {
	db    *sql.DB
	redis redis.UniversalClient
	ttl   time.Duration
	// wait is how long to wait for another instance that loads the list.
	wait time.Duration
	// redisTimeout bounds each Redis call of a read.
	redisTimeout time.Duration
	// loads merges the concurrent misses of this instance.
	loads singleflight.Group
	// refreshing is set while this instance tries to refresh a list that
	// is no longer fresh, so that it tries once at a time.
	refreshing atomic.Bool
	// load reads the public currencies from the database. Tests replace it.
	load func(ctx context.Context) ([]models.Currency, error)
	// lastWarning is when warn last logged, in Unix nanoseconds.
	lastWarning atomic.Int64
}

// New returns a catalog that reads db and caches the list in client, fresh
// for ttl, or for DefaultTTL when ttl is not positive. client must honor
// context deadlines (ContextTimeoutEnabled), as the server's client does.
func New(db *sql.DB, client redis.UniversalClient, ttl time.Duration) *Catalog {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	c := &Catalog{db: db, redis: client, ttl: ttl, wait: defaultWait, redisTimeout: defaultRedisTimeout}
	c.load = c.loadFromDatabase
	return c
}

// ForServices returns a catalog of the server's PostgreSQL and Redis, whose
// list is fresh for redis.currency_cache_ttl_seconds, or nil without either
// service.
func ForServices(is *models.InfraServices) *Catalog {
	if is.Postgres == nil || is.Redis == nil {
		return nil
	}
	return New(is.Postgres.DB, is.Redis.Client, time.Duration(config.Config.Redis.CurrencyCacheTTL)*time.Second)
}

// List returns the public currencies, oldest first. The slice may be shared
// with other callers and must not be modified.
func (c *Catalog) List(ctx context.Context) ([]models.Currency, error) {
	// One round trip reads the list and the generation.
	values, err := c.read(ctx)
	if err != nil {
		c.warn("The currency cache is unavailable, reading the database: %v", err)
		return c.shared(ctx, "")
	}
	generation := "0"
	if g, ok := values[1].(string); ok {
		generation = g
	}
	if cached, ok := c.decode(values[0]); ok {
		if c.fresh(cached) {
			return cached.Currencies, nil
		}
		if fresh, ok := c.refresh(ctx, generation); ok {
			return fresh, nil
		}
		return cached.Currencies, nil
	}
	return c.shared(ctx, generation)
}

// Get returns the public currency with the given hk, which must be the
// canonical form of a UUID. found is false when no public currency has it.
func (c *Catalog) Get(ctx context.Context, hk string) (currency models.Currency, found bool, err error) {
	list, err := c.List(ctx)
	if err != nil {
		return models.Currency{}, false, err
	}
	for _, currency := range list {
		if currency.HK == hk {
			return currency, true, nil
		}
	}
	return models.Currency{}, false, nil
}

// Invalidate drops the cached list. It is called after a change to the
// public currencies is committed. It also bumps the generation, so that a
// list loaded before the change is not written after it. Instances may
// invalidate at the same time: each invalidation is one transaction, and
// repeating it changes nothing but the count.
func (c *Catalog) Invalidate(ctx context.Context) error {
	_, err := c.redis.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Incr(ctx, generationKey)
		p.Del(ctx, listKey)
		return nil
	})
	return err
}

// decode reads a value of listKey, which MGET returns as a string, or nil
// when the key is missing.
func (c *Catalog) decode(value interface{}) (cachedList, bool) {
	data, ok := value.(string)
	if !ok {
		return cachedList{}, false
	}
	var cached cachedList
	if err := json.Unmarshal([]byte(data), &cached); err != nil || cached.Currencies == nil {
		c.warn("The cached currency list cannot be read, loading it again: %v", err)
		return cachedList{}, false
	}
	return cached, true
}

// refresh reloads a list that is no longer fresh, if no other instance is
// doing it. The other callers keep serving the list they read meanwhile.
func (c *Catalog) refresh(ctx context.Context, generation string) ([]models.Currency, bool) {
	if !c.refreshing.CompareAndSwap(false, true) {
		return nil, false
	}
	defer c.refreshing.Store(false)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loadTimeout)
	defer cancel()
	token, locked, err := c.lock(ctx)
	if err != nil || !locked {
		return nil, false
	}
	defer c.unlock(token)
	cached, generation, ok := c.reread(ctx, generation)
	if ok && c.fresh(cached) {
		return cached.Currencies, true
	}
	list, err := c.loadAndWrite(ctx, generation)
	if err != nil {
		c.warn("The currency list could not be refreshed: %v", err)
		return nil, false
	}
	return list, true
}

// shared gets the list for a miss, together with the concurrent callers of
// this instance that saw the same generation. Callers that saw a later
// generation, after a change, do not join a load that may have started
// before the change. An empty generation means that Redis is unavailable,
// so the list is read from the database. Each caller stops waiting when its
// own ctx ends.
func (c *Catalog) shared(ctx context.Context, generation string) ([]models.Currency, error) {
	loaded := c.loads.DoChan("generation "+generation, func() (interface{}, error) {
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loadTimeout)
		defer cancel()
		if generation == "" {
			return c.loadOnly(loadCtx)
		}
		return c.fetch(loadCtx, generation)
	})
	select {
	case result := <-loaded:
		if result.Err != nil {
			return nil, result.Err
		}
		return result.Val.([]models.Currency), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fetch loads and writes the list if this instance gets the lock and the
// list is still missing, and otherwise waits for the instance that has it.
// If the list does not come within wait, it reads the database without
// writing the list.
func (c *Catalog) fetch(ctx context.Context, generation string) ([]models.Currency, error) {
	deadline := time.Now().Add(c.wait)
	for {
		token, locked, err := c.lock(ctx)
		if err != nil {
			c.warn("The currency cache is unavailable, reading the database: %v", err)
			return c.loadOnly(ctx)
		}
		if locked {
			cached, current, ok := c.reread(ctx, generation)
			if ok {
				c.unlock(token)
				return cached.Currencies, nil
			}
			list, err := c.loadAndWrite(ctx, current)
			c.unlock(token)
			return list, err
		}

		if time.Now().After(deadline) {
			c.warn("Another instance is loading the currency list for longer than %v, reading the database", c.wait)
			return c.loadOnly(ctx)
		}
		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		values, err := c.read(ctx)
		if err != nil {
			c.warn("The currency cache is unavailable, reading the database: %v", err)
			return c.loadOnly(ctx)
		}
		if cached, ok := c.decode(values[0]); ok {
			return cached.Currencies, nil
		}
		// A change during the wait: a load from now on writes for it.
		if g, ok := values[1].(string); ok {
			generation = g
		}
	}
}

// fresh reports whether a cached list is younger than the catalog's time.
func (c *Catalog) fresh(cached cachedList) bool {
	return time.Since(time.UnixMilli(cached.LoadedAt)) < c.ttl
}

// reread reads the list again once this instance holds the lock. The
// previous holder writes the list before it releases the lock, so a caller
// that read a miss, or an old list, just before that write finds the new
// list here instead of loading it again. It also returns the generation,
// under which a load that begins after this read may write; on an error, it
// returns the one it was given.
func (c *Catalog) reread(ctx context.Context, generation string) (cachedList, string, bool) {
	values, err := c.read(ctx)
	if err != nil {
		return cachedList{}, generation, false
	}
	if g, ok := values[1].(string); ok {
		generation = g
	}
	cached, ok := c.decode(values[0])
	return cached, generation, ok
}

// read gets the list and the generation in one round trip.
func (c *Catalog) read(ctx context.Context) ([]interface{}, error) {
	ctx, cancel := context.WithTimeout(ctx, c.redisTimeout)
	defer cancel()
	return c.redis.MGet(ctx, listKey, generationKey).Result()
}

// lock takes the lock for loading the list, if no instance holds it.
func (c *Catalog) lock(ctx context.Context) (token string, locked bool, err error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", false, err
	}
	token = hex.EncodeToString(random)
	ctx, cancel := context.WithTimeout(ctx, c.redisTimeout)
	defer cancel()
	locked, err = c.redis.SetNX(ctx, lockKey, token, lockTTL).Result()
	return token, locked, err
}

// unlock releases the lock if this holder still has it. A failure only
// keeps the lock until it expires.
func (c *Catalog) unlock(token string) {
	ctx, cancel := context.WithTimeout(context.Background(), c.redisTimeout)
	defer cancel()
	if err := unlock.Run(ctx, c.redis, []string{lockKey}, token).Err(); err != nil {
		c.warn("The currency list lock could not be released, it expires after %v: %v", lockTTL, err)
	}
}

// loadOnly reads the list from the database without writing it.
func (c *Catalog) loadOnly(ctx context.Context) ([]models.Currency, error) {
	list, err := c.load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load the public currencies: %w", err)
	}
	return list, nil
}

// loadAndWrite reads the list from the database and writes it if the
// generation, read before the load, has not changed since. The caller holds
// the lock.
func (c *Catalog) loadAndWrite(ctx context.Context, generation string) ([]models.Currency, error) {
	loadedAt := time.Now()
	list, err := c.loadOnly(ctx)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(cachedList{LoadedAt: loadedAt.UnixMilli(), Currencies: list})
	if err == nil {
		writeCtx, cancel := context.WithTimeout(ctx, c.redisTimeout)
		err = writeIfGeneration.Run(writeCtx, c.redis, []string{listKey, generationKey},
			generation, data, (2 * c.ttl).Milliseconds()).Err()
		cancel()
	}
	if err != nil {
		c.warn("The currency list was read from the database but not cached: %v", err)
	}
	return list, nil
}

// warn logs a problem with the cache, at most once per warnEvery.
func (c *Catalog) warn(format string, args ...interface{}) {
	now := time.Now().UnixNano()
	last := c.lastWarning.Load()
	if now-last < int64(warnEvery) || !c.lastWarning.CompareAndSwap(last, now) {
		return
	}
	log.Printf(format, args...)
}

func (c *Catalog) loadFromDatabase(ctx context.Context) ([]models.Currency, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT c.id, c.hk, c.status, c.info, c.created_at, c.updated_at, `+DetailColumns+` FROM currencies c
		WHERE c.status = 'approved' AND c.deleted_at IS NULL ORDER BY c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]models.Currency, 0)
	for rows.Next() {
		var currency models.Currency
		var info []byte
		var created, updated sql.NullTime
		var lifecycle DetailScan
		if err := rows.Scan(append([]any{&currency.ID, &currency.HK, &currency.Status, &info, &created, &updated}, lifecycle.Dest()...)...); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(info, &currency.Info); err != nil {
			return nil, fmt.Errorf("currency %s: %w", currency.HK, err)
		}
		var err error
		if err = lifecycle.Apply(&currency); err != nil {
			return nil, fmt.Errorf("currency %s: %w", currency.HK, err)
		}
		currency.CreatedAt = timestamp(created)
		currency.UpdatedAt = timestamp(updated)
		list = append(list, currency)
	}
	return list, rows.Err()
}

// timestamp formats a database time in RFC 3339, in UTC, or as "" when it is
// NULL.
func timestamp(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}
