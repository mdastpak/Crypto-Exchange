// Package routing picks where the liquidity of an OTC trade comes from
// (#386): the exchange's own inventory, an external venue (#384, #385), or
// several of them, by the rules that the administrators set. The router
// runs when a quote is priced (#61), so that its price includes the cost
// of the route, and again when it is executed (#62), against fresh state.
//
// A route is deterministic: the same rules, inputs, and state give the
// same route on every node. See docs/otc-routing.md.
package routing

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/region"

	"github.com/shopspring/decimal"
)

// The strategies of a rule.
const (
	// StrategyOwnFirst takes what the inventory can give, then the venues
	// in their order.
	StrategyOwnFirst = "own_first"
	// StrategyCheapest takes the cheapest sources first, the inventory
	// costing nothing.
	StrategyCheapest = "cheapest"
)

// The bounds of a rule set.
const (
	MaxRules   = 200
	MaxVenues  = 20
	MaxRuleBPS = 10000
)

// Rule is one routing rule. A rule matches a trade by its market, side,
// network, notional, region, and the user's verification level; an empty
// list matches everything. The first matching rule, by priority and then
// name, that gives a route is the route.
type Rule struct {
	Name     string `json:"name"`
	Priority int    `json:"priority"`
	// Markets are our market symbols, such as BTC/USDT.
	Markets []string `json:"markets,omitempty"`
	// Sides are the user's sides: buy or sell.
	Sides []string `json:"sides,omitempty"`
	// Networks are our networks of the user's destination (#57).
	Networks []string `json:"networks,omitempty"`
	// MinNotional and MaxNotional bound the notional of the trade, in its
	// quote currency.
	MinNotional string `json:"min_notional,omitempty"`
	MaxNotional string `json:"max_notional,omitempty"`
	// Regions are the deployment regions (#272) that the rule serves.
	Regions []string `json:"regions,omitempty"`
	// MinLevel is the lowest verification level (#157) that the rule
	// serves.
	MinLevel int `json:"min_level,omitempty"`
	// Strategy is own_first (the default) or cheapest.
	Strategy string `json:"strategy,omitempty"`
	// UseInventory lets the exchange's own inventory fill the trade.
	UseInventory bool `json:"use_inventory"`
	// Venues are the external venues that may fill it.
	Venues []VenueRule `json:"venues,omitempty"`
	// MaxCostBPS bounds the cost of the route, in basis points of the
	// notional; 0 is no bound.
	MaxCostBPS int `json:"max_cost_bps,omitempty"`

	minNotional, maxNotional *decimal.Decimal
}

// VenueRule is a venue that a rule may route to.
type VenueRule struct {
	Venue string `json:"venue"`
	// Priority orders the venues of a rule, lower first.
	Priority int `json:"priority,omitempty"`
	// Weight splits a trade across the venues of one priority in
	// proportion; venues of a priority without weights fill in turn.
	Weight int `json:"weight,omitempty"`
	// MaxBalanceShareBPS is the share of the venue's available balance
	// that one trade may use; 0 is all of it.
	MaxBalanceShareBPS int `json:"max_balance_share_bps,omitempty"`
	// Reserve is an amount of the asset that the venue spends, which no
	// trade takes.
	Reserve string `json:"reserve,omitempty"`
	// MaxDailyNotional bounds the notional that the venue takes in a day,
	// in the quote currency of the trade.
	MaxDailyNotional string `json:"max_daily_notional,omitempty"`

	reserve, maxDaily *decimal.Decimal
}

// RuleSet is a version of the rules.
type RuleSet struct {
	Version int64  `json:"version"`
	Rules   []Rule `json:"rules"`
}

