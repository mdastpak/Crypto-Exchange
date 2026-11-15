package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validPriceAggregator() Configuration {
	var c Configuration
	floor := DefaultPriceOutlierFloorBPS
	a := &c.PriceAggregator
	a.Enabled = true
	a.HealthAddr = ":8082"
	a.AggregationInterval = 100
	a.VWAPWindows = []int{1, 5, 60, 300}
	a.OutlierThreshold = 3
	a.OutlierFloorBPS = &floor
	a.MinProviders = 2
	a.StaleThreshold = 5000
	a.CacheTTLMs = 500
	a.LeaseTTLMs = 2000
	a.Providers = []PriceProviderConfig{
		{Name: "binance", Enabled: true, Type: PriceProviderWebSocket, Symbols: []string{"BTC/USDT"}},
		{Name: "okx", Enabled: true, Type: PriceProviderREST, Symbols: []string{"BTC-USDT"}},
	}
	return c
}

func TestValidatePriceAggregator_NeedsOnlyItsSection(t *testing.T) {
	saved := Config
	defer func() { Config = saved }()
	Config = validPriceAggregator()
	assert.NoError(t, ValidatePriceAggregator(), "no database, Redis, or JWT setting is needed")
}

func TestValidatePriceAggregator_ReportsEveryProblem(t *testing.T) {
	saved := Config
	defer func() { Config = saved }()
	Config = validPriceAggregator()
	a := &Config.PriceAggregator
	a.HealthAddr = "8082"
	a.AggregationInterval = 5
	a.VWAPWindows = []int{0}
	a.OutlierThreshold = 0
	bad := -1
	a.OutlierFloorBPS = &bad
	a.MinProviders = 0
	a.StaleThreshold = 0
	a.Providers = append(a.Providers,
		PriceProviderConfig{Name: "bybit", Enabled: true, Type: PriceProviderWebSocket, Symbols: []string{"BTCUSDT"}},
		PriceProviderConfig{Name: "binance", Enabled: true, Type: "fix", WSEndpoint: "ws://insecure", RESTEndpoint: "http://insecure", Egress: "proxy"},
		PriceProviderConfig{Name: "acme", Enabled: true, Type: PriceProviderREST, Symbols: []string{"BTC/USDT"}, Egress: "tor"},
	)
	err := ValidatePriceAggregator()
	require.Error(t, err)
	for _, want := range []string{
		`price_aggregator.health_addr is "8082"`,
		"price_aggregator.aggregation_interval_ms is 5",
		"price_aggregator.vwap_windows_seconds holds 0",
		"price_aggregator.outlier_threshold_mad is 0",
		"price_aggregator.outlier_floor_bps is -1",
		"price_aggregator.min_providers is 0",
		"price_aggregator.stale_threshold_ms is 0",
		`price_aggregator.providers[2].type is websocket, but "bybit" has no stream`,
		`price_aggregator.providers[2].symbols holds "BTCUSDT"`,
		`price_aggregator.providers[3].name is "binance", which another provider has`,
		`price_aggregator.providers[3].type is "fix"`,
		"price_aggregator.providers[3].symbols is empty",
		`price_aggregator.providers[3].ws_endpoint is "ws://insecure"`,
		`price_aggregator.providers[3].rest_endpoint is "http://insecure"`,
		"price_aggregator.providers[3].egress is proxy, but market_data.egress.proxy_url",
		`price_aggregator.providers[4].type is rest, but "acme" has no REST API here`,
		`price_aggregator.providers[4].egress is "tor"`,
	} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestValidatePriceAggregator_NeedsAProvider(t *testing.T) {
	saved := Config
	defer func() { Config = saved }()
	Config = validPriceAggregator()
	for i := range Config.PriceAggregator.Providers {
		Config.PriceAggregator.Providers[i].Enabled = false
	}
	err := ValidatePriceAggregator()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "price_aggregator.providers has no enabled provider")

	Config.PriceAggregator.Enabled = false
	assert.NoError(t, ValidatePriceAggregator(), "a disabled aggregator needs none")
}

// The Iranian venues are price providers of the Iran region only.
func TestValidatePriceAggregator_IranianProvidersAreOfTheIranRegion(t *testing.T) {
	saved := Config
	defer func() { Config = saved }()
	Config = validPriceAggregator()
	Config.Deployment.Region = "international"
	Config.PriceAggregator.Providers = append(Config.PriceAggregator.Providers,
		PriceProviderConfig{Name: "nobitex", Enabled: true, Type: PriceProviderREST, Symbols: []string{"USDT/IRT"}})
	err := ValidatePriceAggregator()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "price_aggregator.providers[2].enabled")

	Config.Deployment.Region = "ir"
	assert.NoError(t, ValidatePriceAggregator())
}

