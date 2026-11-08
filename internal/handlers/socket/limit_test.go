package socket

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/testutil/redistest"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testCounter returns a counter on the Redis database of this package,
// emptied, whose slots expire after ttl. It skips the test when Redis is
// not reachable.
func testCounter(t *testing.T, ttl time.Duration) *RedisConnectionCounter {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: redistest.Addr, DB: redistest.Socket})
	t.Cleanup(func() { client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("Redis is not reachable at %s: %v", redistest.Addr, err)
	}
	require.NoError(t, client.FlushDB(context.Background()).Err())
	return &RedisConnectionCounter{client: client, ttl: ttl}
}

// instance is a handler that shares counter, as one of several server
// instances, and the URL that serves it.
func instance(t *testing.T, counter ConnectionCounter) (*WebSocketHandler, string) {
	t.Helper()
	h := newTestHandler()
	h.Authenticate = fakeAuth
	h.Counter = counter
	h.limits.maxConnections = 100
	return h, gateway(t, h)
}

// refusal dials url and returns the status and error code that refuse the
// upgrade, or 101.
func refusal(t *testing.T, url string, header http.Header) (int, string) {
	t.Helper()
	_, resp, err := dial(t, url, header)
	require.NotNil(t, resp)
	if err == nil {
		return resp.StatusCode, ""
	}
	defer resp.Body.Close()
	var body struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body.Code
}

var bearerGood = http.Header{"Authorization": {"Bearer good"}}

func TestRedisConnectionCounter(t *testing.T) {
	counter := testCounter(t, 300*time.Millisecond)
	ctx := context.Background()
	slot := func(member string) Slot { return Slot{Key: ipCountKey("192.0.2.1"), Member: member} }

	for _, member := range []string{"a", "b"} {
		ok, err := counter.Acquire(ctx, slot(member), 2)
		require.NoError(t, err)
		require.True(t, ok)
	}
	ok, err := counter.Acquire(ctx, slot("c"), 2)
	require.NoError(t, err)
	assert.False(t, ok, "the third slot of a limit of two")
	ok, err = counter.Acquire(ctx, slot("a"), 2)
	require.NoError(t, err)
	assert.True(t, ok, "a slot that is taken again is not counted twice")

	require.NoError(t, counter.Release(ctx, []Slot{slot("a")}))
	ok, err = counter.Acquire(ctx, slot("c"), 2)
	require.NoError(t, err)
	assert.True(t, ok, "a released slot is free")

	// b is refreshed and c is not: c expires, b stays.
	for range 4 {
		time.Sleep(100 * time.Millisecond)
		require.NoError(t, counter.Refresh(ctx, []Slot{slot("b")}))
	}
	ok, err = counter.Acquire(ctx, slot("d"), 2)
	require.NoError(t, err)
	assert.True(t, ok, "the slot that was not refreshed expired")
	ok, err = counter.Acquire(ctx, slot("e"), 2)
	require.NoError(t, err)
	assert.False(t, ok, "the refreshed slot is still counted")

	// A refresh adds a slot that Redis lost, as after a restart.
	require.NoError(t, counter.client.FlushDB(ctx).Err())
	require.NoError(t, counter.Refresh(ctx, []Slot{slot("b"), slot("d")}))
	ok, err = counter.Acquire(ctx, slot("e"), 2)
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestServe_LimitsHoldAcrossInstances serves one gateway from two
// instances that share Redis (#234): one client address cannot exceed
// max_connections_per_ip in total, and one user cannot exceed
// max_connections_per_user over both instances.
func TestServe_LimitsHoldAcrossInstances(t *testing.T) {
	counter := testCounter(t, time.Minute)
	first, firstURL := instance(t, counter)
	second, secondURL := instance(t, counter)

	t.Run("per address", func(t *testing.T) {
		for _, h := range []*WebSocketHandler{first, second} {
			h.limits.maxPerIP, h.limits.maxPerUser = 3, 10
		}
		for _, url := range []string{firstURL, secondURL, secondURL} {
			status, _ := refusal(t, url, nil)
			require.Equal(t, http.StatusSwitchingProtocols, status)
		}
		for _, url := range []string{firstURL, secondURL} {
			status, code := refusal(t, url, nil)
			assert.Equal(t, http.StatusTooManyRequests, status, "the fourth connection of the address, on either instance")
			assert.Equal(t, "rate_limited", code)
		}
	})
	require.NoError(t, counter.client.FlushDB(context.Background()).Err())

	t.Run("per user", func(t *testing.T) {
		for _, h := range []*WebSocketHandler{first, second} {
			h.limits.maxPerIP, h.limits.maxPerUser = 100, 2
		}
		status, _ := refusal(t, firstURL, bearerGood)
		require.Equal(t, http.StatusSwitchingProtocols, status)
		status, _ = refusal(t, secondURL, bearerGood)
		require.Equal(t, http.StatusSwitchingProtocols, status)
		status, code := refusal(t, firstURL, bearerGood)
		assert.Equal(t, http.StatusTooManyRequests, status, "the third connection of the user")
		assert.Equal(t, CodeTooManyConnections, code)

		status, _ = refusal(t, secondURL, nil)
		assert.Equal(t, http.StatusSwitchingProtocols, status, "an anonymous connection does not count against the user")
	})
}

// TestServe_ClosingAConnectionFreesItsSlots checks that a connection that
// ends releases its places in Redis at once, for the other instances.
func TestServe_ClosingAConnectionFreesItsSlots(t *testing.T) {
	counter := testCounter(t, time.Minute)
	first, firstURL := instance(t, counter)
	_, secondURL := instance(t, counter)
	first.limits.maxPerUser = 1

	conn, _, err := dial(t, firstURL, bearerGood)
	require.NoError(t, err)
	onlyClient(t, first)
	status, _ := refusal(t, secondURL, bearerGood)
	require.Equal(t, http.StatusSwitchingProtocols, status, "the second instance has the default limit of ten")

	conn.Close()
	// The slots are freed right after the client leaves the list.
	assert.Eventually(t, func() bool {
		members, err := counter.client.ZCard(context.Background(), userCountKey("user-1")).Result()
		return err == nil && members == 1
	}, 2*time.Second, 5*time.Millisecond, "only the connection of the second instance is left")
	assert.Zero(t, first.clientCount())
}

// TestServe_CountsRecoverAfterAnInstanceIsKilled leaves the slots of an
// instance that was killed without shutting down: they stop being
// refreshed, expire, and the address can connect again, while the
// connections of a live instance keep their slots (#234).
func TestServe_CountsRecoverAfterAnInstanceIsKilled(t *testing.T) {
	const ttl = 400 * time.Millisecond
	counter := testCounter(t, ttl)
	live, url := instance(t, counter)
	live.limits.maxPerIP = 2
	live.refreshEvery = ttl / 4
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	live.StartConnectionHeartbeat(ctx)

	// The killed instance held both places of the address.
	for _, member := range []string{"killed:1", "killed:2"} {
		ok, err := counter.Acquire(context.Background(), Slot{Key: ipCountKey("127.0.0.1"), Member: member}, 2)
		require.NoError(t, err)
		require.True(t, ok)
	}
	status, _ := refusal(t, url, nil)
	require.Equal(t, http.StatusTooManyRequests, status)

	require.Eventually(t, func() bool {
		status, _ := refusal(t, url, nil)
		return status == http.StatusSwitchingProtocols
	}, 5*ttl, 20*time.Millisecond, "the slots of the killed instance expire")
	status, _ = refusal(t, url, nil)
	require.Equal(t, http.StatusSwitchingProtocols, status)

	// The live connections are refreshed, so they stay counted for longer
	// than the TTL.
	time.Sleep(3 * ttl)
	status, _ = refusal(t, url, nil)
	assert.Equal(t, http.StatusTooManyRequests, status, "the live connections still hold the places")
}

// TestServe_FallsBackToLocalCountsWithoutRedis checks that an upgrade
// does not fail when Redis cannot be reached: each instance then counts
// its own connections.
func TestServe_FallsBackToLocalCountsWithoutRedis(t *testing.T) {
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 100 * time.Millisecond})
	t.Cleanup(func() { down.Close() })
	h, url := instance(t, NewRedisConnectionCounter(down))
	h.limits.maxPerIP, h.limits.maxPerUser = 3, 1

	status, _ := refusal(t, url, bearerGood)
	require.Equal(t, http.StatusSwitchingProtocols, status)
	status, code := refusal(t, url, bearerGood)
	assert.Equal(t, http.StatusTooManyRequests, status, "the local count of the user")
	assert.Equal(t, CodeTooManyConnections, code)
	status, _ = refusal(t, url, nil)
	require.Equal(t, http.StatusSwitchingProtocols, status)
	status, _ = refusal(t, url, nil)
	require.Equal(t, http.StatusSwitchingProtocols, status)
	status, _ = refusal(t, url, nil)
	assert.Equal(t, http.StatusTooManyRequests, status, "the local count of the address")
}

