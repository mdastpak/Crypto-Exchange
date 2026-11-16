package routing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/region"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// fakeVenues are venues of fixed state: a market and the funds of each
// currency, by venue.
type fakeVenues struct {
	markets map[string]VenueMarket
	funds   map[string]map[string]Funds
}

func (f fakeVenues) Market(_ context.Context, venue, base, quote string) (VenueMarket, error) {
	m, ok := f.markets[venue]
	if !ok {
		return VenueMarket{Reasons: []string{ReasonVenueDisabled}}, nil
	}
	return m, nil
}

func (f fakeVenues) Funds(_ context.Context, venue, currency, network string) (Funds, error) {
	key := currency
	if network != "" {
		key += ":" + network
	}
	fu, ok := f.funds[venue][key]
	if !ok {
		return Funds{Reasons: []string{ReasonNotMapped}}, nil
	}
	return fu, nil
}

type fakeInventory struct {
	capacity map[string]decimal.Decimal
}

func (f fakeInventory) Capacity(_ context.Context, hk, symbol string) (decimal.Decimal, bool, error) {
	c, ok := f.capacity[symbol]
	return c, ok, nil
}

type fakeUsage map[string]decimal.Decimal

func (f fakeUsage) DailyNotional(_ context.Context, venue, hk string, _ time.Time) (decimal.Decimal, error) {
	return f[venue], nil
}

type fixedRules struct{ set RuleSet }

func (f fixedRules) Active(context.Context) (RuleSet, error) { return f.set, nil }

// market is a venue market of BTC/USDT with a fee of 10 and a slippage of
// 5 basis points.
func market(symbol string) VenueMarket {
	return VenueMarket{Symbol: symbol, Lot: d("0.001"), FeeBPS: 10, SlippageBPS: 5}
}

// world is a binance and a nobitex with 100000 USDT and 2 BTC each, a
// withdrawal of BTC on bitcoin at 0.0005, and an inventory of 1 BTC and
// 50000 USDT.
func world() (fakeVenues, fakeInventory) {
	funds := func() map[string]Funds {
		return map[string]Funds{
			"USDT":         {Available: d("100000")},
			"BTC":          {Available: d("2")},
			"BTC:bitcoin":  {Available: d("2"), WithdrawFee: d("0.0005")},
			"USDT:tron":    {Available: d("100000"), WithdrawFee: d("1")},
			"USDT:bitcoin": {Reasons: []string{"network_unknown"}},
		}
	}
	return fakeVenues{
			markets: map[string]VenueMarket{"binance": market("BTCUSDT"), "nobitex": market("BTCUSDT")},
			funds:   map[string]map[string]Funds{"binance": funds(), "nobitex": funds()},
		}, fakeInventory{capacity: map[string]decimal.Decimal{
			"BTC": d("1"), "USDT": d("50000"),
		}}
}

func input(side, quantity string) Input {
	return Input{
		Market: "BTC/USDT", Base: "BTC", Quote: "USDT", BaseHK: "base", QuoteHK: "quote",
		Side: side, Quantity: d(quantity), Price: d("50000"), Lot: d("0.001"),
		Region: region.Iran, Level: 2, Day: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC),
	}
}

func router(t *testing.T, rules []Rule, venues fakeVenues, inv fakeInventory, usage fakeUsage) *Router {
	t.Helper()
	require.NoError(t, Validate(rules, nil))
	return &Router{Rules: fixedRules{RuleSet{Version: 7, Rules: rules}}, Venues: venues, Inventory: inv, Usage: usage}
}

func legs(r Route) map[string]string {
	out := map[string]string{}
	for _, l := range r.Legs {
		key := l.Source
		if l.Venue != "" {
			key = l.Venue
		}
		out[key] = l.Quantity.String()
	}
	return out
}

