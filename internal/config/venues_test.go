package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveVenue is a live venue with the defaults applied.
func liveVenue() VenueConfig {
	return VenueConfig{Enabled: true, Mode: VenueModeLive, Egress: EgressDirect, RequestsPerMinute: 60,
		TimeoutSeconds: DefaultVenueTimeoutSeconds, RecvWindowMs: DefaultVenueRecvWindowMs, WithdrawTarget: VenueWithdrawOwnWallet,
		Assets: map[string]VenueAsset{"USDT": {Currency: "USDT", Networks: map[string]string{"TRX": "tron"}}}}
}

// openBaoVenues reads the credentials of exchanges from OpenBao.
func openBaoVenues(exchanges map[string]VenueConfig) VenuesConfig {
	return VenuesConfig{Exchanges: exchanges, Credentials: VenueCredentialsConfig{Provider: VenueCredentialsOpenBao,
		CacheSeconds: DefaultVenueCredentialsCacheSeconds, OpenBao: VenueOpenBaoConfig{Address: "https://openbao.internal:8200",
			TokenFile: "/run/secrets/openbao-token", Mount: DefaultVenueOpenBaoMount, Prefix: DefaultVenueOpenBaoPrefix,
			TimeoutSeconds: DefaultVenueOpenBaoTimeoutSeconds}}, State: defaultVenueState()}
}

// defaultVenueState is venues.state with its defaults.
func defaultVenueState() VenueStateConfig {
	return VenueStateConfig{IntervalSeconds: DefaultVenueStateIntervalSeconds, MaxAgeSeconds: DefaultVenueStateMaxAgeSeconds,
		ConfirmRuns: DefaultVenueStateConfirmRuns}
}

func TestVenues_ExampleAndDefaults(t *testing.T) {
	require.NoError(t, loadFile(t, configFile("config.example.yaml"), nil))
	var p problems
	Config.venueProblems(&p, false)
	assert.Empty(t, p, "the example configures its venues")
	b, ok := Config.Venues.Exchanges[VenueBinance]
	require.True(t, ok, "the example shows binance")
	assert.Equal(t, VenueModeTestnet, b.Mode)
	assert.Equal(t, VenueWithdrawOwnWallet, b.WithdrawTarget)
	assert.Equal(t, DefaultVenueRequestsPerMinuteBinance, b.RequestsPerMinute)
	assert.Equal(t, DefaultVenueOpenBaoMount, Config.Venues.Credentials.OpenBao.Mount)
	assert.Equal(t, DefaultVenueStateMaxAgeSeconds, Config.Venues.State.MaxAgeSeconds)
	assert.Equal(t, "0.01", Config.Venues.State.VenueTolerances()["USDT"].String())
}

func TestVenues_LoadReadsTheSecretsFromTheEnvironment(t *testing.T) {
	require.NoError(t, loadFile(t, configFile("config.example.yaml"), map[string]string{
		"VENUES_OPENBAO_TOKEN": "bao-token", "VENUES_PROXY_URL": "socks5://proxy.internal:1080",
	}))
	assert.Equal(t, "bao-token", Config.Venues.Credentials.OpenBao.Token)
	assert.Equal(t, "socks5://proxy.internal:1080", Config.Venues.ProxyURL)
}

