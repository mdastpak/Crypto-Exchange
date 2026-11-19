package routing

import (
	"context"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// Memoized returns a copy of the router that reads the state of each
// venue, the capacity of the inventory, and the usage of each venue at
// most once (#509). A quote runs the router up to four times, for
// quantities that differ only by the cost of the route, against one state;
// the copy reads that state once.
//
// The copy serves one request and is then dropped: it never serves two
// requests, so that a request never plans on the state that another read.
// An execution plans with the router itself, on the state of now.
func (r *Router) Memoized() *Router {
	c := *r
	if r.Venues != nil {
		c.Venues = &memoVenues{next: r.Venues, markets: map[[3]string]memoMarket{}, funds: map[[3]string]memoFunds{}}
	}
	if r.Inventory != nil {
		c.Inventory = &memoInventory{next: r.Inventory, seen: map[[2]string]memoCapacity{}}
	}
	if r.Usage != nil {
		c.Usage = &memoUsage{next: r.Usage, seen: map[memoDay]memoNotional{}}
	}
	return &c
}

type memoMarket struct {
	m   VenueMarket
	err error
}

type memoFunds struct {
	f   Funds
	err error
}

type memoVenues struct {
	next    Venues
	mu      sync.Mutex
	markets map[[3]string]memoMarket
	funds   map[[3]string]memoFunds
}

func (v *memoVenues) Market(ctx context.Context, venue, base, quote string) (VenueMarket, error) {
	key := [3]string{venue, base, quote}
	v.mu.Lock()
	defer v.mu.Unlock()
	if got, ok := v.markets[key]; ok {
		return got.m, got.err
	}
	m, err := v.next.Market(ctx, venue, base, quote)
	v.markets[key] = memoMarket{m, err}
	return m, err
}

func (v *memoVenues) Funds(ctx context.Context, venue, currency, network string) (Funds, error) {
	key := [3]string{venue, currency, network}
	v.mu.Lock()
	defer v.mu.Unlock()
	if got, ok := v.funds[key]; ok {
		return got.f, got.err
	}
	f, err := v.next.Funds(ctx, venue, currency, network)
	v.funds[key] = memoFunds{f, err}
	return f, err
}

type memoCapacity struct {
	capacity decimal.Decimal
	bounded  bool
	err      error
}

type memoInventory struct {
	next Inventory
	mu   sync.Mutex
	seen map[[2]string]memoCapacity
}

func (i *memoInventory) Capacity(ctx context.Context, currencyHK, symbol string) (decimal.Decimal, bool, error) {
	key := [2]string{currencyHK, symbol}
	i.mu.Lock()
	defer i.mu.Unlock()
	if got, ok := i.seen[key]; ok {
		return got.capacity, got.bounded, got.err
	}
	c, bounded, err := i.next.Capacity(ctx, currencyHK, symbol)
	i.seen[key] = memoCapacity{c, bounded, err}
	return c, bounded, err
}

type memoDay struct {
	venue, currencyHK string
	day               time.Time
}

type memoNotional struct {
	notional decimal.Decimal
	err      error
}

type memoUsage struct {
	next Usage
	mu   sync.Mutex
	seen map[memoDay]memoNotional
}

func (u *memoUsage) DailyNotional(ctx context.Context, venue, currencyHK string, day time.Time) (decimal.Decimal, error) {
	key := memoDay{venue, currencyHK, day.UTC()}
	u.mu.Lock()
	defer u.mu.Unlock()
	if got, ok := u.seen[key]; ok {
		return got.notional, got.err
	}
	n, err := u.next.DailyNotional(ctx, venue, currencyHK, day)
	u.seen[key] = memoNotional{n, err}
	return n, err
}