// TestPlan_EachConstraint covers each constraint of a rule (#386): a
// trade that it does not allow is routed by the next rule, the catch-all
// of the inventory, or by none.
func TestPlan_EachConstraint(t *testing.T) {
	venue := func(mutate func(*VenueRule)) Rule {
		v := VenueRule{Venue: "binance"}
		if mutate != nil {
			mutate(&v)
		}
		return Rule{Name: "venue", Venues: []VenueRule{v}}
	}
	cases := map[string]struct {
		rule    Rule
		in      func(*Input)
		state   func(*fakeVenues)
		usage   fakeUsage
		want    map[string]string
		reason  string
		skipped string
	}{
		"a market of the rule": {
			rule: func() Rule { r := venue(nil); r.Markets = []string{"BTC/USDT"}; return r }(),
			want: map[string]string{"binance": "0.5"},
		},
		"another market": {
			rule:    func() Rule { r := venue(nil); r.Markets = []string{"ETH/USDT"}; return r }(),
			reason:  ReasonNoMatchingRule,
			skipped: skipMarket,
		},
		"another side": {
			rule:    func() Rule { r := venue(nil); r.Sides = []string{"sell"}; return r }(),
			reason:  ReasonNoMatchingRule,
			skipped: skipSide,
		},
		"a network of the rule": {
			rule: func() Rule { r := venue(nil); r.Networks = []string{"bitcoin"}; return r }(),
			in:   func(in *Input) { in.Network = "bitcoin" },
			want: map[string]string{"binance": "0.5"},
		},
		"no network for a rule of networks": {
			rule:    func() Rule { r := venue(nil); r.Networks = []string{"bitcoin"}; return r }(),
			reason:  ReasonNoMatchingRule,
			skipped: skipNetwork,
		},
		"a network closed at the venue": {
			rule:  venue(nil),
			in:    func(in *Input) { in.Network = "bitcoin" },
			state: func(v *fakeVenues) { v.funds["binance"]["BTC:bitcoin"] = Funds{Reasons: []string{"network_suspended"}} },
			// The venue is left out, and nothing else fills it.
			reason: ReasonInsufficientLiquidity,
		},
		"another region": {
			rule:    func() Rule { r := venue(nil); r.Regions = []string{"international"}; return r }(),
			reason:  ReasonNoMatchingRule,
			skipped: skipRegion,
		},
		"a level below the rule": {
			rule:    func() Rule { r := venue(nil); r.MinLevel = 3; return r }(),
			reason:  ReasonNoMatchingRule,
			skipped: skipLevel,
		},
		"a notional below the rule": {
			rule:    func() Rule { r := venue(nil); r.MinNotional = "30000"; return r }(),
			reason:  ReasonNoMatchingRule,
			skipped: skipNotionalLow,
		},
		"a notional above the rule": {
			rule:    func() Rule { r := venue(nil); r.MaxNotional = "20000"; return r }(),
			reason:  ReasonNoMatchingRule,
			skipped: skipNotionalHigh,
		},
		"a market halted at the venue": {
			rule: venue(nil),
			state: func(v *fakeVenues) {
				m := v.markets["binance"]
				m.Reasons = []string{"market_halted"}
				v.markets["binance"] = m
			},
			reason: ReasonInsufficientLiquidity,
		},
		"a share of the balance": {
			// 10% of 100000 USDT buys about 0.1998 BTC at 50000 with the fee.
			rule:   venue(func(v *VenueRule) { v.MaxBalanceShareBPS = 1000 }),
			reason: ReasonInsufficientLiquidity,
		},
		"a reserve never spent": {
			rule:   venue(func(v *VenueRule) { v.Reserve = "80000" }),
			reason: ReasonInsufficientLiquidity,
		},
		"a reserve that leaves enough": {
			rule: venue(func(v *VenueRule) { v.Reserve = "70000" }),
			want: map[string]string{"binance": "0.5"},
		},
		"a daily limit reached": {
			rule:   venue(func(v *VenueRule) { v.MaxDailyNotional = "100000" }),
			usage:  fakeUsage{"binance": d("100000")},
			reason: ReasonInsufficientLiquidity,
		},
		"a daily limit with room": {
			rule:  venue(func(v *VenueRule) { v.MaxDailyNotional = "100000" }),
			usage: fakeUsage{"binance": d("75000")},
			want:  map[string]string{"binance": "0.5"},
		},
		"a cost above the rule": {
			rule:   func() Rule { r := venue(nil); r.MaxCostBPS = 14; return r }(),
			reason: ReasonInsufficientLiquidity,
		},
		"a cost within the rule": {
			rule: func() Rule { r := venue(nil); r.MaxCostBPS = 15; return r }(),
			want: map[string]string{"binance": "0.5"},
		},
		"a withdrawal fee that raises the cost above the rule": {
			// 0.0005 BTC over 0.5 is 10 basis points more.
			rule: func() Rule { r := venue(nil); r.MaxCostBPS = 20; return r }(),
			in:   func(in *Input) { in.Network = "bitcoin" },
			// The venue alone is over: it is left out.
			reason: ReasonInsufficientLiquidity,
		},
		"a lot of the venue that ours is not a multiple of": {
			rule: venue(nil),
			state: func(v *fakeVenues) {
				m := v.markets["binance"]
				m.Lot = d("0.0003")
				v.markets["binance"] = m
			},
			reason: ReasonInsufficientLiquidity,
		},
		"a leg under the venue's minimum": {
			rule: venue(nil),
			state: func(v *fakeVenues) {
				m := v.markets["binance"]
				m.MinNotional = d("30000")
				v.markets["binance"] = m
			},
			reason: ReasonInsufficientLiquidity,
		},
		"the inventory within its limit": {
			rule: Rule{Name: "own", UseInventory: true},
			want: map[string]string{SourceInventory: "0.5"},
		},
		"the inventory beyond its limit": {
			rule:   Rule{Name: "own", UseInventory: true},
			in:     func(in *Input) { in.Quantity = d("1.5") },
			reason: ReasonInsufficientLiquidity,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			venues, inv := world()
			if tc.state != nil {
				tc.state(&venues)
			}
			in := input("buy", "0.5")
			if tc.in != nil {
				tc.in(&in)
			}
			r := router(t, []Rule{tc.rule}, venues, inv, tc.usage)
			e, err := r.Explain(context.Background(), in)
			require.NoError(t, err)
			if tc.want != nil {
				require.NotNil(t, e.Route, "%+v", e)
				assert.Equal(t, tc.want, legs(*e.Route))
				assert.Equal(t, int64(7), e.Route.RuleVersion)
				return
			}
			assert.Nil(t, e.Route)
			assert.Equal(t, tc.reason, e.Reason)
			if tc.skipped != "" {
				assert.Contains(t, e.Rules[0].Skipped, tc.skipped)
			}
			_, err = r.Plan(context.Background(), in)
			var none *NoRoute
			require.True(t, errors.As(err, &none))
			assert.Equal(t, tc.reason, none.Reason)
		})
	}
}

