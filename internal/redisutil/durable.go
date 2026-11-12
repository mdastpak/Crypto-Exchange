// Package redisutil holds what the stores that keep state in Redis share
// across the deployment modes of #231: writes that a failover cannot lose,
// and the hash slots that Redis Cluster places keys in.
package redisutil

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/dberr"

	"github.com/redis/go-redis/v9"
)

// ErrNotReplicated reports a durable write that fewer replicas than
// required acknowledged in time. The write itself was made on the primary:
// a counter was counted, a token blacklisted. Only the guarantee that a
// promoted replica also holds it is missing, so the caller fails the
// request instead of relying on it, and a retry repeats a write that is
// safe to repeat.
var ErrNotReplicated = errors.New("redis: the write was not acknowledged by enough replicas")

// Durability says how many replicas must hold a write before it counts
// (redis.durable_writes). Replication in Redis is asynchronous: a write
// that the primary acknowledged can be missing on the replica that a
// failover promotes. For a security counter, such as the attempts at an
// OTP code, a lost write gives the attempts back to whoever made them. WAIT
// after the write closes that gap: once n replicas hold it, a failover
// that promotes one of them keeps it.
//
// The zero value does not wait, which is the behavior of a deployment
// without replicas.
type Durability struct {
	// Replicas is how many replicas must acknowledge the write; 0 does not
	// wait.
	Replicas int
	// Timeout bounds the wait. WAIT returns after it with the replicas that
	// acknowledged so far.
	Timeout time.Duration
}

// Run runs fn, which writes key and possibly other keys of its hash slot,
// and then waits for the replicas. fn runs on one connection to the
// primary that holds key, because WAIT waits only for the writes of its own
// connection: on a pooled client, the write and the WAIT could otherwise
// run on two connections, or in Redis Cluster on two nodes.
//
// Run returns the error of fn, an error of the wait, or ErrNotReplicated.
// With Replicas at 0, fn runs on client itself.
func (d Durability) Run(ctx context.Context, client redis.UniversalClient, key string, fn func(redis.Cmdable) error) error {
	if d.Replicas <= 0 {
		return fn(client)
	}
	node, err := primaryFor(ctx, client, key)
	if err != nil {
		return err
	}
	conn := node.Conn()
	defer conn.Close()
	// The connection bypasses the cluster client, and with it the hook of
	// FollowClusterFailovers, so a dead primary is reported here.
	if err := fn(conn); err != nil {
		reloadOnNetworkError(ctx, client, err)
		return err
	}
	acknowledged, err := conn.Wait(ctx, d.Replicas, d.Timeout).Result()
	if err != nil {
		reloadOnNetworkError(ctx, client, err)
		return fmt.Errorf("wait for the replicas: %w", err)
	}
	if acknowledged < int64(d.Replicas) {
		// The response helpers answer the request with 503 and
		// Retry-After, as for a busy database: a replica that lags or
		// restarts is a moment, not a bug.
		dberr.MarkBusy(ctx)
		return fmt.Errorf("%w: %d of %d within %v", ErrNotReplicated, acknowledged, d.Replicas, d.Timeout)
	}
	return nil
}

// primaryFor returns the client of the primary that holds key: the node of
// its hash slot in Redis Cluster, and otherwise the client itself, which in
// the sentinel mode always talks to the current primary.
func primaryFor(ctx context.Context, client redis.UniversalClient, key string) (*redis.Client, error) {
	switch c := client.(type) {
	case *redis.ClusterClient:
		node, err := c.MasterForKey(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("find the primary of %s: %w", key, err)
		}
		return node, nil
	case *redis.Client:
		return c, nil
	default:
		return nil, fmt.Errorf("durable writes need a *redis.Client or a *redis.ClusterClient, not %T", client)
	}
}

// DurabilityOf returns the durability of redis.durable_writes of cfg.
func DurabilityOf(c *config.Configuration) Durability {
	cfg := c.Redis.DurableWrites
	return Durability{Replicas: cfg.Replicas, Timeout: time.Duration(cfg.TimeoutMs) * time.Millisecond}
}
