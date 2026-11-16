package routing

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/region"

	"github.com/shopspring/decimal"
)

// The sources of a leg.
const (
	SourceInventory = "inventory"
	SourceVenue     = "venue"
)

// The reasons that no rule routes a trade, which the quote's refusal
// carries.
const (
	// ReasonNoRules is a release without a version of the rules.
	ReasonNoRules = "no_rules"
	// ReasonNoMatchingRule is a trade that no rule matches.
	ReasonNoMatchingRule = "no_matching_rule"
	// ReasonInsufficientLiquidity is a trade above what the sources of
	// every matching rule can fill.
	ReasonInsufficientLiquidity = "insufficient_liquidity"
	// ReasonCostTooHigh is a route above the cost ceiling of its rule.
	ReasonCostTooHigh = "cost_too_high"
)

// The reasons that a rule does not match, or that a source is left out,
// which the explanation shows.
const (
	skipMarket        = "market_not_in_rule"
	skipSide          = "side_not_in_rule"
	skipNetwork       = "network_not_in_rule"
	skipRegion        = "region_not_in_rule"
	skipLevel         = "level_below_rule"
	skipNotionalLow   = "notional_below_rule"
	skipNotionalHigh  = "notional_above_rule"
	skipNoBalance     = "no_available_balance"
	skipDailyLimit    = "daily_limit_reached"
	skipLotMismatch   = "lot_mismatch"
	skipBelowMinimum  = "below_venue_minimum"
	skipCostAboveRule = "cost_above_rule"
	skipExcluded      = "excluded"
)

var bps = decimal.NewFromInt(10000)

// Input is a trade to route. The caller fills it from the quote: the
// market, the side, and the quantity of the base currency, at the
// reference price of the quote currency.
type Input struct {
	// Market is our market symbol, such as BTC/USDT, of the currencies
	// Base and Quote, whose keys are BaseHK and QuoteHK.
	Market, Base, Quote, BaseHK, QuoteHK string
	// Side is the user's side: buy or sell.
	Side     string
	Quantity decimal.Decimal
	// Price is the reference price, in the quote currency, which sizes the
	// notional and what a venue balance buys.
	Price decimal.Decimal
	// Lot is the lot of our market; every leg is a multiple of it.
	Lot decimal.Decimal
	// Region is the region of the user, and Level their verification
	// level (#157).
	Region region.Region
	Level  int
	// Network is our network of the user's destination (#57), when the
	// trade has one: the venue must withdraw on it.
	Network string
	// Day is the day of the venues' daily limits, in UTC.
	Day time.Time
	// Exclude are venues that the route must not use, such as one whose
	// order of the trade failed (#387).
	Exclude []string
}

// Notional is the notional of the trade in the quote currency.
func (in Input) Notional() decimal.Decimal { return in.Quantity.Mul(in.Price) }

// Leg is a part of a route.
type Leg struct {
	Source string `json:"source"`
	Venue  string `json:"venue,omitempty"`
	// Symbol is the venue's symbol of the market.
	Symbol   string          `json:"symbol,omitempty"`
	Quantity decimal.Decimal `json:"quantity"`
	Notional decimal.Decimal `json:"notional"`
	// CostBPS is the venue fee, the slippage, and the withdrawal fee of the
	// leg, in basis points of its notional; 0 for the inventory.
	CostBPS decimal.Decimal `json:"cost_bps"`
}

// Route is where a trade's liquidity comes from.
type Route struct {
	RuleVersion int64  `json:"rule_version"`
	Rule        string `json:"rule"`
	Legs        []Leg  `json:"legs"`
	// Network is the network of the user's destination that the route was
	// planned for, which the execution plans for again.
	Network string `json:"network,omitempty"`
	// Lot is the lot of our market that the legs are multiples of.
	Lot decimal.Decimal `json:"lot,omitzero"`
	// CostBPS is the cost of the legs, weighted by their notional, in
	// basis points of the trade's notional, rounded up to a hundredth.
	CostBPS decimal.Decimal `json:"cost_bps"`
}

// NoRoute is a trade that no rule routes.
type NoRoute struct {
	Reason string
}

func (n *NoRoute) Error() string { return "routing: no route: " + n.Reason }

// VenueMarket is a venue's market of our base and quote currencies.
type VenueMarket struct {
	Symbol string
	// Lot, MinQuantity, and MinNotional are the venue's rules of an order.
	Lot, MinQuantity, MinNotional decimal.Decimal
	// FeeBPS and SlippageBPS are the configured costs of the venue
	// (venues.exchanges.<name>.fee_bps and slippage_bps).
	FeeBPS, SlippageBPS int
	// Reasons are why the market may not take an order; none is routable.
	Reasons []string
}