// TestPlan_RulePriority covers the order of the rules: by priority, then
// name, the first that matches and fills.
func TestPlan_RulePriority(t *testing.T) {
	venues, inv := world()
	rules := []Rule{
		{Name: "z-catch-all", Priority: 10, UseInventory: true},
		{Name: "b-binance", Priority: 1, Venues: []VenueRule{{Venue: "binance"}}},
		{Name: "a-nobitex", Priority: 1, Venues: []VenueRule{{Venue: "nobitex"}}},
		{Name: "large", Priority: 0, MinNotional: "1000000", Venues: []VenueRule{{Venue: "binance"}}},
	}
	r := router(t, rules, venues, inv, nil)
	route, err := r.Plan(context.Background(), input("buy", "0.5"))
	require.NoError(t, err)
	assert.Equal(t, "a-nobitex", route.Rule, "of one priority, by name")

	// nobitex cannot fill: the next rule.
	venues.funds["nobitex"]["USDT"] = Funds{Available: d("100")}
	route, err = r.Plan(context.Background(), input("buy", "0.5"))
	require.NoError(t, err)
	assert.Equal(t, "b-binance", route.Rule)

	// Neither venue: the catch-all of the inventory.
	venues.funds["binance"]["USDT"] = Funds{Available: d("0")}
	route, err = r.Plan(context.Background(), input("buy", "0.5"))
	require.NoError(t, err)
	assert.Equal(t, "z-catch-all", route.Rule)
	assert.Equal(t, map[string]string{SourceInventory: "0.5"}, legs(route))
	assert.True(t, route.CostBPS.IsZero())

	e, err := r.Explain(context.Background(), input("buy", "0.5"))
	require.NoError(t, err)
	names := []string{}
	for _, res := range e.Rules {
		names = append(names, res.Rule)
	}
	assert.Equal(t, []string{"large", "a-nobitex", "b-binance", "z-catch-all"}, names)
	assert.False(t, e.Rules[0].Matched)
	assert.Equal(t, ReasonInsufficientLiquidity, e.Rules[1].Failure)
}

