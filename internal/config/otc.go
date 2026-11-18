package config

import (
	"fmt"
	"maps"
	"math/big"
	"regexp"
	"slices"
	"strings"
)

// The bounds and defaults of the OTC quotes (#61).
const (
	MinOTCQuoteTTLSeconds     = 5
	MaxOTCQuoteTTLSeconds     = 10
	DefaultOTCQuoteTTLSeconds = 8
	DefaultOTCQuotesPerSecond = 5
	DefaultOTCQuoteBurst      = 10
	// MaxOTCPriceBPS bounds the sum of the components of a quote, so that
	// a sell is always quoted at a positive price.
	MaxOTCPriceBPS = 5000
	// The bounds and default of how long a node keeps the routing rules
	// (#386) before it checks for a new version.
	MinOTCRoutingCacheSeconds     = 1
	MaxOTCRoutingCacheSeconds     = 60
	DefaultOTCRoutingCacheSeconds = 5
	// The bounds and defaults of the routed orders (#387): how far from the
	// reference price an order's limit may go, how long an order may stay
	// open before it is cancelled and its remainder routed again, and how
	// long a venue may take to show an order whose answer was lost.
	MinOTCRoutingSlippageBPS         = 1
	MaxOTCRoutingSlippageBPS         = 1000
	DefaultOTCRoutingSlippageBPS     = 50
	MinOTCRoutingOrderTimeoutSeconds = 5
	MaxOTCRoutingOrderTimeoutSeconds = 3600
	DefaultOTCRoutingOrderTimeout    = 60
	MinOTCRoutingUnknownGraceSeconds = 10
	MaxOTCRoutingUnknownGraceSeconds = 3600
	DefaultOTCRoutingUnknownGrace    = 120
)

// OTCConfig configures the OTC quotes (#61). See
// docs/configuration.md#otc.
type OTCConfig struct {
	// QuoteTTLSeconds is how long a quote is firm, from 5 to 10 seconds.
	QuoteTTLSeconds int `yaml:"quote_ttl_seconds"`
	// QuotesPerSecond and Burst limit the quote requests of each user, in
	// Redis, so that the limit holds over every instance.
	QuotesPerSecond int `yaml:"quotes_per_second"`
	Burst           int `yaml:"burst"`
	// Markets are the markets that are quoted, by their symbol, such as
	// "BTC/USDT". A market without an entry is not quoted.
	Markets map[string]OTCMarket `yaml:"markets"`
	// InventoryLimits bound the inventory of the exchange per currency
	// symbol (#62). A release build requires one for both currencies of
	// every quoted market, so that no currency is traded without a bound.
	InventoryLimits map[string]OTCInventoryLimit `yaml:"inventory_limits"`
	// Routing routes the trades to the inventory or the venues (#386).
	Routing OTCRoutingConfig `yaml:"routing"`
}

// OTCRoutingConfig configures the routing of the OTC trades (#386). The
// rules themselves are data, which the administrators version through
// the admin API; see docs/otc-routing.md.
type OTCRoutingConfig struct {
	// Enabled routes every quote and execution by the active rules: a
	// trade that no rule routes is refused. Off, the inventory takes every
	// trade, as before routing.
	Enabled bool `yaml:"enabled"`
	// CacheSeconds is how long a node keeps the active rules before it
	// checks for a new version: a change applies on every node within it.
	CacheSeconds int `yaml:"cache_seconds"`
	// MaxSlippageBPS bounds the limit of a routed order (#387): a buy at
	// most reference × (1 + it), and a sell at least reference × (1 − it).
	MaxSlippageBPS int `yaml:"max_slippage_bps"`
	// OrderTimeoutSeconds is how long a routed order may stay open: then it
	// is cancelled, and what it did not fill is routed again.
	OrderTimeoutSeconds int `yaml:"order_timeout_seconds"`
	// UnknownGraceSeconds is how long a routed order whose answer was lost
	// is looked up before it is placed again: a venue may not show an
	// order at once.
	UnknownGraceSeconds int `yaml:"unknown_grace_seconds"`
}

