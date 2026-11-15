package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOTC_ExampleAndDefaults(t *testing.T) {
	require.NoError(t, loadFile(t, configFile("config.example.yaml"), nil))
	var p problems
	Config.otcProblems(&p)
	assert.Empty(t, p, "the example prices its markets")
	assert.Contains(t, Config.OTC.Markets, "USDT/IRR")
	assert.Equal(t, "10", Config.OTC.Markets["USDT/IRR"].ReferenceMultiplier)

	var empty Configuration
	empty.applyOTCDefaults()
	assert.Equal(t, DefaultOTCQuoteTTLSeconds, empty.OTC.QuoteTTLSeconds)
	assert.Equal(t, DefaultOTCQuotesPerSecond, empty.OTC.QuotesPerSecond)
	assert.Equal(t, DefaultOTCQuoteBurst, empty.OTC.Burst)
}

func TestOTC_Problems(t *testing.T) {
	market := func(c *Configuration, m OTCMarket) {
		c.OTC.Markets = map[string]OTCMarket{"BTC/USDT": m}
	}
	for name, tc := range map[string]struct {
		change func(c *Configuration)
		want   string
	}{
		"TTL under 5 seconds": {func(c *Configuration) { c.OTC.QuoteTTLSeconds = 4 }, "otc.quote_ttl_seconds is 4; it must be from 5 to 10"},
		"TTL over 10 seconds": {func(c *Configuration) { c.OTC.QuoteTTLSeconds = 11 }, "otc.quote_ttl_seconds is 11"},
		"no rate":             {func(c *Configuration) { c.OTC.QuotesPerSecond = -1 }, "otc.quotes_per_second is -1"},
		"lower-case market":   {func(c *Configuration) { market(c, OTCMarket{}); c.OTC.Markets["btc/usdt"] = OTCMarket{} }, "otc.markets.btc/usdt is not a market symbol"},
		"negative spread":     {func(c *Configuration) { market(c, OTCMarket{SpreadBPS: -1}) }, "otc.markets.BTC/USDT.spread_bps is -1"},
		"bad reference":       {func(c *Configuration) { market(c, OTCMarket{Reference: "USDT-IRT"}) }, `otc.markets.BTC/USDT.reference is "USDT-IRT"`},
		"zero multiplier":     {func(c *Configuration) { market(c, OTCMarket{ReferenceMultiplier: "0"}) }, "otc.markets.BTC/USDT.reference_multiplier"},
		"negative liquidity":  {func(c *Configuration) { market(c, OTCMarket{Liquidity: "-5"}) }, `otc.markets.BTC/USDT.liquidity is "-5"`},
		"bands going down": {func(c *Configuration) {
			market(c, OTCMarket{MarginBands: []OTCMarginBand{{MinQuantity: "10", MarginBPS: 5}, {MinQuantity: "1", MarginBPS: 3}}})
		}, "otc.markets.BTC/USDT.margin_bands.1.min_quantity is 1; the bands must go up in quantity, after 10"},
		"band without a quantity": {func(c *Configuration) {
			market(c, OTCMarket{MarginBands: []OTCMarginBand{{MarginBPS: 5}}})
		}, "otc.markets.BTC/USDT.margin_bands.0.min_quantity must be set"},
		"no price left on a sell": {func(c *Configuration) {
			market(c, OTCMarket{SpreadBPS: 2000, MarginBPS: 3000, MaxSlippageBPS: 1000})
		}, "otc.markets.BTC/USDT adds up to 5000 basis points over the reference; it must stay under 5000"},
	} {
		t.Run(name, func(t *testing.T) {
			c := secureRelease(t)
			tc.change(&c)
			assert.Contains(t, strings.Join(problemsOf(c), "\n"), tc.want)
		})
	}
}
