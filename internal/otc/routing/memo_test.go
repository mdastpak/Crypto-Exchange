package routing

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// counting counts the reads of the venues, the inventory, and the usage.
type counting struct {
	venues                          fakeVenues
	inventory                       fakeInventory
	usage                           fakeUsage
	markets, funds, capacity, daily int
}

func (c *counting) Market(ctx context.Context, venue, base, quote string) (VenueMarket, error) {
	c.markets++
	return c.venues.Market(ctx, venue, base, quote)
}

func (c *counting) Funds(ctx context.Context, venue, currency, network string) (Funds, error) {
	c.funds++
	return c.venues.Funds(ctx, venue, currency, network)
}

func (c *counting) Capacity(ctx context.Context, hk, symbol string) (decimal.Decimal, bool, error) {
	c.capacity++
	return c.inventory.Capacity(ctx, hk, symbol)
}

func (c *counting) DailyNotional(ctx context.Context, venue, hk string, day time.Time) (decimal.Decimal, error) {
	c.daily++
	return c.usage.DailyNotional(ctx, venue, hk, day)
}

// TestMemoized_ReadsTheStateOnce covers #509: the plans of one memoized
// router, for several quantities, read each venue, the inventory, and the
// usage once, and route as the router does; the router itself reads
// again.
func TestMemoized_ReadsTheStateOnce(t *testing.T) {
	ctx := context.Background()
	v, inv := world()
	c := &counting{venues: v, inventory: inv, usage: fakeUsage{}}
	rules := []Rule{{Name: "all", UseInventory: true,
		Venues: []VenueRule{{Venue: "binance", MaxDailyNotional: "1000000"}, {Venue: "nobitex"}}}}
	require.NoError(t, Validate(rules, nil))
	r := &Router{Rules: fixedRules{RuleSet{Version: 1, Rules: rules}}, Venues: c, Inventory: c, Usage: c}

	want, err := r.Plan(ctx, input("buy", "2.5"))
	require.NoError(t, err)
	once := *c

	m := r.Memoized()
	for _, q := range []string{"2.5", "2.4", "2.3"} {
		_, err := m.Plan(ctx, input("buy", q))
		require.NoError(t, err)
	}
	got, err := m.Plan(ctx, input("buy", "2.5"))
	require.NoError(t, err)
	assert.Equal(t, legs(want), legs(got), "the memoized router routes as the router")
	assert.Equal(t, 2*once.markets, c.markets, "each market is read once by the memoized router")
	assert.Equal(t, 2*once.funds, c.funds)
	assert.Equal(t, 2*once.capacity, c.capacity)
	assert.Equal(t, 2*once.daily, c.daily)
	assert.Positive(t, once.funds)
	assert.Positive(t, once.capacity)
	assert.Positive(t, once.daily)

	_, err = r.Plan(ctx, input("buy", "2.5"))
	require.NoError(t, err)
	assert.Equal(t, 3*once.funds, c.funds, "the router itself reads the state again")
}