// Funds is what the router may plan on of a currency at a venue.
type Funds struct {
	// Available is the free balance less the open reservations (#385).
	Available decimal.Decimal
	// WithdrawFee is the fee of a withdrawal on the network asked for.
	WithdrawFee decimal.Decimal
	// Reasons are why the currency is not routable; none is routable.
	Reasons []string
}

// Venues reads the state of the venues (#385).
type Venues interface {
	// Market returns the venue's market of our currencies base and quote.
	Market(ctx context.Context, venue, base, quote string) (VenueMarket, error)
	// Funds returns what the router may plan on of our currency at venue;
	// a non-empty network also needs our network open there.
	Funds(ctx context.Context, venue, currency, network string) (Funds, error)
}

// Inventory reads what the exchange's own inventory may give.
type Inventory interface {
	// Capacity returns how much of the currency the inventory may still
	// give before its limit (#62); bounded is false for no limit.
	Capacity(ctx context.Context, currencyHK, symbol string) (capacity decimal.Decimal, bounded bool, err error)
}

// Usage reads the notional that the routed orders took from each venue.
type Usage interface {
	// DailyNotional is the notional of a quote currency that venue took on
	// day.
	DailyNotional(ctx context.Context, venue, currencyHK string, day time.Time) (decimal.Decimal, error)
}

// RuleSource returns the active rules.
type RuleSource interface {
	Active(ctx context.Context) (RuleSet, error)
}

// Router plans the routes.
type Router struct {
	Rules     RuleSource
	Venues    Venues
	Inventory Inventory
	Usage     Usage
}

// Plan returns the route of a trade, or a *NoRoute.
func (r *Router) Plan(ctx context.Context, in Input) (Route, error) {
	e, err := r.Explain(ctx, in)
	if err != nil {
		return Route{}, err
	}
	if e.Route == nil {
		return Route{}, &NoRoute{Reason: e.Reason}
	}
	return *e.Route, nil
}

// Explanation is how the rules route a trade: each rule tried, in order,
// and the route, which places nothing.
type Explanation struct {
	RuleVersion int64        `json:"rule_version"`
	Rules       []RuleResult `json:"rules"`
	Route       *Route       `json:"route,omitempty"`
	// Reason is why there is no route.
	Reason string `json:"reason,omitempty"`
}

// RuleResult is one rule tried.
type RuleResult struct {
	Rule    string `json:"rule"`
	Matched bool   `json:"matched"`
	// Skipped are why the rule does not match.
	Skipped []string `json:"skipped,omitempty"`
	// Sources are the sources of a matched rule.
	Sources []SourceResult `json:"sources,omitempty"`
	// Failure is why a matched rule gives no route.
	Failure string `json:"failure,omitempty"`
}

// SourceResult is a source of a matched rule, and what it can fill.
type SourceResult struct {
	Source string `json:"source"`
	Venue  string `json:"venue,omitempty"`
	// Capacity is the quantity of the base currency that it can fill;
	// empty is no bound.
	Capacity *decimal.Decimal `json:"capacity,omitempty"`
	CostBPS  decimal.Decimal  `json:"cost_bps"`
	Skipped  []string         `json:"skipped,omitempty"`
}

// Explain tries the rules in order and returns the route of the first
// that gives one.
func (r *Router) Explain(ctx context.Context, in Input) (Explanation, error) {
	set, err := r.Rules.Active(ctx)
	if err != nil {
		return Explanation{}, err
	}
	e := Explanation{RuleVersion: set.Version, Rules: []RuleResult{}}
	if set.Version == 0 {
		e.Reason = ReasonNoRules
		return e, nil
	}
	if !in.Quantity.IsPositive() || !in.Price.IsPositive() {
		return Explanation{}, fmt.Errorf("routing: the quantity and the price must be positive")
	}
	in.Day = in.Day.UTC().Truncate(24 * time.Hour)
	for _, rule := range set.sorted() {
		res := RuleResult{Rule: rule.Name}
		if res.Skipped = rule.mismatch(in); len(res.Skipped) > 0 {
			e.Rules = append(e.Rules, res)
			continue
		}
		res.Matched = true
		legs, sources, failure, err := r.fill(ctx, rule, in)
		if err != nil {
			return Explanation{}, err
		}
		res.Sources, res.Failure = sources, failure
		e.Rules = append(e.Rules, res)
		if failure != "" {
			if e.Reason == "" {
				e.Reason = failure
			}
			continue
		}
		route := Route{RuleVersion: set.Version, Rule: rule.Name, Legs: legs, Network: in.Network, Lot: in.Lot, CostBPS: costOf(legs, in.Notional())}
		if rule.MaxCostBPS > 0 && route.CostBPS.GreaterThan(decimal.NewFromInt(int64(rule.MaxCostBPS))) {
			e.Rules[len(e.Rules)-1].Failure = ReasonCostTooHigh
			if e.Reason == "" {
				e.Reason = ReasonCostTooHigh
			}
			continue
		}
		e.Route, e.Reason = &route, ""
		return e, nil
	}
	if e.Reason == "" {
		e.Reason = ReasonNoMatchingRule
	}
	return e, nil
}

