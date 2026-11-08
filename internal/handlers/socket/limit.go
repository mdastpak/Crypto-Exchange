package socket

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// The connection limits of websocket.max_connections_per_ip and
// websocket.max_connections_per_user hold across all server instances
// (#234): each connection takes a slot in Redis, under its client address
// and, when it is authenticated, under its user. Each instance also counts
// its own connections in memory, which bounds them when Redis cannot be
// reached, as the rate limiters fall back to a local decision (#119).
//
// A slot expires unless its instance refreshes it, so that the slots of an
// instance that was killed without shutting down are freed within
// slotTTL, and the limits recover by themselves.

// CodeTooManyConnections is the error code of an upgrade, or a reauth,
// refused because the user already has websocket.max_connections_per_user
// connections.
const CodeTooManyConnections = "too_many_connections"

// CloseTooManyConnections ends a connection that became authenticated
// with a reauth while its user already had
// websocket.max_connections_per_user connections. It mirrors HTTP 429.
const CloseTooManyConnections = 4429

const (
	// slotTTL is how long a slot outlives its last refresh.
	slotTTL = 60 * time.Second
	// slotRefresh is how often each instance refreshes the slots of its
	// connections: three times per slotTTL, so that one missed refresh
	// does not free a slot that is still in use.
	slotRefresh = slotTTL / 3
	// sharedCountTimeout bounds one call to Redis during an upgrade. When
	// Redis does not answer in time, the local counts decide.
	sharedCountTimeout = 500 * time.Millisecond
)

// Slot is the place of one connection in one shared count.
type Slot struct {
	Key    string
	Member string
}

// ConnectionCounter counts the open connections of every instance.
type ConnectionCounter interface {
	// Acquire takes a slot for member under key, unless key already holds
	// limit slots that have not expired. It reports whether it took one.
	Acquire(ctx context.Context, slot Slot, limit int) (bool, error)
	// Refresh extends the slots of open connections by the TTL.
	Refresh(ctx context.Context, slots []Slot) error
	// Release frees slots.
	Release(ctx context.Context, slots []Slot) error
}

// RedisConnectionCounter keeps each count in a sorted set, whose members
// are the slots and whose scores are the times at which they expire, in
// Redis's clock, so that the clocks of the instances do not matter. Every
// script touches one key, so it works with Redis Cluster.
type RedisConnectionCounter struct {
	client redis.UniversalClient
	ttl    time.Duration
}

// NewRedisConnectionCounter returns a counter on client whose slots expire
// after slotTTL without a refresh.
func NewRedisConnectionCounter(client redis.UniversalClient) *RedisConnectionCounter {
	return &RedisConnectionCounter{client: client, ttl: slotTTL}
}

// acquireScript drops the expired slots of KEYS[1] and takes ARGV[1] when
// fewer than ARGV[2] remain, for ARGV[3] milliseconds. A slot that is
// already taken is extended, so that a retried acquire does not count
// twice.
var acquireScript = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local ttl = tonumber(ARGV[3])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
if not redis.call('ZSCORE', KEYS[1], ARGV[1]) and redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[2]) then
  return 0
end
redis.call('ZADD', KEYS[1], now + ttl, ARGV[1])
redis.call('PEXPIRE', KEYS[1], ttl)
return 1
`)

// refreshScript extends the slot ARGV[1] of KEYS[1] by ARGV[2]
// milliseconds. It adds the slot when it is missing, as after a restart of
// Redis, so that the count includes the connection again.
const refreshScript = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local ttl = tonumber(ARGV[2])
redis.call('ZADD', KEYS[1], now + ttl, ARGV[1])
if redis.call('PTTL', KEYS[1]) < ttl then
  redis.call('PEXPIRE', KEYS[1], ttl)
end
return 1
`

func (c *RedisConnectionCounter) Acquire(ctx context.Context, slot Slot, limit int) (bool, error) {
	taken, err := acquireScript.Run(ctx, c.client, []string{slot.Key}, slot.Member, limit, c.ttl.Milliseconds()).Int()
	return taken == 1, err
}

func (c *RedisConnectionCounter) Refresh(ctx context.Context, slots []Slot) error {
	if len(slots) == 0 {
		return nil
	}
	// EVAL rather than EVALSHA: a pipeline cannot load a missing script.
	_, err := c.client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for _, slot := range slots {
			pipe.Eval(ctx, refreshScript, []string{slot.Key}, slot.Member, c.ttl.Milliseconds())
		}
		return nil
	})
	return err
}

func (c *RedisConnectionCounter) Release(ctx context.Context, slots []Slot) error {
	if len(slots) == 0 {
		return nil
	}
	_, err := c.client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for _, slot := range slots {
			pipe.ZRem(ctx, slot.Key, slot.Member)
		}
		return nil
	})
	return err
}

// ipCountKey and userCountKey name the shared counts.
func ipCountKey(ip string) string       { return "ws:connections:ip:" + ip }
func userCountKey(userID string) string { return "ws:connections:user:" + userID }

