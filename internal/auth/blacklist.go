package auth

import (
	"context"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/redisutil"

	"github.com/redis/go-redis/v9"
)

// TokenBlacklist keeps the access tokens that were logged out until they
// expire. The session checks, of RequireActiveSession for a user token and
// of the admin handler for an administrator's, reject such a token too, from
// the database; the blacklist is a second check.
type TokenBlacklist struct {
	RedisClient redis.UniversalClient
	Prefix      string
	// Durability is how many replicas must hold a blacklisted token before
	// the logout succeeds (redis.durable_writes, #231), so that a failover
	// cannot bring a logged-out administrator token back. The zero value
	// does not wait.
	Durability redisutil.Durability
}

// NewTokenBlacklist returns a blacklist on client with the durability of
// redis.durable_writes.
func NewTokenBlacklist(client redis.UniversalClient) *TokenBlacklist {
	return &TokenBlacklist{
		RedisClient: client,
		Prefix:      "blacklist:token:",
		Durability:  redisutil.DurabilityFromConfig(),
	}
}

func (tb *TokenBlacklist) tokenKey(token string) string {
	return tb.Prefix + HashToken(token)
}

func (tb *TokenBlacklist) AddTokenToBlacklist(ctx context.Context, token string, expiresAt time.Time) error {
	if tb.RedisClient == nil {
		return nil // No-op if no Redis client
	}
	ttl := time.Until(expiresAt)
	if ttl <= 0 {
		return nil
	}
	key := tb.tokenKey(token)
	return tb.Durability.Run(ctx, tb.RedisClient, key, func(c redis.Cmdable) error {
		return c.Set(ctx, key, "1", ttl).Err()
	})
}

func (tb *TokenBlacklist) IsTokenBlacklisted(ctx context.Context, token string) (bool, error) {
	if tb.RedisClient == nil {
		return false, nil // No-op if no Redis client
	}
	key := tb.tokenKey(token)
	val, err := tb.RedisClient.Get(ctx, key).Result()
	if err == redis.Nil {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return val == "1", nil
}