// mismatch returns why the rule does not match the trade; none matches.
func (rule Rule) mismatch(in Input) []string {
	var out []string
	if len(rule.Markets) > 0 && !slices.Contains(rule.Markets, in.Market) {
		out = append(out, skipMarket)
	}
	if len(rule.Sides) > 0 && !slices.Contains(rule.Sides, in.Side) {
		out = append(out, skipSide)
	}
	if len(rule.Networks) > 0 && !slices.Contains(rule.Networks, in.Network) {
		out = append(out, skipNetwork)
	}
	if len(rule.Regions) > 0 && !slices.ContainsFunc(rule.Regions, func(s string) bool { return strings.EqualFold(s, string(in.Region)) }) {
		out = append(out, skipRegion)
	}
	if in.Level < rule.MinLevel {
		out = append(out, skipLevel)
	}
	notional := in.Notional()
	if rule.minNotional != nil && notional.LessThan(*rule.minNotional) {
		out = append(out, skipNotionalLow)
	}
	if rule.maxNotional != nil && notional.GreaterThan(*rule.maxNotional) {
		out = append(out, skipNotionalHigh)
	}
	return out
}

// source is a source of a matched rule.
type source struct {
	result   SourceResult
	priority int
	weight   int
	symbol   string
	// capacity is nil for no bound; step is what its quantity is a
	// multiple of; minQuantity and minNotional bound a leg of a venue.
	capacity                       *decimal.Decimal
	step, minQuantity, minNotional decimal.Decimal
	// feeBPS is the fee and the slippage; withdrawFee the fee of the
	// withdrawal, in the currency that the venue gives.
	feeBPS, withdrawFee decimal.Decimal
}

// fill sizes the sources of a matched rule and allocates the trade to
// them. It returns the legs, or the reason that they cannot fill it.
func (r *Router) fill(ctx context.Context, rule Rule, in Input) ([]Leg, []SourceResult, string, error) {
	var sources []source
	if rule.UseInventory {
		s, err := r.inventory(ctx, in)
		if err != nil {
			return nil, nil, "", err
		}
		sources = append(sources, s)
	}
	venues := slices.Clone(rule.Venues)
	slices.SortStableFunc(venues, func(a, b VenueRule) int {
		if a.Priority != b.Priority {
			return a.Priority - b.Priority
		}
		return strings.Compare(a.Venue, b.Venue)
	})
	for _, v := range venues {
		s, err := r.venue(ctx, rule, v, in)
		if err != nil {
			return nil, nil, "", err
		}
		sources = append(sources, s)
	}
	results := func() []SourceResult {
		out := make([]SourceResult, 0, len(sources))
		for _, s := range sources {
			out = append(out, s.result)
		}
		return out
	}

	// A venue whose leg would be under its minimum is left out, and the
	// trade allocated again, until every leg places.
	for range len(sources) + 1 {
		usable := make([]source, 0, len(sources))
		for _, s := range sources {
			if len(s.result.Skipped) == 0 {
				usable = append(usable, s)
			}
		}
		groups := order(usable, rule.Strategy)
		amounts, left := allocate(groups, in.Quantity)
		if left.IsPositive() {
			return nil, results(), ReasonInsufficientLiquidity, nil
		}
		var legs []Leg
		small := false
		for gi, g := range groups {
			for si, s := range g {
				q := amounts[gi][si]
				if !q.IsPositive() {
					continue
				}
				notional := q.Mul(in.Price)
				if s.result.Source == SourceVenue && (q.LessThan(s.minQuantity) || notional.LessThan(s.minNotional)) {
					for i := range sources {
						if sources[i].result.Venue == s.result.Venue {
							sources[i].result.Skipped = append(sources[i].result.Skipped, skipBelowMinimum)
						}
					}
					small = true
					continue
				}
				leg := Leg{Source: s.result.Source, Venue: s.result.Venue, Symbol: s.symbol, Quantity: q, Notional: notional}
				if s.result.Source == SourceVenue {
					leg.CostBPS = legCost(s, in, q)
				}
				legs = append(legs, leg)
			}
		}
		if !small {
			return legs, results(), "", nil
		}
	}
	return nil, results(), ReasonInsufficientLiquidity, nil
}

