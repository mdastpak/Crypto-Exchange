package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/region"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// iranRelease is a release for Iran with every Iran-only feature on.
func iranRelease(t *testing.T) Configuration {
	t.Helper()
	c := secureRelease(t)
	m := &c.Identity.MobileOwnership
	m.Provider, m.Endpoint, m.APIKey, m.SecretKey = MobileOwnershipJibit, DefaultJibitEndpoint, "k", "s"
	c.Notifications.SMS = SMSConfig{Provider: "kavenegar", Kavenegar: KavenegarConfig{APIKey: "a-key", OTPTemplate: "otp"}}
	c.MarketData.Sources = map[string]MarketDataSource{
		"nobitex": {Enabled: true, TimeoutSeconds: 20},
		"wallex":  {Enabled: true, TimeoutSeconds: 20},
		"binance": {Enabled: true, TimeoutSeconds: 20},
	}
	c.MarketData.API.Regions = []string{"global", "ir"}
	c.MarketData.Candles.Quotes = []string{"USDT", "IRT"}
	c.Sites.Default = "global"
	c.Sites.List = []SiteConfig{
		{ID: "global", Domain: "example.com", Locales: []string{"en"}},
		{ID: "ir", Domain: "example.ir", Countries: []string{"ir"}, Locales: []string{"fa"}},
	}
	return c
}

func TestRegion_IranAcceptsItsFeatures(t *testing.T) {
	c := iranRelease(t)
	assert.Equal(t, region.Iran, c.Region(), "the default region is Iran")
	assert.Empty(t, problemsOf(c))
}

func TestRegion_InternationalRefusesTheIranOnlyFeatures(t *testing.T) {
	c := iranRelease(t)
	c.Deployment.Region = "international"
	problems := problemsOf(c)
	for _, key := range []string{
		"identity.mobile_ownership.provider", "notifications.sms.provider", "market_data.sources.nobitex.enabled",
		"market_data.sources.wallex.enabled", "market_data.api.regions", "market_data.candles.quotes", "sites.list[1].countries",
	} {
		found := false
		for _, p := range problems {
			if strings.HasPrefix(p, key) {
				found = true
				assert.Contains(t, p, "deployment.region (DEPLOYMENT_REGION) is international", p)
				assert.Contains(t, p, "a feature of the ir region only", p)
			}
		}
		assert.True(t, found, "%s is not refused: %v", key, problems)
	}
	assert.Len(t, problems, 7, "binance is a global venue: %v", problems)
	assert.Contains(t, strings.Join(problems, "\n"),
		`identity.mobile_ownership.provider (IDENTITY_MOBILE_OWNERSHIP_PROVIDER) is "jibit", which turns on the Shahkar check of Iranian mobile numbers (#253)`)

	c.Identity.MobileOwnership.Provider = MobileOwnershipNone
	c.Notifications.SMS.Provider = ""
	c.MarketData.Sources["nobitex"] = MarketDataSource{TimeoutSeconds: 20}
	c.MarketData.Sources["wallex"] = MarketDataSource{TimeoutSeconds: 20}
	c.MarketData.API.Regions = []string{"global"}
	c.MarketData.Candles.Quotes = []string{"USDT"}
	c.Sites.List = c.Sites.List[:1]
	assert.Empty(t, problemsOf(c), "an international release without them starts")
}

func TestRegion_TheWorkerChecksItsFeatures(t *testing.T) {
	saved := Config
	t.Cleanup(func() { Config = saved })
	Config = validMarketWorker()
	require.NoError(t, ValidateMarketWorker())
	Config.Deployment.Region = "international"
	m := &Config.Identity.MobileOwnership
	m.Provider, m.Endpoint, m.APIKey, m.SecretKey = MobileOwnershipJibit, DefaultJibitEndpoint, "k", "s"
	err := ValidateMarketWorker()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "market_data.sources.nobitex.enabled")
	assert.NotContains(t, err.Error(), "identity.mobile_ownership", "the worker does not check the server's features")
}

func TestRegion_LoadAppliesTheDefaultsOfTheRegion(t *testing.T) {
	saved, savedPath := Config, configPath
	t.Cleanup(func() { Config, configPath = saved, savedPath })
	configPath = filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("server:\n  mode: debug\n"), 0o600))

	t.Setenv("DEPLOYMENT_REGION", "")
	require.NoError(t, Load())
	assert.Equal(t, region.Iran, Config.Region())
	assert.Equal(t, []string{"USDT", "USD", "IRT"}, Config.MarketData.Candles.Quotes)
	assert.Empty(t, Config.MarketData.API.Regions, "every region")

	t.Setenv("DEPLOYMENT_REGION", "International")
	require.NoError(t, Load())
	assert.Equal(t, region.International, Config.Region())
	assert.Equal(t, "international", Config.Deployment.Region)
	assert.Equal(t, []string{"USDT", "USD"}, Config.MarketData.Candles.Quotes, "no toman prices")
	assert.Equal(t, []string{"global"}, Config.MarketData.API.Regions, "no Iranian venues")

	t.Setenv("DEPLOYMENT_REGION", "europe")
	err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deployment.region (DEPLOYMENT_REGION)")
}