// TestPlan_SplitsAcrossVenues covers the weights of one priority, the
// leftover after a venue's capacity, and the inventory first.
func TestPlan_SplitsAcrossVenues(t *testing.T) {
	t.Run("by weight", func(t *testing.T) {
		venues, inv := world()
		r := router(t, []Rule{{Name: "split", Venues: []VenueRule{
			{Venue: "binance", Weight: 3}, {Venue: "nobitex", Weight: 1},
		}}}, venues, inv, nil)
		route, err := r.Plan(context.Background(), input("buy", "1"))
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"binance": "0.75", "nobitex": "0.25"}, legs(route))
		assert.True(t, route.CostBPS.Equal(d("15")), route.CostBPS.String())
	})
	t.Run("the remainder of the lot to the first", func(t *testing.T) {
		venues, inv := world()
		r := router(t, []Rule{{Name: "split", Venues: []VenueRule{
			{Venue: "binance", Weight: 1}, {Venue: "nobitex", Weight: 2},
		}}}, venues, inv, nil)
		route, err := r.Plan(context.Background(), input("buy", "0.1"))
		require.NoError(t, err)
		// 0.0333… and 0.0666… floored to the lot, and 0.001 left over.
		assert.Equal(t, map[string]string{"binance": "0.034", "nobitex": "0.066"}, legs(route))
	})
	t.Run("a venue's share over its capacity goes to the other", func(t *testing.T) {
		venues, inv := world()
		venues.funds["binance"]["USDT"] = Funds{Available: d("10010")} // 0.2 BTC with the fee
		r := router(t, []Rule{{Name: "split", Venues: []VenueRule{
			{Venue: "binance", Weight: 1}, {Venue: "nobitex", Weight: 1},
		}}}, venues, inv, nil)
		route, err := r.Plan(context.Background(), input("buy", "1"))
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"binance": "0.2", "nobitex": "0.8"}, legs(route))
	})
	t.Run("own first, then the venues in priority", func(t *testing.T) {
		venues, inv := world()
		r := router(t, []Rule{{Name: "own", UseInventory: true, Venues: []VenueRule{
			{Venue: "nobitex", Priority: 2}, {Venue: "binance", Priority: 1},
		}}}, venues, inv, nil)
		route, err := r.Plan(context.Background(), input("buy", "3.5"))
		require.NoError(t, err)
		// 1 BTC of the inventory, 100000 USDT buys 1.998 BTC at binance,
		// and nobitex the rest.
		assert.Equal(t, map[string]string{SourceInventory: "1", "binance": "1.998", "nobitex": "0.502"}, legs(route))
		assert.Equal(t, SourceInventory, route.Legs[0].Source)
	})
	t.Run("cheapest first", func(t *testing.T) {
		venues, inv := world()
		m := venues.markets["nobitex"]
		m.FeeBPS = 2
		venues.markets["nobitex"] = m
		r := router(t, []Rule{{Name: "cheap", Strategy: StrategyCheapest, Venues: []VenueRule{
			{Venue: "binance", Priority: 1}, {Venue: "nobitex", Priority: 2},
		}}}, venues, inv, nil)
		route, err := r.Plan(context.Background(), input("buy", "2.5"))
		require.NoError(t, err)
		assert.Equal(t, "nobitex", route.Legs[0].Venue)
		assert.Equal(t, map[string]string{"nobitex": "1.999", "binance": "0.501"}, legs(route))
	})
	t.Run("a sell spends the base at the venue", func(t *testing.T) {
		venues, inv := world()
		r := router(t, []Rule{{Name: "sell", Venues: []VenueRule{{Venue: "binance"}, {Venue: "nobitex"}}}}, venues, inv, nil)
		route, err := r.Plan(context.Background(), input("sell", "3"))
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"binance": "2", "nobitex": "1"}, legs(route))
	})
}