// inventory sizes the exchange's own inventory: what it may give of the
// currency that the user receives, in the base currency.
func (r *Router) inventory(ctx context.Context, in Input) (source, error) {
	s := source{result: SourceResult{Source: SourceInventory}, step: in.Lot}
	if r.Inventory == nil {
		return s, nil
	}
	hk, symbol := in.BaseHK, in.Base
	if in.Side == "sell" {
		hk, symbol = in.QuoteHK, in.Quote
	}
	c, bounded, err := r.Inventory.Capacity(ctx, hk, symbol)
	if err != nil {
		return s, err
	}
	if bounded {
		if in.Side == "sell" {
			c = c.Div(in.Price)
		}
		c = floorTo(decimal.Max(c, decimal.Zero), in.Lot)
		s.capacity = &c
		s.result.Capacity = &c
		if !c.IsPositive() {
			s.result.Skipped = append(s.result.Skipped, skipNoBalance)
		}
	}
	return s, nil
}

// venue sizes a venue of a rule. A buy buys the base currency at the
// venue with its quote balance; a sell sells the base balance there.
func (r *Router) venue(ctx context.Context, rule Rule, v VenueRule, in Input) (source, error) {
	s := source{result: SourceResult{Source: SourceVenue, Venue: v.Venue}, priority: v.Priority, weight: v.Weight}
	skip := func(reasons ...string) { s.result.Skipped = append(s.result.Skipped, reasons...) }
	if slices.Contains(in.Exclude, v.Venue) {
		skip(skipExcluded)
		return s, nil
	}
	m, err := r.Venues.Market(ctx, v.Venue, in.Base, in.Quote)
	if err != nil {
		return s, err
	}
	skip(m.Reasons...)
	s.symbol, s.minQuantity, s.minNotional = m.Symbol, m.MinQuantity, m.MinNotional
	s.feeBPS = decimal.NewFromInt(int64(m.FeeBPS + m.SlippageBPS))
	switch {
	case !m.Lot.IsPositive() || isMultiple(in.Lot, m.Lot):
		s.step = in.Lot
	case !in.Lot.IsPositive() || isMultiple(m.Lot, in.Lot):
		s.step = m.Lot
	default:
		skip(skipLotMismatch)
	}

	spent, received := in.Quote, in.Base
	if in.Side == "sell" {
		spent, received = in.Base, in.Quote
	}
	funds, err := r.Venues.Funds(ctx, v.Venue, spent, "")
	if err != nil {
		return s, err
	}
	skip(funds.Reasons...)
	if in.Network != "" {
		out, err := r.Venues.Funds(ctx, v.Venue, received, in.Network)
		if err != nil {
			return s, err
		}
		skip(out.Reasons...)
		s.withdrawFee = out.WithdrawFee
	}

	available := funds.Available
	if v.reserve != nil {
		available = available.Sub(*v.reserve)
	}
	if v.MaxBalanceShareBPS > 0 {
		available = available.Mul(decimal.NewFromInt(int64(v.MaxBalanceShareBPS))).Div(bps)
	}
	capacity := available
	if in.Side == "buy" {
		capacity = available.Div(in.Price.Mul(decimal.NewFromInt(1).Add(decimal.NewFromInt(int64(m.FeeBPS)).Div(bps))))
	}
	if v.maxDaily != nil && r.Usage != nil {
		used, err := r.Usage.DailyNotional(ctx, v.Venue, in.QuoteHK, in.Day)
		if err != nil {
			return s, err
		}
		left := v.maxDaily.Sub(used)
		if !left.IsPositive() {
			skip(skipDailyLimit)
		}
		capacity = decimal.Min(capacity, left.Div(in.Price))
	}
	capacity = floorTo(decimal.Max(capacity, decimal.Zero), s.step)
	s.capacity, s.result.Capacity = &capacity, &capacity
	if !capacity.IsPositive() && !slices.Contains(s.result.Skipped, skipDailyLimit) {
		skip(skipNoBalance)
	}
	s.result.CostBPS = legCost(s, in, in.Quantity)
	if rule.MaxCostBPS > 0 && s.result.CostBPS.GreaterThan(decimal.NewFromInt(int64(rule.MaxCostBPS))) {
		skip(skipCostAboveRule)
	}
	return s, nil
}

