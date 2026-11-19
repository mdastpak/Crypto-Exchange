package routing

import (
	"context"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// QuoteState returns a copy of the router that keeps what it reads of the
// venues, the inventory, and the usage for ttl on this node, shared by
// every request (#509). Its routes are for quotes: a quote is priced on a
// state at most ttl old, and the execution plans again with the router
// itself on the state of now, under its locks, so a quote that the state
// no longer routes is refused as quote_not_routable. Keep ttl well under
// venues.state.max_age_seconds. A failed read is never kept, and
// concurrent requests for one key wait for one read.
func (r *Router) QuoteState(ttl time.Duration) *Router {
	c := *r
	if ttl <= 0 {
		return &c
	}
	if r.Venues != nil {
		c.Venues = &cachedVenues{next: r.Venues, markets: newTTL[[3]string, VenueMarket](ttl), funds: newTTL[[3]string, Funds](ttl)}
	}
	if r.Inventory != nil {
		c.Inventory = &cachedInventory{next: r.Inventory, seen: newTTL[[2]string, memoCapacity](ttl)}
	}
	if r.Usage != nil {
		c.Usage = &cachedUsage{next: r.Usage, seen: newTTL[memoDay, decimal.Decimal](ttl)}
	}
	return &c
}

// ttlCache keeps values for a ttl; a key being read is read once, and the
// others wait for it.
type ttlCache[K comparable, V any] struct {
	ttl  time.Duration
	now  func() time.Time
	mu   sync.Mutex
	vals map[K]*ttlEntry[V]
}

type ttlEntry[V any] struct {
	done chan struct{}
	at   time.Time
	val  V
	err  error
}

func newTTL[K comparable, V any](ttl time.Duration) *ttlCache[K, V] {
	return &ttlCache[K, V]{ttl: ttl, now: time.Now, vals: map[K]*ttlEntry[V]{}}
}

func (c *ttlCache[K, V]) get(ctx context.Context, key K, read func() (V, error)) (V, error) {
	for {
		c.mu.Lock()
		e, ok := c.vals[key]
		if ok {
			select {
			case <-e.done:
				if e.err == nil && c.now().Sub(e.at) < c.ttl {
					c.mu.Unlock()
					return e.val, nil
				}
				ok = false
			default:
			}
		}
		if !ok {
			e = &ttlEntry[V]{done: make(chan struct{})}
			c.vals[key] = e
			c.mu.Unlock()
			e.val, e.err = read()
			e.at = c.now()
			c.mu.Lock()
			if e.err != nil && c.vals[key] == e {
				delete(c.vals, key)
			}
			close(e.done)
			c.mu.Unlock()
			return e.val, e.err
		}
		c.mu.Unlock()
		select {
		case <-e.done:
			// The read ended: take its value, or read again after a
			// failure.
		case <-ctx.Done():
			var zero V
			return zero, ctx.Err()
		}
	}
}

type cachedVenues struct {
	next    Venues
	markets *ttlCache[[3]string, VenueMarket]
	funds   *ttlCache[[3]string, Funds]
}

func (v *cachedVenues) Market(ctx context.Context, venue, base, quote string) (VenueMarket, error) {
	return v.markets.get(ctx, [3]string{venue, base, quote}, func() (VenueMarket, error) {
		return v.next.Market(ctx, venue, base, quote)
	})
}

func (v *cachedVenues) Funds(ctx context.Context, venue, currency, network string) (Funds, error) {
	return v.funds.get(ctx, [3]string{venue, currency, network}, func() (Funds, error) {
		return v.next.Funds(ctx, venue, currency, network)
	})
}

type cachedInventory struct {
	next Inventory
	seen *ttlCache[[2]string, memoCapacity]
}

func (i *cachedInventory) Capacity(ctx context.Context, currencyHK, symbol string) (decimal.Decimal, bool, error) {
	got, err := i.seen.get(ctx, [2]string{currencyHK, symbol}, func() (memoCapacity, error) {
		c, bounded, err := i.next.Capacity(ctx, currencyHK, symbol)
		return memoCapacity{capacity: c, bounded: bounded}, err
	})
	return got.capacity, got.bounded, err
}

type cachedUsage struct {
	next Usage
	seen *ttlCache[memoDay, decimal.Decimal]
}

func (u *cachedUsage) DailyNotional(ctx context.Context, venue, currencyHK string, day time.Time) (decimal.Decimal, error) {
	return u.seen.get(ctx, memoDay{venue, currencyHK, day.UTC()}, func() (decimal.Decimal, error) {
		return u.next.DailyNotional(ctx, venue, currencyHK, day)
	})
}