// newMember names the slots of a new connection: the instance and the
// connection, so that no two connections share one.
func (h *WebSocketHandler) newMember() string {
	return h.instanceID + ":" + uuid.NewString()
}

// reserveShared takes the slots of a new connection from ip, and of
// userID when it is not empty. It returns the slots it took, or the status,
// error code, and message to refuse the connection with. When Redis fails,
// it logs it and lets the connection through with the slots taken so far:
// the local counts, which reserve already checked, then bound the
// connections of this instance.
func (h *WebSocketHandler) reserveShared(ctx context.Context, ip, userID string) ([]Slot, int, string, string) {
	if h.Counter == nil {
		return nil, 0, "", ""
	}
	member := h.newMember()
	ipSlot := Slot{Key: ipCountKey(ip), Member: member}
	ok, err := h.acquire(ctx, ipSlot, h.limits.maxPerIP)
	if err != nil {
		return nil, 0, "", ""
	}
	if !ok {
		return nil, http.StatusTooManyRequests, "rate_limited", "Too many connections from this address"
	}
	slots := []Slot{ipSlot}
	if userID == "" {
		return slots, 0, "", ""
	}
	userSlot := Slot{Key: userCountKey(userID), Member: member}
	ok, err = h.acquire(ctx, userSlot, h.limits.maxPerUser)
	if err != nil {
		return slots, 0, "", ""
	}
	if !ok {
		h.releaseShared(slots)
		return nil, http.StatusTooManyRequests, CodeTooManyConnections, "Too many connections for this user"
	}
	return append(slots, userSlot), 0, "", ""
}

// acquire takes one slot within sharedCountTimeout, and logs a failure.
func (h *WebSocketHandler) acquire(ctx context.Context, slot Slot, limit int) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, sharedCountTimeout)
	defer cancel()
	ok, err := h.Counter.Acquire(ctx, slot, limit)
	if err != nil {
		log.Printf("WebSocket: Redis cannot count the connections, counting this instance's only: %v", err)
	}
	return ok, err
}

// releaseShared frees slots, logging a failure: the slots then expire by
// themselves.
func (h *WebSocketHandler) releaseShared(slots []Slot) {
	if h.Counter == nil || len(slots) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sharedCountTimeout)
	defer cancel()
	if err := h.Counter.Release(ctx, slots); err != nil {
		log.Printf("WebSocket: could not free connection slots, they expire within %s: %v", slotTTL, err)
	}
}

// countUser counts an anonymous connection that a reauth authenticates
// under its new user, locally and in Redis. It reports false when the user
// already has websocket.max_connections_per_user connections.
func (h *WebSocketHandler) countUser(client *Client, userID string) bool {
	h.Mu.Lock()
	if h.openPerUser[userID] >= h.limits.maxPerUser {
		h.Mu.Unlock()
		return false
	}
	h.openPerUser[userID]++
	h.Mu.Unlock()

	client.mu.Lock()
	client.countedUser = userID
	client.mu.Unlock()
	if h.Counter == nil {
		return true
	}
	slot := Slot{Key: userCountKey(userID), Member: h.newMember()}
	ok, err := h.acquire(context.Background(), slot, h.limits.maxPerUser)
	if err != nil {
		return true
	}
	if !ok {
		// The connection closes; unregisterClient releases the local count.
		return false
	}
	client.mu.Lock()
	client.slots = append(client.slots, slot)
	client.mu.Unlock()
	return true
}

// takeSlots returns the slots of c and forgets them, once it has
// disconnected.
func (c *Client) takeSlots() []Slot {
	c.mu.Lock()
	defer c.mu.Unlock()
	slots := c.slots
	c.slots = nil
	return slots
}

// StartConnectionHeartbeat refreshes the slots of the open connections
// every slotRefresh, until ctx ends or the handler shuts down. Without a
// Counter it does nothing.
func (h *WebSocketHandler) StartConnectionHeartbeat(ctx context.Context) {
	if h.Counter == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(h.slotRefresh())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-h.ShutdownCh:
				return
			case <-ticker.C:
				h.refreshSlots(ctx)
			}
		}
	}()
}

// slotRefresh is slotRefresh, or the interval that a test sets.
func (h *WebSocketHandler) slotRefresh() time.Duration {
	if h.refreshEvery > 0 {
		return h.refreshEvery
	}
	return slotRefresh
}

// refreshSlots refreshes the slots of every open connection at once.
func (h *WebSocketHandler) refreshSlots(ctx context.Context) {
	var slots []Slot
	h.Mu.RLock()
	for client := range h.Clients {
		client.mu.Lock()
		slots = append(slots, client.slots...)
		client.mu.Unlock()
	}
	h.Mu.RUnlock()
	if len(slots) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := h.Counter.Refresh(ctx, slots); err != nil {
		log.Printf("WebSocket: could not refresh %d connection slots: %v", len(slots), err)
	}
}
