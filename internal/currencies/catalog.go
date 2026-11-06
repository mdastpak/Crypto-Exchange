// Package currencies serves the public currencies, the approved ones that
// are not deleted. PostgreSQL holds them. Redis holds one copy of the whole
// list, which every server instance shares and reads in one round trip; the
// list is loaded from PostgreSQL whenever it is not cached, and read from
// PostgreSQL directly while Redis is unavailable.
package currencies

import (
	"context"
	"database/sql"
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
	// listKey holds the JSON list of the public currencies. The hash tag
	// keeps both keys in one slot of a Redis cluster, since a script uses
	// both. The version changes with the format of the list.
	listKey = "{currencies}:public:v1"
	// generationKey counts the invalidations. A list loaded from the
	// database is cached only if no invalidation happened since the load
	// began, so that a slow load cannot cache what a change replaced.
	generationKey = "{currencies}:public:v1:generation"

	// DefaultTTL is how long the list stays cached when the configuration
	// sets no time. Changes invalidate it at once; the time bounds how long
	// a failed invalidation, or a change made directly in the database, can
	// leave it out of date.
	DefaultTTL = 5 * time.Minute

	// loadTimeout bounds a load from the database, which several requests
	// may share, so that it does not end with the request that started it.
	loadTimeout = 5 * time.Second

	// warnEvery bounds the warnings about the cache, which every read
	// would log while Redis is unavailable.
	warnEvery = 10 * time.Second
)

// setIfGeneration caches the list only while the generation is the one read
// before the load. A missing generation counts as "0".
var setIfGeneration = redis.NewScript(`
if (redis.call('GET', KEYS[2]) or '0') == ARGV[1] then
	redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
	return 1
end
return 0
`)

// Catalog lists the public currencies. Concurrent requests that find the
// list missing share one load from the database.
type Catalog struct {
	db    *sql.DB
	redis *redis.Client
	ttl   time.Duration
	loads singleflight.Group
	// load reads the public currencies from the database. Tests replace it.
	load func(ctx context.Context) ([]models.Currency, error)
	// lastWarning is when warn last logged, in Unix nanoseconds.
	lastWarning atomic.Int64
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

// New returns a catalog that reads db and caches the list in client for ttl,
// or for DefaultTTL when ttl is not positive.
func New(db *sql.DB, client *redis.Client, ttl time.Duration) *Catalog {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	c := &Catalog{db: db, redis: client, ttl: ttl}
	c.load = c.loadFromDatabase
	return c
}

// ForServices returns a catalog of the server's PostgreSQL and Redis, which
// caches the list for redis.currency_cache_ttl_seconds, or nil without
// either service.
func ForServices(is *models.InfraServices) *Catalog {
	if is.Postgres == nil || is.Redis == nil {
		return nil
	}
	return New(is.Postgres.DB, is.Redis.Client, time.Duration(config.Config.Redis.CurrencyCacheTTL)*time.Second)
}

// List returns the public currencies, oldest first. The slice may be shared
// with other callers and must not be modified.
func (c *Catalog) List(ctx context.Context) ([]models.Currency, error) {
	// One round trip reads the list and, for a miss, the generation.
	values, err := c.redis.MGet(ctx, listKey, generationKey).Result()
	if err != nil {
		// The cache cannot be filled either, so the load does not try.
		c.warn("The currency cache is unavailable, reading the database: %v", err)
		return c.shared(ctx, "")
	}
	if data, ok := values[0].(string); ok {
		var list []models.Currency
		if err := json.Unmarshal([]byte(data), &list); err == nil {
			return list, nil
		}
		c.warn("The cached currency list cannot be read, loading it again: %v", err)
	}
	generation := "0"
	if g, ok := values[1].(string); ok {
		generation = g
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
// list loaded before the change is not cached after it.
func (c *Catalog) Invalidate(ctx context.Context) error {
	_, err := c.redis.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Incr(ctx, generationKey)
		p.Del(ctx, listKey)
		return nil
	})
	return err
}

// shared loads the list from the database, together with the concurrent
// callers that saw the same generation, and caches it unless generation is
// empty. Callers that saw a later generation, after a change, do not join a
// load that may have started before the change. Each caller stops waiting
// when its own ctx ends.
func (c *Catalog) shared(ctx context.Context, generation string) ([]models.Currency, error) {
	loaded := c.loads.DoChan("generation "+generation, func() (interface{}, error) {
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loadTimeout)
		defer cancel()
		return c.loadAndFill(loadCtx, generation)
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

// loadAndFill loads the list and caches it if the generation, read before
// the load, has not changed since. An empty generation means not to cache.
func (c *Catalog) loadAndFill(ctx context.Context, generation string) ([]models.Currency, error) {
	list, err := c.load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load the public currencies: %w", err)
	}
	if generation == "" {
		return list, nil
	}
	data, err := json.Marshal(list)
	if err == nil {
		err = setIfGeneration.Run(ctx, c.redis, []string{listKey, generationKey}, generation, data, c.ttl.Milliseconds()).Err()
	}
	if err != nil {
		c.warn("The currency list was read from the database but not cached: %v", err)
	}
	return list, nil
}

func (c *Catalog) loadFromDatabase(ctx context.Context) ([]models.Currency, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, hk, status, info, created_at, updated_at FROM currencies
		WHERE status = 'approved' AND deleted_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]models.Currency, 0)
	for rows.Next() {
		var currency models.Currency
		var info []byte
		var created, updated sql.NullTime
		if err := rows.Scan(&currency.ID, &currency.HK, &currency.Status, &info, &created, &updated); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(info, &currency.Info); err != nil {
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