// OTCMarket prices the quotes of one market. A buy is quoted at
// reference × (1 + spread/2 + hedge cost + margin + risk premium +
// slippage), and a sell at reference × (1 − the same). Every part in basis
// points is of the reference price: 100 is 1%.
type OTCMarket struct {
	// Reference is the market of the price engine (#58) that gives the
	// reference price; empty is the market itself. ReferenceMultiplier
	// converts its price to the quote currency: 10 for a rial market whose
	// reference is in tomans, such as USDT/IRR from USDT/IRT. Empty is 1.
	Reference           string `yaml:"reference"`
	ReferenceMultiplier string `yaml:"reference_multiplier"`
	// SpreadBPS is the whole spread; each side takes half of it.
	SpreadBPS int `yaml:"spread_bps"`
	// HedgeCostBPS is what covering the trade on the hedging venue costs:
	// its taker fee and the transfer fees, in basis points. HedgeCostFixed
	// is a fixed part of it per quote, in the quote currency, which is
	// spread over the quantity.
	HedgeCostBPS   int    `yaml:"hedge_cost_bps"`
	HedgeCostFixed string `yaml:"hedge_cost_fixed"`
	// MarginBPS is the profit margin over the hedge cost. MarginBands may
	// lower or raise it from a quantity on. No quote has a margin under
	// MinMarginBPS: a lower one is widened to it.
	MarginBPS    int             `yaml:"margin_bps"`
	MinMarginBPS int             `yaml:"min_margin_bps"`
	MarginBands  []OTCMarginBand `yaml:"margin_bands"`
	// RiskPremiumBPS covers volatility and inventory far from its target.
	RiskPremiumBPS int `yaml:"risk_premium_bps"`
	// The slippage is sqrt(SlippageK × quantity / Liquidity), a fraction of
	// the reference, with Liquidity in units of the base currency. Empty or
	// zero has no slippage. A quote whose slippage would be above
	// MaxSlippageBPS is refused as too large; 0 is no bound.
	SlippageK      string `yaml:"slippage_k"`
	Liquidity      string `yaml:"liquidity"`
	MaxSlippageBPS int    `yaml:"max_slippage_bps"`
}

// OTCMarginBand sets the margin of the quotes from a quantity on, in units
// of the base currency.
type OTCMarginBand struct {
	MinQuantity string `yaml:"min_quantity"`
	MarginBPS   int    `yaml:"margin_bps"`
}

// applyOTCDefaults sets the TTL and the rate limit when they are unset.
func (c *Configuration) applyOTCDefaults() {
	o := &c.OTC
	if o.QuoteTTLSeconds == 0 {
		o.QuoteTTLSeconds = DefaultOTCQuoteTTLSeconds
	}
	if o.QuotesPerSecond == 0 {
		o.QuotesPerSecond = DefaultOTCQuotesPerSecond
	}
	if o.Burst == 0 {
		o.Burst = DefaultOTCQuoteBurst
	}
	if o.Routing.CacheSeconds == 0 {
		o.Routing.CacheSeconds = DefaultOTCRoutingCacheSeconds
	}
	if o.Routing.MaxSlippageBPS == 0 {
		o.Routing.MaxSlippageBPS = DefaultOTCRoutingSlippageBPS
	}
	if o.Routing.OrderTimeoutSeconds == 0 {
		o.Routing.OrderTimeoutSeconds = DefaultOTCRoutingOrderTimeout
	}
	if o.Routing.UnknownGraceSeconds == 0 {
		o.Routing.UnknownGraceSeconds = DefaultOTCRoutingUnknownGrace
	}
}

var otcMarketSymbol = regexp.MustCompile(`^[A-Z0-9]{1,15}/[A-Z0-9]{1,15}$`)

// otcProblems reports a TTL out of range, a limit that is not positive, and
// markets whose settings cannot price a quote.
func (c *Configuration) otcProblems(p *problems) {
	o := c.OTC
	if o.QuoteTTLSeconds < MinOTCQuoteTTLSeconds || o.QuoteTTLSeconds > MaxOTCQuoteTTLSeconds {
		p.add("otc.quote_ttl_seconds", "is %d; it must be from %d to %d", o.QuoteTTLSeconds, MinOTCQuoteTTLSeconds, MaxOTCQuoteTTLSeconds)
	}
	if o.QuotesPerSecond < 1 {
		p.add("otc.quotes_per_second", "is %d; it must be at least 1", o.QuotesPerSecond)
	}
	if o.Burst < 1 {
		p.add("otc.burst", "is %d; it must be at least 1", o.Burst)
	}
	if r := o.Routing.CacheSeconds; r < MinOTCRoutingCacheSeconds || r > MaxOTCRoutingCacheSeconds {
		p.add("otc.routing.cache_seconds", "is %d; it must be from %d to %d", r, MinOTCRoutingCacheSeconds, MaxOTCRoutingCacheSeconds)
	}
	for _, b := range []struct {
		key           string
		value, lo, hi int
	}{
		{"otc.routing.max_slippage_bps", o.Routing.MaxSlippageBPS, MinOTCRoutingSlippageBPS, MaxOTCRoutingSlippageBPS},
		{"otc.routing.order_timeout_seconds", o.Routing.OrderTimeoutSeconds, MinOTCRoutingOrderTimeoutSeconds, MaxOTCRoutingOrderTimeoutSeconds},
		{"otc.routing.unknown_grace_seconds", o.Routing.UnknownGraceSeconds, MinOTCRoutingUnknownGraceSeconds, MaxOTCRoutingUnknownGraceSeconds},
	} {
		if b.value < b.lo || b.value > b.hi {
			p.add(b.key, "is %d; it must be from %d to %d", b.value, b.lo, b.hi)
		}
	}
	for _, symbol := range slices.Sorted(maps.Keys(o.Markets)) {
		key := "otc.markets." + symbol
		if !otcMarketSymbol.MatchString(symbol) {
			p.add(key, "is not a market symbol: BASE/QUOTE in upper-case letters and digits")
			continue
		}
		otcMarketProblems(p, key, o.Markets[symbol])
	}
	for _, symbol := range slices.Sorted(maps.Keys(o.InventoryLimits)) {
		key := "otc.inventory_limits." + symbol
		if !otcCurrencySymbol.MatchString(symbol) {
			p.add(key, "is not a currency symbol: upper-case letters and digits")
			continue
		}
		l := o.InventoryLimits[symbol]
		nonNegativeAmount(p, key+".max_short", l.MaxShort)
		nonNegativeAmount(p, key+".max_long", l.MaxLong)
	}
}

