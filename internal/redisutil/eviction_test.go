package redisutil

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setPolicy sets maxmemory-policy on the test server until the test ends.
// Without a maxmemory limit, which the test server never has, no policy
// evicts anything, so the other packages that share the server are not
// affected while it is changed.
func setPolicy(t *testing.T, client *redis.Client, policy string) {
	t.Helper()
	ctx := context.Background()
	before, err := client.ConfigGet(ctx, "maxmemory-policy").Result()
	require.NoError(t, err)
	require.NoError(t, client.ConfigSet(ctx, "maxmemory-policy", policy).Err())
	t.Cleanup(func() {
		client.ConfigSet(context.Background(), "maxmemory-policy", before["maxmemory-policy"])
	})
}

// TestCheckNoEviction_AcceptsNoEviction checks the policy that keeps every
// key.
func TestCheckNoEviction_AcceptsNoEviction(t *testing.T) {
	client := newTestRedis(t)
	setPolicy(t, client, NoEviction)

	require.NoError(t, CheckNoEviction(context.Background(), client))
	policies, err := EvictionPolicies(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{client.Options().Addr: NoEviction}, policies)
}

// TestCheckNoEviction_RefusesEveryEvictingPolicy checks that every other
// policy is refused, including the volatile ones: every security key has a
// TTL, so they would evict it too.
func TestCheckNoEviction_RefusesEveryEvictingPolicy(t *testing.T) {
	client := newTestRedis(t)
	for _, policy := range []string{"allkeys-lru", "allkeys-lfu", "allkeys-random", "volatile-lru", "volatile-lfu", "volatile-random", "volatile-ttl"} {
		t.Run(policy, func(t *testing.T) {
			setPolicy(t, client, policy)
			err := CheckNoEviction(context.Background(), client)
			require.ErrorIs(t, err, ErrEvictingPolicy)
			assert.Contains(t, err.Error(), policy)
		})
	}
}

// TestEvictionWatch_QuietWithoutEvictions checks that a watch reports
// nothing while no key is evicted.
func TestEvictionWatch_QuietWithoutEvictions(t *testing.T) {
	client := newTestRedis(t)
	ctx := context.Background()
	watch, err := NewEvictionWatch(ctx, client)
	require.NoError(t, err)
	require.NoError(t, client.Set(ctx, "eviction:key", "1", 0).Err())
	assert.NoError(t, watch.Check(ctx))
}

// TestEvictionWatch_ReportsNewEvictions checks the comparison with the
// count at the start, and that a lower count, after a restart or a
// failover, becomes the new start.
func TestEvictionWatch_ReportsNewEvictions(t *testing.T) {
	watch := &EvictionWatch{baseline: 5}
	assert.NoError(t, watch.compare(5))
	assert.ErrorIs(t, watch.compare(7), ErrKeysEvicted)
	assert.NoError(t, watch.compare(2), "a restarted primary counts from zero")
	assert.ErrorIs(t, watch.compare(3), ErrKeysEvicted)
}

// TestParseEvictedKeys reads the field from the text of INFO stats.
func TestParseEvictedKeys(t *testing.T) {
	n, err := parseEvictedKeys("# Stats\r\ntotal_connections_received:3\r\nevicted_keys:42\r\nexpired_keys:1\r\n")
	require.NoError(t, err)
	assert.Equal(t, int64(42), n)

	_, err = parseEvictedKeys("# Stats\r\nexpired_keys:1\r\n")
	assert.Error(t, err)
}

// TestCheckNoEviction_UnreadableIsAnotherError checks that a policy that
// cannot be read is not mistaken for an evicting one, so that the server can
// tell the operator which of the two to fix.
func TestCheckNoEviction_UnreadableIsAnotherError(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	t.Cleanup(func() { client.Close() })
	err := CheckNoEviction(context.Background(), client)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrEvictingPolicy)
}