func TestPriceProxyURL(t *testing.T) {
	var c Configuration
	c.MarketData.Egress.ProxyURL = "socks5://proxy:1080"
	assert.Empty(t, c.PriceProxyURL(PriceProviderConfig{}))
	assert.Equal(t, "socks5://proxy:1080", c.PriceProxyURL(PriceProviderConfig{Egress: EgressProxy}))
	c.MarketData.Egress.Default = EgressProxy
	assert.Equal(t, "socks5://proxy:1080", c.PriceProxyURL(PriceProviderConfig{}), "the default route of the node")
	assert.Empty(t, c.PriceProxyURL(PriceProviderConfig{Egress: EgressDirect}))
}

// The example file starts the price aggregator as it is, and with the
// Iranian venues of the Iran region turned on.
func TestConfigExample_StartsThePriceAggregator(t *testing.T) {
	require.NoError(t, loadFile(t, configFile("config.example.yaml"), map[string]string{
		"RABBITMQ_USER": "guest", "RABBITMQ_PASSWORD": "guest",
	}))
	require.NoError(t, ValidatePriceAggregator())
	assert.True(t, Config.PriceAggregator.PublishToRedis)
	assert.True(t, Config.PriceAggregator.PublishToRabbitMQ)
	enabled := 0
	for i, p := range Config.PriceAggregator.Providers {
		if p.Name == "nobitex" || p.Name == "wallex" {
			Config.PriceAggregator.Providers[i].Enabled = true
		}
		if Config.PriceAggregator.Providers[i].Enabled {
			enabled++
		}
	}
	assert.GreaterOrEqual(t, enabled, 3)
	assert.NoError(t, ValidatePriceAggregator())
	assert.Equal(t, DefaultPriceOutlierFloorBPS, *Config.PriceAggregator.OutlierFloorBPS)
}

func TestValidatePriceAggregator_ChecksThePublishing(t *testing.T) {
	saved := Config
	defer func() { Config = saved }()
	Config = validPriceAggregator()
	a := &Config.PriceAggregator
	a.CacheTTLMs = 50
	a.LeaseTTLMs = 100
	a.PublishToRabbitMQ = true
	err := ValidatePriceAggregator()
	require.Error(t, err)
	for _, want := range []string{
		"price_aggregator.cache_ttl_ms is 50, below aggregation_interval_ms (100)",
		"price_aggregator.lease_ttl_ms is 100; it must be from three intervals (300)",
		"price_aggregator.publish_to_rabbitmq needs publish_to_redis",
		"publish_to_rabbitmq needs: rabbitmq.host",
	} {
		assert.Contains(t, err.Error(), want)
	}

	Config = validPriceAggregator()
	Config.PriceAggregator.PublishToRedis = true
	err = ValidatePriceAggregator()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "publish_to_redis needs: redis.host")
	Config.Redis.Host, Config.Redis.Port = "localhost", "6379"
	Config.Redis.DurableWrites.TimeoutMs = 100
	assert.NoError(t, ValidatePriceAggregator())
}

// Release mode signs every price written to Redis (#450).
func TestValidatePriceAggregator_ReleaseNeedsTheSigningKey(t *testing.T) {
	saved := Config
	defer func() { Config = saved }()
	Config = validPriceAggregator()
	Config.PriceAggregator.PublishToRedis = true
	Config.Redis.Host, Config.Redis.Port = "localhost", "6379"
	Config.Redis.DurableWrites.TimeoutMs = 100
	assert.NoError(t, ValidatePriceAggregator(), "unsigned outside release mode")

	Config.Server.Mode = "release"
	Config.PriceAggregator.HealthAddr = "127.0.0.1:8082"
	Config.Redis.Password = "a-password"
	Config.Redis.TLS.Enabled = true
	err := ValidatePriceAggregator()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "price_aggregator.signing_key (PRICE_AGGREGATOR_SIGNING_KEY) is not set")

	Config.PriceAggregator.SigningKey = KeyPath(filepath.Join(t.TempDir(), "missing.pem"))
	err = ValidatePriceAggregator()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be read")

	key := filepath.Join(t.TempDir(), "price-signing.pem")
	require.NoError(t, os.WriteFile(key, []byte("-----BEGIN PRIVATE KEY-----\n"), 0o600))
	Config.PriceAggregator.SigningKey = KeyPath(key)
	assert.NoError(t, ValidatePriceAggregator())
}

