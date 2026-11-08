package main

import (
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/jobs"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func names(specs []jobs.Spec) []string {
	var out []string
	for _, s := range specs {
		out = append(out, s.Job.Name())
	}
	return out
}

func TestJobSpecs_OnlyTheCatalogSourcesThisNodeReaches(t *testing.T) {
	saved := config.Config
	defer func() { config.Config = saved }()

	config.Config.MarketData = config.MarketDataConfig{
		Egress: config.MarketDataEgress{Default: config.EgressDirect},
		Sources: map[string]config.MarketDataSource{
			"coingecko":     {Enabled: true, TimeoutSeconds: 20},
			"coinmarketcap": {Enabled: true, APIKey: "key", TimeoutSeconds: 20, Egress: config.EgressNone},
		},
		Catalog: config.MarketDataCatalog{Priority: []string{"coingecko", "coinmarketcap"}, Coverage: config.CatalogCoverageList,
			Top: 500, IntervalHours: 24, DetailsPerRun: 200, DetailsMaxAgeDays: 30, DeactivateAfterDays: 14},
		Tickers: config.MarketDataTickers{IntervalSeconds: 120, StaleAfterSeconds: 600, OutlierPercent: 5, MinVenues: 3,
			MapRankLimit: 1000, DeactivatePairsAfterHours: 24},
		Candles: config.MarketDataCandles{Quotes: []string{"USDT"}, MaxPairsPerVenue: 200, RequestsPerRun: 300,
			Resolutions: config.DefaultCandleResolutions()},
	}
	specs, err := jobSpecs(nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"catalog:coingecko", "candles:partitions"}, names(specs), "this node does not reach CoinMarketCap")
	assert.Equal(t, "24h0m0s", specs[0].Interval.String())

	s := config.Config.MarketData.Sources["coinmarketcap"]
	s.Egress = ""
	config.Config.MarketData.Sources["coinmarketcap"] = s
	specs, err = jobSpecs(nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"catalog:coingecko", "catalog:coinmarketcap", "candles:partitions"}, names(specs))

	config.Config.MarketData.Sources["binance"] = config.MarketDataSource{Enabled: true, TimeoutSeconds: 20}
	config.Config.MarketData.Sources["nobitex"] = config.MarketDataSource{Enabled: true, TimeoutSeconds: 20, Egress: config.EgressDirect}
	config.Config.MarketData.Egress.Default = config.EgressNone
	specs, err = jobSpecs(nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"tickers:nobitex", "candles:nobitex:1h", "candles:partitions"}, names(specs),
		"a node in Iran without a route abroad collects the Iranian venues")
	assert.Equal(t, "2m0s", specs[0].Interval.String())
	assert.Equal(t, "1h0m0s", specs[1].Interval.String(), "no daily candles of Tehran's days")

	config.Config.MarketData.Sources = nil
	specs, err = jobSpecs(nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"candles:partitions"}, names(specs), "no source is enabled")
}
