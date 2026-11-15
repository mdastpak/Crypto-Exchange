// Package otc prices firm, expiring OTC quotes (#61): a request for a
// quote of a market returns a price that the user may take until it
// expires, bound to the user and signed, so that the execution (#62) runs
// at that price or not at all.
package otc

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/shopspring/decimal"
)

// Side is the side of the user: a buy of the base currency, paid in the
// quote currency, or a sell of it.
type Side string

const (
	SideBuy  Side = "buy"
	SideSell Side = "sell"
)

// Valid reports whether s is a side.
func (s Side) Valid() bool { return s == SideBuy || s == SideSell }

var (
	bpsUnit = decimal.NewFromInt(10000)
	one     = decimal.NewFromInt(1)
)

// Pricing is the parsed settings of a market (config.OTCMarket).
type Pricing struct {
	Reference           string
	ReferenceMultiplier decimal.Decimal
	SpreadBPS           int
	HedgeCostBPS        int
	HedgeCostFixed      decimal.Decimal
	MarginBPS           int
	MinMarginBPS        int
	Bands               []Band
	RiskPremiumBPS      int
	SlippageK           decimal.Decimal
	Liquidity           decimal.Decimal
	MaxSlippageBPS      int
}

// Band is a margin from a quantity on.
type Band struct {
	MinQuantity decimal.Decimal
	MarginBPS   int
}

// ParsePricing parses the settings of the market symbol; an empty
// reference is the market itself.
func ParsePricing(symbol string, m config.OTCMarket) (Pricing, error) {
	p := Pricing{
		Reference: m.Reference, SpreadBPS: m.SpreadBPS, HedgeCostBPS: m.HedgeCostBPS, MarginBPS: m.MarginBPS,
		MinMarginBPS: m.MinMarginBPS, RiskPremiumBPS: m.RiskPremiumBPS, MaxSlippageBPS: m.MaxSlippageBPS,
		ReferenceMultiplier: one,
	}
	if p.Reference == "" {
		p.Reference = symbol
	}
	var err error
	parse := func(name, s string, into *decimal.Decimal) {
		if s == "" || err != nil {
			return
		}
		d, e := decimal.NewFromString(s)
		if e != nil || d.IsNegative() {
			err = fmt.Errorf("otc.markets.%s.%s: %q is not an amount of zero or more", symbol, name, s)
			return
		}
		*into = d
	}
	parse("reference_multiplier", m.ReferenceMultiplier, &p.ReferenceMultiplier)
	parse("hedge_cost_fixed", m.HedgeCostFixed, &p.HedgeCostFixed)
	parse("slippage_k", m.SlippageK, &p.SlippageK)
	parse("liquidity", m.Liquidity, &p.Liquidity)
	for i, b := range m.MarginBands {
		var q decimal.Decimal
		parse(fmt.Sprintf("margin_bands.%d.min_quantity", i), b.MinQuantity, &q)
		p.Bands = append(p.Bands, Band{MinQuantity: q, MarginBPS: b.MarginBPS})
	}
	if err == nil && !p.ReferenceMultiplier.IsPositive() {
		err = fmt.Errorf("otc.markets.%s.reference_multiplier must be positive", symbol)
	}
	return p, err
}

// Components are the parts of a quoted price, in basis points of the
// reference, which the quote records so that the margin of every trade is
// known: the realized margin is the quoted price less the hedge price and
// the hedge costs.
type Components struct {
	// Reference is the reference price in the quote currency, after the
	// multiplier.
	Reference decimal.Decimal `json:"reference"`
	// HalfSpreadBPS is the half of the spread of this side.
	HalfSpreadBPS decimal.Decimal `json:"half_spread_bps"`
	// HedgeCostBPS has the fixed part of the hedge cost, spread over the
	// quantity.
	HedgeCostBPS   decimal.Decimal `json:"hedge_cost_bps"`
	MarginBPS      decimal.Decimal `json:"margin_bps"`
	RiskPremiumBPS decimal.Decimal `json:"risk_premium_bps"`
	SlippageBPS    decimal.Decimal `json:"slippage_bps"`
	// MarginWidened reports a margin that was below the minimum and was
	// widened to it.
	MarginWidened bool `json:"margin_widened,omitempty"`
	// OverrideID is the administrator's price override that the reference
	// was (#453), for the execution to record which one it traded on.
	OverrideID string `json:"override_id,omitempty"`
}