var otcCurrencySymbol = regexp.MustCompile(`^[A-Z0-9]{1,15}$`)

// otcInventoryProblems reports, in a release build, a currency of a quoted
// market without an inventory limit with both bounds (#62).
func (c *Configuration) otcInventoryProblems(p *problems) {
	o := c.OTC
	for _, symbol := range slices.Sorted(maps.Keys(o.Markets)) {
		base, quote, ok := strings.Cut(symbol, "/")
		if !ok {
			continue
		}
		for _, currency := range []string{base, quote} {
			l, ok := o.InventoryLimits[currency]
			if !ok || l.MaxShort == "" || l.MaxLong == "" {
				p.add("otc.inventory_limits."+currency, "must set max_short and max_long in a release build: the market %s trades it", symbol)
			}
		}
	}
}

func otcMarketProblems(p *problems, key string, m OTCMarket) {
	if m.Reference != "" && !otcMarketSymbol.MatchString(m.Reference) {
		p.add(key+".reference", "is %q; it must be a market of the price engine, BASE/QUOTE", m.Reference)
	}
	if s := m.ReferenceMultiplier; s != "" {
		if r, ok := new(big.Rat).SetString(s); !ok || r.Sign() <= 0 || !plainDecimal.MatchString(s) {
			p.add(key+".reference_multiplier", "is %q; it must be a positive decimal number, such as \"10\"", s)
		}
	}
	bps := func(name string, v int) {
		if v < 0 || v > MaxOTCPriceBPS {
			p.add(key+"."+name, "is %d; it must be from 0 to %d basis points", v, MaxOTCPriceBPS)
		}
	}
	bps("spread_bps", m.SpreadBPS)
	bps("hedge_cost_bps", m.HedgeCostBPS)
	bps("margin_bps", m.MarginBPS)
	bps("min_margin_bps", m.MinMarginBPS)
	bps("risk_premium_bps", m.RiskPremiumBPS)
	bps("max_slippage_bps", m.MaxSlippageBPS)
	nonNegativeAmount(p, key+".hedge_cost_fixed", m.HedgeCostFixed)
	nonNegativeAmount(p, key+".slippage_k", m.SlippageK)
	nonNegativeAmount(p, key+".liquidity", m.Liquidity)
	highest := max(m.MarginBPS, m.MinMarginBPS)
	var last string
	for i, band := range m.MarginBands {
		bandKey := fmt.Sprintf("%s.margin_bands.%d", key, i)
		bps(fmt.Sprintf("margin_bands.%d.margin_bps", i), band.MarginBPS)
		highest = max(highest, band.MarginBPS)
		q, ok := positiveAmount(p, bandKey+".min_quantity", band.MinQuantity)
		if ok && q == nil {
			p.add(bandKey+".min_quantity", "must be set")
			continue
		}
		if ok && last != "" {
			prev, _ := positiveAmount(&problems{}, "", last)
			if prev != nil && q.Cmp(prev) <= 0 {
				p.add(bandKey+".min_quantity", "is %s; the bands must go up in quantity, after %s", band.MinQuantity, last)
			}
		}
		if ok {
			last = band.MinQuantity
		}
	}
	// The sell side subtracts every part: their sum must leave a positive
	// price. The slippage counts at its bound, when it has one.
	if sum := m.SpreadBPS/2 + m.HedgeCostBPS + highest + m.RiskPremiumBPS + m.MaxSlippageBPS; sum >= MaxOTCPriceBPS {
		p.add(key, "adds up to %d basis points over the reference; it must stay under %d", sum, MaxOTCPriceBPS)
	}
}

// nonNegativeAmount reports an amount that is set and is not a decimal
// number of zero or more.
func nonNegativeAmount(p *problems, key, s string) {
	if s == "" || s == "0" {
		return
	}
	if !plainDecimal.MatchString(s) {
		p.add(key, "is %q; it must be a decimal amount of zero or more, such as \"0.5\"", s)
	}
}

// OTCInventoryLimit bounds the net position of the exchange's inventory in
// a currency from the OTC trades (#62), in units of the currency. MaxShort
// is how much the inventory may owe: what it gave users beyond what they
// gave it. MaxLong is how much more it may hold than it gave. Empty is no
// bound. A trade that would take the position beyond a bound is refused;
// one that brings it back toward zero always passes.
type OTCInventoryLimit struct {
	MaxShort string `yaml:"max_short"`
	MaxLong  string `yaml:"max_long"`
}
