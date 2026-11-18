package registry

import (
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/venues"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func venue(mode string) config.VenueConfig {
	return config.VenueConfig{Enabled: true, Mode: mode, Egress: config.EgressDirect, RequestsPerMinute: 60, TimeoutSeconds: 5,
		RecvWindowMs: 5000, WithdrawTarget: config.VenueWithdrawOwnWallet,
		Assets: map[string]config.VenueAsset{"USDT": {Currency: "USDT", Networks: map[string]string{"TRX": "tron"}}}}
}

func configWith(exchanges map[string]config.VenueConfig) *config.Configuration {
	var c config.Configuration
	c.Venues = config.VenuesConfig{Exchanges: exchanges, Credentials: config.VenueCredentialsConfig{
		Provider: config.VenueCredentialsFile, File: "venues.json", CacheSeconds: 60}}
	return &c
}

func TestFromConfig_BuildsTheEnabledVenuesInTheirMode(t *testing.T) {
	off := venue(config.VenueModeLive)
	off.Enabled = false
	got, err := FromConfig(configWith(map[string]config.VenueConfig{
		"binance": venue(config.VenueModeTestnet),
		"nobitex": venue(config.VenueModeDryRun),
	}), nil, nil)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, venues.ModeTestnet, got["binance"].Mode())
	assert.False(t, got["binance"].Supports(venues.CapabilityWithdraw), "the testnet has no withdrawals")
	assert.Equal(t, venues.ModeDryRun, got["nobitex"].Mode())
	_, ok := got["nobitex"].Venue.(*venues.DryRun)
	assert.True(t, ok)
	currency, network, ok := got["binance"].Mapping.Ours("USDT", "TRX")
	assert.True(t, ok)
	assert.Equal(t, "USDT", currency)
	assert.Equal(t, "tron", network)
	assert.Equal(t, config.VenueWithdrawOwnWallet, got["binance"].WithdrawTarget)

	none, err := FromConfig(configWith(map[string]config.VenueConfig{"binance": off}), nil, nil)
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestFromConfig_RefusesWhatCannotRun(t *testing.T) {
	_, err := FromConfig(configWith(map[string]config.VenueConfig{"nobitex": venue(config.VenueModeTestnet)}), nil, nil)
	assert.ErrorContains(t, err, "no testnet")
	bad := venue(config.VenueModeLive)
	bad.Assets["USDT2"] = config.VenueAsset{Currency: "USDT"}
	_, err = FromConfig(configWith(map[string]config.VenueConfig{"binance": bad}), nil, nil)
	assert.ErrorContains(t, err, "both map to")
	c := configWith(map[string]config.VenueConfig{"binance": venue(config.VenueModeLive)})
	c.Venues.Credentials.Provider = "vault-agent"
	_, err = FromConfig(c, nil, nil)
	assert.Error(t, err)
}

func TestUsageSource_IsAValidSourceName(t *testing.T) {
	// market_source_usage.source is ^[a-z0-9_]{1,40}$.
	for _, name := range config.KnownVenues {
		assert.Regexp(t, `^[a-z0-9_]{1,40}$`, UsageSource(name))
	}
}