func TestVenues_Problems(t *testing.T) {
	venue := func(c *Configuration, name string, change func(v *VenueConfig)) {
		v := liveVenue()
		change(&v)
		c.Venues = openBaoVenues(map[string]VenueConfig{name: v})
	}
	for name, tc := range map[string]struct {
		change func(c *Configuration)
		want   string
	}{
		"an unknown venue": {func(c *Configuration) { venue(c, "kraken", func(*VenueConfig) {}) },
			"venues.exchanges.kraken is not a venue with a connector"},
		"a fee above the bound": {func(c *Configuration) { venue(c, "binance", func(v *VenueConfig) { v.FeeBPS = 1001 }) },
			"venues.exchanges.binance.fee_bps is 1001; it must be from 0 to 1000"},
		"a negative slippage": {func(c *Configuration) { venue(c, "binance", func(v *VenueConfig) { v.SlippageBPS = -1 }) },
			"venues.exchanges.binance.slippage_bps is -1"},
		"an unknown mode": {func(c *Configuration) { venue(c, "binance", func(v *VenueConfig) { v.Mode = "paper" }) },
			`venues.exchanges.binance.mode is "paper"`},
		"nobitex on a testnet": {func(c *Configuration) { venue(c, "nobitex", func(v *VenueConfig) { v.Mode = VenueModeTestnet }) },
			"Nobitex has no testnet"},
		"a proxy without its address": {func(c *Configuration) { venue(c, "binance", func(v *VenueConfig) { v.Egress = EgressProxy }) },
			"venues.exchanges.binance.egress is proxy, but venues.proxy_url (VENUES_PROXY_URL) is not set"},
		"an http address": {func(c *Configuration) {
			venue(c, "binance", func(v *VenueConfig) { v.BaseURL = "http://api.binance.com" })
		},
			"venues.exchanges.binance.base_url"},
		"no request budget": {func(c *Configuration) { venue(c, "binance", func(v *VenueConfig) { v.RequestsPerMinute = -1 }) },
			"venues.exchanges.binance.requests_per_minute is -1"},
		"an unknown withdraw target": {func(c *Configuration) { venue(c, "binance", func(v *VenueConfig) { v.WithdrawTarget = "user" }) },
			`venues.exchanges.binance.withdraw_target is "user"`},
		"a sub-account at nobitex": {func(c *Configuration) { venue(c, "nobitex", func(v *VenueConfig) { v.SubAccount = "x" }) },
			"Nobitex has no sub-accounts"},
		"two assets of one currency": {func(c *Configuration) {
			venue(c, "binance", func(v *VenueConfig) { v.Assets["USDT2"] = VenueAsset{Currency: "usdt"} })
		}, "venues.exchanges.binance.assets.USDT2.currency is usdt, which USDT maps to as well"},
		"a deposit cost above the bound": {func(c *Configuration) { venue(c, "binance", func(v *VenueConfig) { v.DepositCostBPS = 1001 }) },
			"venues.exchanges.binance.deposit_cost_bps is 1001; it must be from 0 to 1000"},
		"a return network that is not mapped": {func(c *Configuration) {
			venue(c, "binance", func(v *VenueConfig) {
				a := v.Assets["USDT"]
				a.ReturnNetwork = "bsc"
				v.Assets["USDT"] = a
			})
		}, "venues.exchanges.binance.assets.USDT.return_network is bsc, which is not one of its networks"},
		"two networks of one network": {func(c *Configuration) {
			venue(c, "binance", func(v *VenueConfig) { v.Assets["USDT"].Networks["TRC20"] = "tron" })
		}, "venues.exchanges.binance.assets.USDT.networks.TRX is tron, which TRC20 maps to as well"},
		"credentials in a file": {func(c *Configuration) {
			venue(c, "binance", func(*VenueConfig) {})
			c.Venues.Credentials.Provider, c.Venues.Credentials.File = VenueCredentialsFile, "/etc/venues.json"
		}, "venues.credentials.provider (VENUES_CREDENTIALS_PROVIDER) is file; release mode reads the keys of the venues from OpenBao"},
		"no credentials": {func(c *Configuration) {
			venue(c, "binance", func(*VenueConfig) {})
			c.Venues.Credentials.Provider = ""
		}, `venues.credentials.provider (VENUES_CREDENTIALS_PROVIDER) is ""`},
		"OpenBao over http": {func(c *Configuration) {
			venue(c, "binance", func(*VenueConfig) {})
			c.Venues.Credentials.OpenBao.Address = "http://openbao.internal:8200"
		}, "release mode reaches OpenBao over https"},
		"OpenBao without a token": {func(c *Configuration) {
			venue(c, "binance", func(*VenueConfig) {})
			c.Venues.Credentials.OpenBao.TokenFile = ""
		}, "OpenBao needs one (VENUES_OPENBAO_TOKEN)"},
		"a state run too rare for its cutoff": {func(c *Configuration) {
			venue(c, "binance", func(*VenueConfig) {})
			c.Venues.State.MaxAgeSeconds = 90
		}, "venues.state.max_age_seconds is 90; it must be at least twice interval_seconds (120)"},
		"a state run too often": {func(c *Configuration) {
			venue(c, "binance", func(*VenueConfig) {})
			c.Venues.State.IntervalSeconds = 1
		}, "venues.state.interval_seconds is 1"},
		"no confirmation of a difference": {func(c *Configuration) {
			venue(c, "binance", func(*VenueConfig) {})
			c.Venues.State.ConfirmRuns = -1
		}, "venues.state.confirm_runs is -1"},
		"a tolerance that is not a decimal": {func(c *Configuration) {
			venue(c, "binance", func(*VenueConfig) {})
			c.Venues.State.Tolerances = map[string]string{"USDT": "-1"}
		}, "venues.state.tolerances.USDT is not a decimal"},
		"a proxy address that is not one": {func(c *Configuration) {
			venue(c, "binance", func(*VenueConfig) {})
			c.Venues.ProxyURL = "ftp://user:pass@proxy"
		}, "venues.proxy_url (VENUES_PROXY_URL) is not an http://"},
	} {
		t.Run(name, func(t *testing.T) {
			c := secureRelease(t)
			tc.change(&c)
			all := strings.Join(problemsOf(c), "\n")
			assert.Contains(t, all, tc.want)
			assert.NotContains(t, all, "user:pass", "a proxy address is never repeated")
		})
	}
}

func TestVenues_ADisabledVenueNeedsNoCredentials(t *testing.T) {
	c := secureRelease(t)
	v := liveVenue()
	v.Enabled = false
	c.Venues = VenuesConfig{Exchanges: map[string]VenueConfig{"binance": v}}
	assert.Empty(t, problemsOf(c))

	c.Server.Mode = "debug"
	c.Venues = openBaoVenues(map[string]VenueConfig{"binance": liveVenue()})
	c.Venues.Credentials.Provider, c.Venues.Credentials.File = VenueCredentialsFile, "venues.json"
	assert.Empty(t, problemsOf(c), "development reads the keys from a file")
}

func TestVenues_TheChecksOfANameMatchTheLists(t *testing.T) {
	for _, name := range []string{VenueBinance, VenueNobitex, "kraken", ""} {
		assert.Equal(t, slices.Contains(KnownVenues, name), IsKnownVenue(name), name)
		assert.Equal(t, slices.Contains(IranianVenues, name), IsIranianVenue(name), name)
	}
}
