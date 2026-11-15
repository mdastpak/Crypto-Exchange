package otc

import (
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func assertDec(t *testing.T, want string, got decimal.Decimal, msg ...any) {
	t.Helper()
	assert.True(t, dec(want).Equal(got), "want %s, got %s %v", want, got, msg)
}

func pricing(t *testing.T, m config.OTCMarket) Pricing {
	t.Helper()
	p, err := ParsePricing("BTC/USDT", m)
	require.NoError(t, err)
	return p
}

// TestComponents_AddUpOnEachSide checks the formula of #61: a buy is the
// reference raised by half the spread, the hedge cost, the margin, and the
// risk premium; a sell is lowered by the same.
func TestComponents_AddUpOnEachSide(t *testing.T) {
	p := pricing(t, config.OTCMarket{SpreadBPS: 20, HedgeCostBPS: 15, MarginBPS: 20, RiskPremiumBPS: 5})
	parts, err := p.Components(dec("100"), dec("1"))
	require.NoError(t, err)
	assertDec(t, "10", parts.HalfSpreadBPS)
	assertDec(t, "50", parts.TotalBPS())
	buy, err := parts.Price(SideBuy)
	require.NoError(t, err)
	assertDec(t, "100.5", buy)
	sell, err := parts.Price(SideSell)
	require.NoError(t, err)
	assertDec(t, "99.5", sell)
	assert.Equal(t, "BTC/USDT", p.Reference, "an empty reference is the market itself")
}

// TestSlippage_GrowsWithTheSquareRootOfTheQuantity checks
// sqrt(k × quantity / liquidity): four times the quantity doubles it.
func TestSlippage_GrowsWithTheSquareRootOfTheQuantity(t *testing.T) {
	p := pricing(t, config.OTCMarket{SlippageK: "0.0001", Liquidity: "100"})
	one, err := p.Components(dec("100"), dec("1"))
	require.NoError(t, err)
	assertDec(t, "10", one.SlippageBPS, "sqrt(0.0001 × 1 / 100) = 0.001")
	four, err := p.Components(dec("100"), dec("4"))
	require.NoError(t, err)
	assertDec(t, "20", four.SlippageBPS)
	buy, _ := four.Price(SideBuy)
	assertDec(t, "100.2", buy)

	none := pricing(t, config.OTCMarket{SlippageK: "0.0001"})
	parts, err := none.Components(dec("100"), dec("4"))
	require.NoError(t, err)
	assert.True(t, parts.SlippageBPS.IsZero(), "no liquidity set is no slippage")

	bounded := pricing(t, config.OTCMarket{SlippageK: "0.0001", Liquidity: "100", MaxSlippageBPS: 15})
	_, err = bounded.Components(dec("100"), dec("1"))
	require.NoError(t, err)
	_, err = bounded.Components(dec("100"), dec("4"))
	require.ErrorIs(t, err, ErrTooLarge)
}

func TestSqrt(t *testing.T) {
	assertDec(t, "1.414213562373095049", sqrt(dec("2")))
	assertDec(t, "0.001", sqrt(dec("0.000001")))
	assertDec(t, "0", sqrt(dec("0")))
	assertDec(t, "0", sqrt(dec("-4")))
}

// TestMargin_BandsAndTheMinimum checks the bands of the margin by size,
// and that no quote has a margin under the minimum: it is widened (the
// guard against a quote at a loss).
func TestMargin_BandsAndTheMinimum(t *testing.T) {
	p := pricing(t, config.OTCMarket{MarginBPS: 20, MinMarginBPS: 10, MarginBands: []config.OTCMarginBand{
		{MinQuantity: "10", MarginBPS: 15}, {MinQuantity: "100", MarginBPS: 5},
	}})
	for _, c := range []struct {
		quantity, margin string
		widened          bool
	}{{"1", "20", false}, {"10", "15", false}, {"50", "15", false}, {"100", "10", true}, {"500", "10", true}} {
		parts, err := p.Components(dec("100"), dec(c.quantity))
		require.NoError(t, err)
		assertDec(t, c.margin, parts.MarginBPS, c.quantity)
		assert.Equal(t, c.widened, parts.MarginWidened, c.quantity)
	}

	low := pricing(t, config.OTCMarket{MarginBPS: 2, MinMarginBPS: 8})
	parts, err := low.Components(dec("100"), dec("1"))
	require.NoError(t, err)
	assertDec(t, "8", parts.MarginBPS)
}

// TestHedgeCost_SpreadsTheFixedPartOverTheNotional checks that a fixed
// cost per quote weighs less on a larger quote.
func TestHedgeCost_SpreadsTheFixedPartOverTheNotional(t *testing.T) {
	p := pricing(t, config.OTCMarket{HedgeCostBPS: 10, HedgeCostFixed: "1"})
	small, err := p.Components(dec("100"), dec("2"))
	require.NoError(t, err)
	assertDec(t, "60", small.HedgeCostBPS, "1 over a notional of 200 is 50 bps")
	large, err := p.Components(dec("100"), dec("200"))
	require.NoError(t, err)
	assertDec(t, "10.5", large.HedgeCostBPS)
}

// TestRoundPrice_GoesAgainstTheUser rounds a buy up and a sell down to the
// tick.
func TestRoundPrice_GoesAgainstTheUser(t *testing.T) {
	assertDec(t, "100.15", RoundPrice(dec("100.123"), dec("0.05"), SideBuy))
	assertDec(t, "100.1", RoundPrice(dec("100.123"), dec("0.05"), SideSell))
	assertDec(t, "100.1", RoundPrice(dec("100.1"), dec("0.05"), SideBuy), "a price on the tick stays")
	assertDec(t, "65001", RoundPrice(dec("65000.2"), dec("1"), SideBuy))
	assertDec(t, "65000.2", RoundPrice(dec("65000.2"), dec("0"), SideBuy), "no tick leaves it")
	assertDec(t, "0.123", FloorTo(dec("0.12399"), dec("0.001")))
}

func TestParsePricing(t *testing.T) {
	p := pricing(t, config.OTCMarket{Reference: "USDT/IRT", ReferenceMultiplier: "10"})
	assert.Equal(t, "USDT/IRT", p.Reference)
	assertDec(t, "10", p.ReferenceMultiplier)
	_, err := ParsePricing("BTC/USDT", config.OTCMarket{SlippageK: "-1"})
	require.Error(t, err)
	_, err = ParsePricing("BTC/USDT", config.OTCMarket{ReferenceMultiplier: "0"})
	require.Error(t, err)
}

// TestPrice_RefusesAZeroPrice is a sell whose parts would leave nothing,
// which the configuration refuses, and the pricing too.
func TestPrice_RefusesAZeroPrice(t *testing.T) {
	_, err := Components{Reference: dec("100"), MarginBPS: dec("10000")}.Price(SideSell)
	require.Error(t, err)
}