// secureReleaseAggregator is a release aggregator that publishes to Redis
// and RabbitMQ with settings that pass every release check.
func secureReleaseAggregator(t *testing.T) Configuration {
	t.Helper()
	c := validPriceAggregator()
	c.Server.Mode = "release"
	a := &c.PriceAggregator
	a.HealthAddr = "10.0.3.7:8082"
	a.MinProviders = 2
	a.PublishToRedis, a.PublishToRabbitMQ = true, true
	key := filepath.Join(t.TempDir(), "price-signing.pem")
	require.NoError(t, os.WriteFile(key, []byte("-----BEGIN PRIVATE KEY-----\n"), 0o600))
	a.SigningKey = KeyPath(key)
	c.Redis.Host, c.Redis.Port, c.Redis.Password = "redis", "6379", "a-redis-password"
	c.Redis.DurableWrites.TimeoutMs = 100
	c.RabbitMQ.Host, c.RabbitMQ.Username, c.RabbitMQ.Password = "rabbitmq", "prices", "a-rabbit-password"
	c.RabbitMQ.Exchanges.PriceUpdates = "price_updates"
	c.Redis.TLS.Enabled, c.RabbitMQ.TLS.PrivateNetwork = true, true
	return c
}

// TestValidatePriceAggregator_ReleaseChecks runs the release checks of
// #454 one rule at a time, from a secure release configuration.
func TestValidatePriceAggregator_ReleaseChecks(t *testing.T) {
	saved := Config
	defer func() { Config = saved }()
	Config = secureReleaseAggregator(t)
	require.NoError(t, ValidatePriceAggregator())

	for name, tc := range map[string]struct {
		change func(c *Configuration)
		want   string
	}{
		"no Redis password": {func(c *Configuration) { c.Redis.Password = "" },
			"redis.password (REDIS_PASSWORD) is empty; release mode needs Redis to require a password"},
		"the guest user": {func(c *Configuration) { c.RabbitMQ.Username = "guest" },
			"rabbitmq.username (RABBITMQ_USER) and rabbitmq.password (RABBITMQ_PASSWORD) use the default guest account"},
		"the guest password": {func(c *Configuration) { c.RabbitMQ.Password = "guest" },
			"use the default guest account"},
		"every interface": {func(c *Configuration) { c.PriceAggregator.HealthAddr = ":8082" },
			"price_aggregator.health_addr is \":8082\", which listens on every interface"},
		"0.0.0.0": {func(c *Configuration) { c.PriceAggregator.HealthAddr = "0.0.0.0:8082" },
			"which listens on every interface"},
		"[::]": {func(c *Configuration) { c.PriceAggregator.HealthAddr = "[::]:8082" },
			"which listens on every interface"},
		"Redis without TLS": {func(c *Configuration) { c.Redis.TLS.Enabled = false },
			"redis.tls.enabled (REDIS_TLS_ENABLED) is false; release mode needs TLS for redis"},
		"RabbitMQ without TLS": {func(c *Configuration) { c.RabbitMQ.TLS.PrivateNetwork = false },
			"rabbitmq.tls.enabled (RABBITMQ_TLS_ENABLED) is false; release mode needs TLS for rabbitmq"},
		"a quorum of one": {func(c *Configuration) { c.PriceAggregator.MinProviders = 1 },
			"price_aggregator.min_providers is 1; release mode needs at least 2"},
	} {
		Config = secureReleaseAggregator(t)
		tc.change(&Config)
		err := ValidatePriceAggregator()
		if assert.Error(t, err, name) {
			assert.Contains(t, err.Error(), tc.want, name)
		}
	}

	// A host name and localhost are hosts.
	for _, addr := range []string{"127.0.0.1:8082", "localhost:8082", "price-aggregator.internal:8082", "[fd00::7]:8082"} {
		Config = secureReleaseAggregator(t)
		Config.PriceAggregator.HealthAddr = addr
		assert.NoError(t, ValidatePriceAggregator(), addr)
	}

	// The same Redis and RabbitMQ settings are refused by the server with
	// the same messages.
	var server problems
	Config = secureReleaseAggregator(t)
	Config.Redis.Password, Config.RabbitMQ.Username = "", "guest"
	Config.Redis.TLS.Enabled = false
	Config.releaseProblems(&server)
	clientTLSProblems(&server, "redis", Config.Redis.Host, Config.Redis.TLS, true)
	var agg problems
	Config.priceAggregatorProblems(&agg)
	for _, line := range agg {
		assert.Contains(t, server, line)
	}
	assert.Len(t, agg, 3)

	// Development settings still start.
	Config = secureReleaseAggregator(t)
	Config.Server.Mode = "debug"
	Config.PriceAggregator.HealthAddr, Config.PriceAggregator.MinProviders = ":8082", 1
	Config.Redis.Password, Config.RabbitMQ.Username, Config.RabbitMQ.Password = "", "guest", "guest"
	Config.Redis.TLS.Enabled, Config.RabbitMQ.TLS.PrivateNetwork = false, false
	assert.NoError(t, ValidatePriceAggregator())
}