// TestPlan_IsDeterministic plans one trade many times, through rule sets
// of the same rules in another order: one route.
func TestPlan_IsDeterministic(t *testing.T) {
	venues, inv := world()
	rules := []Rule{
		{Name: "b", Priority: 1, Venues: []VenueRule{{Venue: "nobitex", Weight: 1}, {Venue: "binance", Weight: 1}}},
		{Name: "a", Priority: 1, Venues: []VenueRule{{Venue: "binance", Weight: 2}, {Venue: "nobitex", Weight: 1}}},
	}
	first, err := router(t, rules, venues, inv, nil).Plan(context.Background(), input("buy", "0.7"))
	require.NoError(t, err)
	reversed := []Rule{rules[1], rules[0]}
	for range 20 {
		route, err := router(t, reversed, venues, inv, nil).Plan(context.Background(), input("buy", "0.7"))
		require.NoError(t, err)
		assert.Equal(t, first, route)
	}
	assert.Equal(t, "a", first.Rule)
}

func TestPlan_NoVersionRoutesNothing(t *testing.T) {
	venues, inv := world()
	r := &Router{Rules: fixedRules{}, Venues: venues, Inventory: inv}
	_, err := r.Plan(context.Background(), input("buy", "0.5"))
	var none *NoRoute
	require.True(t, errors.As(err, &none))
	assert.Equal(t, ReasonNoRules, none.Reason)
}

func TestValidate_RefusesWhatCannotRoute(t *testing.T) {
	international := &config.Configuration{}
	international.Deployment.Region = "international"
	cases := map[string]struct {
		rules []Rule
		c     *config.Configuration
		field string
	}{
		"a name twice": {[]Rule{{Name: "a", UseInventory: true}, {Name: "a", UseInventory: true}}, nil, "rules[1].name"},
		"no name":      {[]Rule{{UseInventory: true}}, nil, "rules[0].name"},
		"no source":    {[]Rule{{Name: "a"}}, nil, "rules[0]"},
		"a venue without a connector": {
			[]Rule{{Name: "a", Venues: []VenueRule{{Venue: "kraken"}}}}, nil, "rules[0].venues[0].venue",
		},
		"an Iranian venue in an international release": {
			[]Rule{{Name: "a", Venues: []VenueRule{{Venue: "nobitex"}}}}, international, "rules[0].venues[0].venue",
		},
		"a venue twice": {
			[]Rule{{Name: "a", Venues: []VenueRule{{Venue: "binance"}, {Venue: "binance"}}}}, nil, "rules[0].venues[1].venue",
		},
		"a market that is not one": {[]Rule{{Name: "a", UseInventory: true, Markets: []string{"btc-usdt"}}}, nil, "rules[0].markets[0]"},
		"a side that is not one":   {[]Rule{{Name: "a", UseInventory: true, Sides: []string{"short"}}}, nil, "rules[0].sides[0]"},
		"a region that is not one": {[]Rule{{Name: "a", UseInventory: true, Regions: []string{"eu"}}}, nil, "rules[0].regions[0]"},
		"a strategy that is not one": {
			[]Rule{{Name: "a", UseInventory: true, Strategy: "random"}}, nil, "rules[0].strategy",
		},
		"a negative amount": {
			[]Rule{{Name: "a", Venues: []VenueRule{{Venue: "binance", Reserve: "-1"}}}}, nil, "rules[0].venues[0].reserve",
		},
		"a minimum above the maximum": {
			[]Rule{{Name: "a", UseInventory: true, MinNotional: "10", MaxNotional: "5"}}, nil, "rules[0].max_notional",
		},
		"a share above the whole": {
			[]Rule{{Name: "a", Venues: []VenueRule{{Venue: "binance", MaxBalanceShareBPS: 10001}}}}, nil,
			"rules[0].venues[0].max_balance_share_bps",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := Validate(tc.rules, tc.c)
			var p Problems
			require.True(t, errors.As(err, &p), "%v", err)
			assert.Contains(t, p, tc.field)
		})
	}
	assert.NoError(t, Validate([]Rule{{Name: "a", Venues: []VenueRule{{Venue: "binance"}}}}, international))
	assert.NoError(t, Validate([]Rule{{Name: "a", Venues: []VenueRule{{Venue: "nobitex"}}}}, &config.Configuration{}))
}