var ruleName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._:-]{0,63}$`)

// Problems are what Validate refused, by the path of the field.
type Problems map[string]string

func (p Problems) Error() string {
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range p {
			if !yield(k) {
				return
			}
		}
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+" "+p[k])
	}
	return "routing: " + strings.Join(parts, "; ")
}

// Validate checks rules against the configuration: every venue is one with
// a connector, an international release names no Iranian venue (#272),
// and the amounts and bounds are in range. It fills the parsed amounts.
func Validate(rules []Rule, c *config.Configuration) error {
	p := Problems{}
	if len(rules) > MaxRules {
		p["rules"] = fmt.Sprintf("are %d; at most %d", len(rules), MaxRules)
	}
	international := c != nil && c.Region().International()
	names := map[string]bool{}
	for i := range rules {
		r := &rules[i]
		key := fmt.Sprintf("rules[%d]", i)
		if !ruleName.MatchString(r.Name) {
			p[key+".name"] = "must be 1 to 64 letters, digits, spaces, or ._:-"
		} else if names[r.Name] {
			p[key+".name"] = fmt.Sprintf("%q is the name of another rule", r.Name)
		}
		names[r.Name] = true
		for j, m := range r.Markets {
			base, quote, ok := strings.Cut(m, "/")
			if !ok || base == "" || quote == "" || strings.ToUpper(m) != m {
				p[fmt.Sprintf("%s.markets[%d]", key, j)] = fmt.Sprintf("%q is not a market such as BTC/USDT", m)
			}
		}
		for j, s := range r.Sides {
			if s != "buy" && s != "sell" {
				p[fmt.Sprintf("%s.sides[%d]", key, j)] = fmt.Sprintf("%q is not buy or sell", s)
			}
		}
		for j, rg := range r.Regions {
			if _, err := region.Parse(rg); err != nil {
				p[fmt.Sprintf("%s.regions[%d]", key, j)] = fmt.Sprintf("%q is not a region", rg)
			}
		}
		switch r.Strategy {
		case "", StrategyOwnFirst, StrategyCheapest:
		default:
			p[key+".strategy"] = fmt.Sprintf("%q is not own_first or cheapest", r.Strategy)
		}
		if r.MinLevel < 0 || r.MinLevel > 4 {
			p[key+".min_level"] = "must be from 0 to 4"
		}
		if r.MaxCostBPS < 0 || r.MaxCostBPS > MaxRuleBPS {
			p[key+".max_cost_bps"] = fmt.Sprintf("must be from 0 to %d", MaxRuleBPS)
		}
		r.minNotional = amount(p, key+".min_notional", r.MinNotional)
		r.maxNotional = amount(p, key+".max_notional", r.MaxNotional)
		if r.minNotional != nil && r.maxNotional != nil && r.minNotional.GreaterThan(*r.maxNotional) {
			p[key+".max_notional"] = "is below min_notional"
		}
		if !r.UseInventory && len(r.Venues) == 0 {
			p[key] = "routes to nothing: set use_inventory or a venue"
		}
		if len(r.Venues) > MaxVenues {
			p[key+".venues"] = fmt.Sprintf("are %d; at most %d", len(r.Venues), MaxVenues)
		}
		seen := map[string]bool{}
		for j := range r.Venues {
			v := &r.Venues[j]
			vk := fmt.Sprintf("%s.venues[%d]", key, j)
			switch {
			case !config.IsKnownVenue(v.Venue):
				p[vk+".venue"] = fmt.Sprintf("%q is not a venue with a connector", v.Venue)
			case international && config.IsIranianVenue(v.Venue):
				p[vk+".venue"] = fmt.Sprintf("%s is an Iranian venue, which an international release does not use", v.Venue)
			case seen[v.Venue]:
				p[vk+".venue"] = fmt.Sprintf("%s is named twice in the rule", v.Venue)
			}
			seen[v.Venue] = true
			if v.Weight < 0 || v.Weight > MaxRuleBPS {
				p[vk+".weight"] = fmt.Sprintf("must be from 0 to %d", MaxRuleBPS)
			}
			if v.MaxBalanceShareBPS < 0 || v.MaxBalanceShareBPS > MaxRuleBPS {
				p[vk+".max_balance_share_bps"] = fmt.Sprintf("must be from 0 to %d", MaxRuleBPS)
			}
			v.reserve = amount(p, vk+".reserve", v.Reserve)
			v.maxDaily = amount(p, vk+".max_daily_notional", v.MaxDailyNotional)
		}
	}
	if len(p) > 0 {
		return p
	}
	return nil
}

// amount parses an optional amount of zero or more.
func amount(p Problems, key, s string) *decimal.Decimal {
	if s == "" {
		return nil
	}
	d, err := decimal.NewFromString(s)
	if err != nil || d.IsNegative() {
		p[key] = fmt.Sprintf("%q is not an amount of zero or more", s)
		return nil
	}
	return &d
}

// Decode reads the rules of a version as stored, and parses them.
func Decode(version int64, raw []byte, c *config.Configuration) (RuleSet, error) {
	var rules []Rule
	if err := json.Unmarshal(raw, &rules); err != nil {
		return RuleSet{}, fmt.Errorf("routing: decode the rules of version %d: %w", version, err)
	}
	if err := Validate(rules, c); err != nil {
		return RuleSet{}, fmt.Errorf("routing: the rules of version %d: %w", version, err)
	}
	return RuleSet{Version: version, Rules: rules}, nil
}

// sorted returns the rules in the order they are tried: by priority, then
// name.
func (s RuleSet) sorted() []Rule {
	out := slices.Clone(s.Rules)
	slices.SortStableFunc(out, func(a, b Rule) int {
		if a.Priority != b.Priority {
			return a.Priority - b.Priority
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}