// TestReauth_CountsTheUserOfAnAnonymousConnection signs in an anonymous
// connection with a reauth while its user is at the limit: the connection
// is closed with 4429 (#234).
func TestReauth_CountsTheUserOfAnAnonymousConnection(t *testing.T) {
	counter := testCounter(t, time.Minute)
	first, firstURL := instance(t, counter)
	second, secondURL := instance(t, counter)
	for _, h := range []*WebSocketHandler{first, second} {
		h.Authenticate = expiringAuth
		h.limits.maxPerUser = 1
	}
	_, _, err := dial(t, firstURL, http.Header{"Authorization": {"Bearer user-1|1h"}})
	require.NoError(t, err)

	conn, _, err := dial(t, secondURL, nil)
	require.NoError(t, err)
	require.NoError(t, conn.WriteJSON(map[string]string{"event": "reauth", "token": "user-1|1h"}))
	code, reason := expectClose(t, conn)
	assert.Equal(t, CloseTooManyConnections, code)
	assert.Equal(t, CodeTooManyConnections, reason)
	require.Eventually(t, func() bool { return second.clientCount() == 0 }, 2*time.Second, 5*time.Millisecond)
	second.Mu.RLock()
	assert.Empty(t, second.openPerUser, "the refused user is not left counted")
	second.Mu.RUnlock()

	// Another user signs in on an anonymous connection and is counted.
	conn, _, err = dial(t, secondURL, nil)
	require.NoError(t, err)
	require.NoError(t, conn.WriteJSON(map[string]string{"event": "reauth", "token": "user-2|1h"}))
	readEvent(t, conn, "reauth")
	status, code2 := refusal(t, firstURL, http.Header{"Authorization": {"Bearer user-2|1h"}})
	assert.Equal(t, http.StatusTooManyRequests, status)
	assert.Equal(t, CodeTooManyConnections, code2)
}
