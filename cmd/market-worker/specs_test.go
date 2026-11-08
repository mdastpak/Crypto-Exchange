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
	}
	specs, err := jobSpecs(nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"catalog:coingecko"}, names(specs), "this node does not reach CoinMarketCap")
	assert.Equal(t, "24h0m0s", specs[0].Interval.String())

	s := config.Config.MarketData.Sources["coinmarketcap"]
	s.Egress = ""
	config.Config.MarketData.Sources["coinmarketcap"] = s
	specs, err = jobSpecs(nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"catalog:coingecko", "catalog:coinmarketcap"}, names(specs))

	config.Config.MarketData.Sources = nil
	specs, err = jobSpecs(nil)
	require.NoError(t, err)
	assert.Empty(t, specs, "no source is enabled")
}