// legCost is the cost of a leg of q at a venue, in basis points of its
// notional: the fee and the slippage, and the withdrawal fee over the leg.
func legCost(s source, in Input, q decimal.Decimal) decimal.Decimal {
	cost := s.feeBPS
	if s.withdrawFee.IsPositive() && q.IsPositive() {
		over := q
		if in.Side == "sell" {
			over = q.Mul(in.Price)
		}
		cost = cost.Add(s.withdrawFee.Div(over).Mul(bps))
	}
	return cost.RoundUp(2)
}

// costOf is the cost of the legs weighted by their notional.
func costOf(legs []Leg, notional decimal.Decimal) decimal.Decimal {
	if !notional.IsPositive() {
		return decimal.Zero
	}
	sum := decimal.Zero
	for _, l := range legs {
		sum = sum.Add(l.CostBPS.Mul(l.Notional))
	}
	return sum.Div(notional).RoundUp(2)
}

// order groups the sources in the order they fill. Own first: the
// inventory, then the venues by priority, those of one priority in one
// group. Cheapest: each source alone, by cost, then priority and name.
func order(sources []source, strategy string) [][]source {
	if strategy == StrategyCheapest {
		sorted := slices.Clone(sources)
		slices.SortStableFunc(sorted, func(a, b source) int {
			if c := a.result.CostBPS.Cmp(b.result.CostBPS); c != 0 {
				return c
			}
			if a.result.Source != b.result.Source {
				// The inventory before a venue of the same cost.
				return strings.Compare(a.result.Source, b.result.Source)
			}
			if a.priority != b.priority {
				return a.priority - b.priority
			}
			return strings.Compare(a.result.Venue, b.result.Venue)
		})
		out := make([][]source, 0, len(sorted))
		for _, s := range sorted {
			out = append(out, []source{s})
		}
		return out
	}
	var out [][]source
	for _, s := range sources {
		if n := len(out); n > 0 && s.result.Source == SourceVenue && out[n-1][0].result.Source == SourceVenue &&
			out[n-1][0].priority == s.priority {
			out[n-1] = append(out[n-1], s)
			continue
		}
		out = append(out, []source{s})
	}
	return out
}

// allocate allocates quantity to the groups in order. In a group with
// weights, each source first takes its share of what is left, floored to
// its step; then what is left goes to the sources in order, up to their
// capacity. It returns the amounts, by group and source, and what no
// source could take.
func allocate(groups [][]source, quantity decimal.Decimal) ([][]decimal.Decimal, decimal.Decimal) {
	left := quantity
	amounts := make([][]decimal.Decimal, len(groups))
	take := func(s source, want decimal.Decimal) decimal.Decimal {
		q := want
		if s.capacity != nil {
			q = decimal.Min(q, *s.capacity)
		}
		return floorTo(decimal.Max(q, decimal.Zero), s.step)
	}
	for gi, g := range groups {
		amounts[gi] = make([]decimal.Decimal, len(g))
		total := 0
		for _, s := range g {
			total += s.weight
		}
		if total > 0 && left.IsPositive() {
			share := left
			for si, s := range g {
				q := take(s, share.Mul(decimal.NewFromInt(int64(s.weight))).Div(decimal.NewFromInt(int64(total))))
				amounts[gi][si] = q
				left = left.Sub(q)
			}
		}
		for si, s := range g {
			if !left.IsPositive() {
				break
			}
			room := left
			if s.capacity != nil {
				room = decimal.Min(room, s.capacity.Sub(amounts[gi][si]))
			}
			q := floorTo(decimal.Max(room, decimal.Zero), s.step)
			amounts[gi][si] = amounts[gi][si].Add(q)
			left = left.Sub(q)
		}
	}
	return amounts, left
}

// floorTo rounds amount down to a multiple of step; a step of zero leaves
// it.
func floorTo(amount, step decimal.Decimal) decimal.Decimal {
	if !step.IsPositive() {
		return amount
	}
	return amount.Div(step).Floor().Mul(step)
}

// isMultiple reports whether a is a whole multiple of b.
func isMultiple(a, b decimal.Decimal) bool {
	return b.IsPositive() && a.Mod(b).IsZero()
}
