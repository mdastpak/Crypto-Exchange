package marketdata

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Cache keeps the answers of the API, shared by every instance. Its keys
// carry a generation: Invalidate raises it, so that every instance stops
// reading the answers cached before, at once, without deleting them; they
// expire with their TTL.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Generation(ctx context.Context) (int64, error)
	Invalidate(ctx context.Context) error
}

// generationKey holds the generation of the cached answers.
const generationKey = "marketdata:api:generation"

// RedisCache is the Cache in Redis.
type RedisCache struct {
	Client redis.UniversalClient
}

// Get implements Cache.
func (r RedisCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	value, err := r.Client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	return value, err == nil, err
}

// Set implements Cache.
func (r RedisCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return r.Client.Set(ctx, key, value, ttl).Err()
}

// Generation implements Cache.
func (r RedisCache) Generation(ctx context.Context) (int64, error) {
	raw, err := r.Client.Get(ctx, generationKey).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(raw, 10, 64)
}

// Invalidate implements Cache.
func (r RedisCache) Invalidate(ctx context.Context) error {
	return r.Client.Incr(ctx, generationKey).Err()
}