// TotalBPS is the sum of the parts.
func (c Components) TotalBPS() decimal.Decimal {
	return c.HalfSpreadBPS.Add(c.HedgeCostBPS).Add(c.MarginBPS).Add(c.RiskPremiumBPS).Add(c.SlippageBPS)
}

// ErrTooLarge is a quantity whose slippage is above the bound of its
// market.
var ErrTooLarge = errors.New("otc: the quantity is above what the market can quote")

// errNoPrice is a price that the parts would make zero or negative.
var errNoPrice = errors.New("otc: the parts of the price leave no positive price")

// margin returns the margin of a quantity: that of the last band it
// reaches, or MarginBPS, and never under MinMarginBPS.
func (p Pricing) margin(quantity decimal.Decimal) (int, bool) {
	m := p.MarginBPS
	for _, b := range p.Bands {
		if quantity.GreaterThanOrEqual(b.MinQuantity) {
			m = b.MarginBPS
		}
	}
	if m < p.MinMarginBPS {
		return p.MinMarginBPS, true
	}
	return m, false
}

// slippageBPS returns sqrt(k × quantity / liquidity) in basis points.
func (p Pricing) slippageBPS(quantity decimal.Decimal) decimal.Decimal {
	if !p.SlippageK.IsPositive() || !p.Liquidity.IsPositive() || !quantity.IsPositive() {
		return decimal.Zero
	}
	return sqrt(p.SlippageK.Mul(quantity).Div(p.Liquidity)).Mul(bpsUnit)
}

// Components returns the parts of the price of quantity at the reference
// price ref, which is already in the quote currency.
func (p Pricing) Components(ref, quantity decimal.Decimal) (Components, error) {
	margin, widened := p.margin(quantity)
	c := Components{
		Reference:      ref,
		HalfSpreadBPS:  decimal.NewFromInt(int64(p.SpreadBPS)).Div(decimal.NewFromInt(2)),
		HedgeCostBPS:   decimal.NewFromInt(int64(p.HedgeCostBPS)),
		MarginBPS:      decimal.NewFromInt(int64(margin)),
		RiskPremiumBPS: decimal.NewFromInt(int64(p.RiskPremiumBPS)),
		SlippageBPS:    p.slippageBPS(quantity),
		MarginWidened:  widened,
	}
	if p.HedgeCostFixed.IsPositive() && quantity.IsPositive() && ref.IsPositive() {
		// The fixed part over the notional, in basis points.
		c.HedgeCostBPS = c.HedgeCostBPS.Add(p.HedgeCostFixed.Div(quantity.Mul(ref)).Mul(bpsUnit))
	}
	c.SlippageBPS = c.SlippageBPS.Round(4)
	c.HedgeCostBPS = c.HedgeCostBPS.Round(4)
	if p.MaxSlippageBPS > 0 && c.SlippageBPS.GreaterThan(decimal.NewFromInt(int64(p.MaxSlippageBPS))) {
		return c, ErrTooLarge
	}
	return c, nil
}

// Price returns the unrounded price of a side for the parts c: the
// reference raised by them for a buy, and lowered by them for a sell.
func (c Components) Price(side Side) (decimal.Decimal, error) {
	f := c.TotalBPS().Div(bpsUnit)
	if side == SideSell {
		f = f.Neg()
	}
	price := c.Reference.Mul(one.Add(f))
	if !price.IsPositive() {
		return decimal.Zero, errNoPrice
	}
	return price, nil
}

// RoundPrice rounds a price to the tick, against the user: up for a buy,
// and down for a sell. A zero tick leaves it as it is.
func RoundPrice(price, tick decimal.Decimal, side Side) decimal.Decimal {
	if !tick.IsPositive() {
		return price
	}
	n := price.Div(tick)
	if side == SideBuy {
		n = n.Ceil()
	} else {
		n = n.Floor()
	}
	return n.Mul(tick)
}

// FloorTo rounds an amount down to a multiple of step; a zero step leaves
// it as it is.
func FloorTo(amount, step decimal.Decimal) decimal.Decimal {
	if !step.IsPositive() {
		return amount
	}
	return amount.Div(step).Floor().Mul(step)
}

// sqrt returns the square root of x, to 18 decimal places.
func sqrt(x decimal.Decimal) decimal.Decimal {
	if !x.IsPositive() {
		return decimal.Zero
	}
	f, _, err := big.ParseFloat(x.String(), 10, 256, big.ToNearestEven)
	if err != nil {
		return decimal.Zero
	}
	r, err := decimal.NewFromString(new(big.Float).SetPrec(256).Sqrt(f).Text('f', 18))
	if err != nil {
		return decimal.Zero
	}
	return r
}
