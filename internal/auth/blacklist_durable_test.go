package auth

import (
	"context"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/redisutil"
	"github.com/mdastpak/crypto-exchange/internal/testutil/redistest"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTokenBlacklist_DurableWrites checks the blacklist of #231: a
// blacklist entry that a failover loses leaves a logged-out token to the
// session check alone. With durable
// writes on, a logout whose entry no replica acknowledged fails, and the
// client retries it; without them, the entry is written as before.
func TestTokenBlacklist_DurableWrites(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: redistest.Addr, DB: redistest.Auth})
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		t.Skipf("Redis is not reachable at %s: %v", redistest.Addr, err)
	}
	require.NoError(t, client.FlushDB(ctx).Err())
	t.Cleanup(func() {
		client.FlushDB(context.Background())
		client.Close()
	})

	tb := NewTokenBlacklist(client)
	assert.Zero(t, tb.Durability.Replicas, "the test configuration does not wait")
	require.NoError(t, tb.AddTokenToBlacklist(ctx, "token-a", time.Now().Add(time.Minute)))
	blacklisted, err := tb.IsTokenBlacklisted(ctx, "token-a")
	require.NoError(t, err)
	assert.True(t, blacklisted)

	// The test server has no replica, so a durable write cannot succeed.
	tb.Durability = redisutil.Durability{Replicas: 1, Timeout: 50 * time.Millisecond}
	err = tb.AddTokenToBlacklist(ctx, "token-b", time.Now().Add(time.Minute))
	require.ErrorIs(t, err, redisutil.ErrNotReplicated)
	ttl := client.TTL(ctx, tb.tokenKey("token-b")).Val()
	assert.Greater(t, ttl, 50*time.Second, "the entry was written with the token's lifetime; only its replication is missing")

	// An expired token is not written, and so not waited for.
	require.NoError(t, tb.AddTokenToBlacklist(ctx, "token-c", time.Now().Add(-time.Second)))
}
