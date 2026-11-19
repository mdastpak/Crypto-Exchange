package routing

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQuoteState_SharesTheStateForItsTTL covers #509: the router of the
// quotes reads each venue once for many requests within its ttl, reads
// again after it, never keeps a failed read, and lets concurrent requests
// wait for one read; the router itself reads every time.
func TestQuoteState_SharesTheStateForItsTTL(t *testing.T) {
	ctx := context.Background()
	v, inv := world()
	c := &counting{venues: v, inventory: inv, usage: fakeUsage{}}
	rules := []Rule{{Name: "all", UseInventory: true,
		Venues: []VenueRule{{Venue: "binance", MaxDailyNotional: "1000000"}, {Venue: "nobitex"}}}}
	require.NoError(t, Validate(rules, nil))
	r := &Router{Rules: fixedRules{RuleSet{Version: 1, Rules: rules}}, Venues: c, Inventory: c, Usage: c}
	_, err := r.Plan(ctx, input("buy", "2.5"))
	require.NoError(t, err)
	once := *c

	q := r.QuoteState(time.Second)
	clock := time.Now()
	for _, cache := range []interface{ setNow(func() time.Time) }{
		q.Venues.(*cachedVenues).markets, q.Venues.(*cachedVenues).funds,
		q.Inventory.(*cachedInventory).seen, q.Usage.(*cachedUsage).seen,
	} {
		cache.setNow(func() time.Time { return clock })
	}
	for range 5 {
		_, err := q.Memoized().Plan(ctx, input("buy", "2.5"))
		require.NoError(t, err)
	}
	assert.Equal(t, 2*once.funds, c.funds, "five quotes within the ttl read the venues once")
	assert.Equal(t, 2*once.capacity, c.capacity)
	assert.Equal(t, 2*once.daily, c.daily)
	clock = clock.Add(1100 * time.Millisecond)
	_, err = q.Plan(ctx, input("buy", "2.5"))
	require.NoError(t, err)
	assert.Equal(t, 3*once.funds, c.funds, "after the ttl the state is read again")
	_, err = r.Plan(ctx, input("buy", "2.5"))
	require.NoError(t, err)
	assert.Equal(t, 4*once.funds, c.funds, "the router of the executions reads every time")
}

func (c *ttlCache[K, V]) setNow(now func() time.Time) { c.now = now }

// TestTTLCache_ReadsOnceAndKeepsNoFailure covers the cache under
// concurrent requests and failed reads.
func TestTTLCache_ReadsOnceAndKeepsNoFailure(t *testing.T) {
	ctx := context.Background()
	c := newTTL[string, int](time.Minute)
	var reads atomic.Int64
	release := make(chan struct{})
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			v, err := c.get(ctx, "k", func() (int, error) {
				reads.Add(1)
				<-release
				return 7, nil
			})
			assert.NoError(t, err)
			assert.Equal(t, 7, v)
		})
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	assert.Equal(t, int64(1), reads.Load(), "concurrent requests wait for one read")

	failed := errors.New("down")
	_, err := c.get(ctx, "f", func() (int, error) { return 0, failed })
	require.ErrorIs(t, err, failed)
	v, err := c.get(ctx, "f", func() (int, error) { return 3, nil })
	require.NoError(t, err)
	assert.Equal(t, 3, v, "a failed read is not kept")
}
