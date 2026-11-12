package redisutil

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/dberr"
	"github.com/mdastpak/crypto-exchange/internal/testutil/redistest"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestRedis returns a client of an empty database of the test server,
// which has no replicas, and skips the test when it is not reachable.
func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: redistest.Addr, DB: redistest.RedisUtil})
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
	return client
}

// TestDurability_WithoutReplicasRunsDirectly checks the zero value: the
// write runs on the client, and nothing waits.
func TestDurability_WithoutReplicasRunsDirectly(t *testing.T) {
	client := newTestRedis(t)
	ctx := context.Background()
	var got redis.Cmdable
	err := Durability{}.Run(ctx, client, "k", func(c redis.Cmdable) error {
		got = c
		return c.Set(ctx, "k", "v", 0).Err()
	})
	require.NoError(t, err)
	assert.Same(t, client, got, "no dedicated connection without replicas")
	assert.Equal(t, "v", client.Get(ctx, "k").Val())
}

// TestDurability_RefusesAWriteThatNoReplicaHolds runs a durable write on a
// server without replicas: the write is made, but Run reports that it is
// not replicated, within the timeout, and notes the request as busy, so
// that the response is 503 with Retry-After.
func TestDurability_RefusesAWriteThatNoReplicaHolds(t *testing.T) {
	client := newTestRedis(t)
	ctx := dberr.Track(context.Background())
	durability := Durability{Replicas: 1, Timeout: 100 * time.Millisecond}

	var got redis.Cmdable
	started := time.Now()
	err := durability.Run(ctx, client, "counter", func(c redis.Cmdable) error {
		got = c
		return c.Incr(ctx, "counter").Err()
	})
	require.ErrorIs(t, err, ErrNotReplicated)
	assert.Contains(t, err.Error(), "0 of 1 within 100ms")
	assert.Less(t, time.Since(started), 2*time.Second, "WAIT returned at its timeout")
	assert.IsType(t, &redis.Conn{}, got, "the write and the WAIT share one connection")
	assert.Equal(t, "1", client.Get(ctx, "counter").Val(), "the write itself was made")
	assert.True(t, dberr.Busy(ctx))

	// WAIT for no replica returns at once: the waiting is what Replicas asks.
	require.NoError(t, Durability{Replicas: 0, Timeout: time.Hour}.Run(ctx, client, "counter", func(c redis.Cmdable) error {
		return c.Incr(ctx, "counter").Err()
	}))
}

// TestDurability_ReturnsTheWriteError checks that a failed write is
// returned as it is, without waiting and without noting the request.
func TestDurability_ReturnsTheWriteError(t *testing.T) {
	client := newTestRedis(t)
	ctx := dberr.Track(context.Background())
	boom := errors.New("boom")
	err := Durability{Replicas: 1, Timeout: time.Hour}.Run(ctx, client, "k", func(redis.Cmdable) error { return boom })
	assert.ErrorIs(t, err, boom)
	assert.False(t, dberr.Busy(ctx))
}

// TestDurability_RefusesAnotherClient checks that a client kind that has no
// primary to pin, such as a Ring, is refused instead of waiting on an
// arbitrary connection.
func TestDurability_RefusesAnotherClient(t *testing.T) {
	ring := redis.NewRing(&redis.RingOptions{Addrs: map[string]string{"a": redistest.Addr}})
	defer ring.Close()
	err := Durability{Replicas: 1, Timeout: time.Second}.Run(context.Background(), ring, "k", func(redis.Cmdable) error {
		t.Fatal("the write must not run")
		return nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "*redis.Ring")
}

func TestDurabilityOf(t *testing.T) {
	saved := config.Config.Redis.DurableWrites
	t.Cleanup(func() { config.Config.Redis.DurableWrites = saved })
	config.Config.Redis.DurableWrites.Replicas, config.Config.Redis.DurableWrites.TimeoutMs = 2, 300
	assert.Equal(t, Durability{Replicas: 2, Timeout: 300 * time.Millisecond}, DurabilityOf(&config.Config))
}
