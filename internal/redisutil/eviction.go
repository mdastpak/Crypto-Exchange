package redisutil

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
)

// NoEviction is the only maxmemory-policy under which Redis never drops a
// key to make room. Every other policy, including the volatile-* ones, may
// drop the keys that security depends on: each of them has a TTL, so
// "volatile" does not spare them. When such a key is evicted, its check
// passes without any error. A logged-out administrator token is valid again,
// and the attempt counters of sign-in codes and the rate limits start over
// (#305).
const NoEviction = "noeviction"

// ErrEvictingPolicy is returned by CheckNoEviction when a primary may evict
// keys.
var ErrEvictingPolicy = errors.New("redis may evict keys")

// forEachPrimary runs fn on every primary that client writes to: each
// primary of a cluster, and otherwise the client itself, which in the
// sentinel mode talks to the current primary.
func forEachPrimary(ctx context.Context, client redis.UniversalClient, fn func(context.Context, *redis.Client) error) error {
	switch c := client.(type) {
	case *redis.ClusterClient:
		return c.ForEachMaster(ctx, fn)
	case *redis.Client:
		return fn(ctx, c)
	default:
		return fmt.Errorf("redis: cannot reach the primaries of a %T", client)
	}
}

// EvictionPolicies returns the maxmemory-policy of each primary, by its
// address. It fails when a primary refuses CONFIG GET, as some managed Redis
// services do.
func EvictionPolicies(ctx context.Context, client redis.UniversalClient) (map[string]string, error) {
	var mu sync.Mutex
	policies := map[string]string{}
	err := forEachPrimary(ctx, client, func(ctx context.Context, node *redis.Client) error {
		values, err := node.ConfigGet(ctx, "maxmemory-policy").Result()
		if err != nil {
			return fmt.Errorf("CONFIG GET maxmemory-policy on %s: %w", node.Options().Addr, err)
		}
		policy, ok := values["maxmemory-policy"]
		if !ok {
			return fmt.Errorf("CONFIG GET maxmemory-policy on %s returned no value", node.Options().Addr)
		}
		mu.Lock()
		policies[node.Options().Addr] = policy
		mu.Unlock()
		return nil
	})
	return policies, err
}

// CheckNoEviction returns nil when every primary has the policy noeviction,
// an error that wraps ErrEvictingPolicy and names each other policy when one
// does not, and the error of the read when the policy cannot be read.
func CheckNoEviction(ctx context.Context, client redis.UniversalClient) error {
	policies, err := EvictionPolicies(ctx, client)
	if err != nil {
		return err
	}
	var evicting []string
	for addr, policy := range policies {
		if policy != NoEviction {
			evicting = append(evicting, fmt.Sprintf("%s has %s", addr, policy))
		}
	}
	if len(evicting) > 0 {
		return fmt.Errorf("%w: %s; set maxmemory-policy %s", ErrEvictingPolicy, strings.Join(evicting, ", "), NoEviction)
	}
	return nil
}

// EvictedKeys returns the number of keys that the primaries have evicted
// since they started, from the evicted_keys field of INFO stats, summed over
// the primaries of a cluster.
func EvictedKeys(ctx context.Context, client redis.UniversalClient) (int64, error) {
	var mu sync.Mutex
	var total int64
	err := forEachPrimary(ctx, client, func(ctx context.Context, node *redis.Client) error {
		info, err := node.Info(ctx, "stats").Result()
		if err != nil {
			return fmt.Errorf("INFO stats on %s: %w", node.Options().Addr, err)
		}
		n, err := parseEvictedKeys(info)
		if err != nil {
			return fmt.Errorf("INFO stats on %s: %w", node.Options().Addr, err)
		}
		mu.Lock()
		total += n
		mu.Unlock()
		return nil
	})
	return total, err
}

// parseEvictedKeys reads evicted_keys from the text of INFO stats.
func parseEvictedKeys(info string) (int64, error) {
	for _, line := range strings.Split(info, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "evicted_keys:")
		if !ok {
			continue
		}
		return strconv.ParseInt(value, 10, 64)
	}
	return 0, errors.New("no evicted_keys field")
}

// EvictionWatch reports whether Redis has evicted keys since the watch
// started. The count of a primary only grows until it restarts, so a count
// above the one at the start means that keys were evicted, which the
// readiness probe reports as degraded.
type EvictionWatch struct {
	client redis.UniversalClient

	mu       sync.Mutex
	baseline int64
}

// ErrKeysEvicted is returned by EvictionWatch.Check after an eviction.
var ErrKeysEvicted = errors.New("redis evicted keys")

// NewEvictionWatch returns a watch that starts at the current count.
func NewEvictionWatch(ctx context.Context, client redis.UniversalClient) (*EvictionWatch, error) {
	n, err := EvictedKeys(ctx, client)
	if err != nil {
		return nil, err
	}
	return &EvictionWatch{client: client, baseline: n}, nil
}

// WatchOf returns a watch of client that starts at the count baseline,
// for tests that cannot make Redis evict.
func WatchOf(client redis.UniversalClient, baseline int64) *EvictionWatch {
	return &EvictionWatch{client: client, baseline: baseline}
}

// Check returns an error that wraps ErrKeysEvicted when the primaries have
// evicted keys since the watch started, and the error of the read when the
// count cannot be read. A count below the start, after a primary restarted
// or failed over, becomes the new start.
func (w *EvictionWatch) Check(ctx context.Context) error {
	n, err := EvictedKeys(ctx, w.client)
	if err != nil {
		return err
	}
	return w.compare(n)
}

// compare checks the count n against the start.
func (w *EvictionWatch) compare(n int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n < w.baseline {
		w.baseline = n
	}
	if n > w.baseline {
		return fmt.Errorf("%w: %d since this instance started", ErrKeysEvicted, n-w.baseline)
	}
	return nil
}
