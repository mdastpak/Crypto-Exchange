package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetConfig() {
	Config = Configuration{}
}

func TestSetAndGetConfigPath(t *testing.T) {
	original := configPath
	defer func() { configPath = original }()

	SetConfigPath("/tmp/custom-config.yaml")
	assert.Equal(t, "/tmp/custom-config.yaml", ConfigPath())
}

func TestSetAndGetMigrationsPath(t *testing.T) {
	original := migrationsPath
	defer func() { migrationsPath = original }()

	SetMigrationsPath("file:///tmp/migrations")
	assert.Equal(t, "file:///tmp/migrations", MigrationsPath())
}

func TestLoadConfig_FromExplicitFile(t *testing.T) {
	defer resetConfig()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yamlContent := `
server:
  ip: "127.0.0.1"
  port: "9999"
postgres:
  host: "db-host"
  port: "5432"
  user: "svc"
  password: "secret"
  dbname: "exchange"
`
	require.NoError(t, os.WriteFile(path, []byte(yamlContent), 0644))

	originalPath := configPath
	defer func() { configPath = originalPath }()
	SetConfigPath(path)

	LoadConfig()

	assert.Equal(t, "127.0.0.1", Config.Server.IP)
	assert.Equal(t, "9999", Config.Server.Port)
	assert.Equal(t, "db-host", Config.Postgres.Host)
	// setDefaults should have filled in zero-value fields
	assert.Equal(t, 10, Config.Server.ReadTimeout)
	assert.Equal(t, "disable", Config.Postgres.SSLMode)
}

func TestLoadConfig_FallsBackToEmbedded(t *testing.T) {
	defer resetConfig()

	originalPath := configPath
	defer func() { configPath = originalPath }()
	// Point at a file that does not exist so LoadConfig falls back to the
	// embedded config.example.yaml.
	SetConfigPath(filepath.Join(t.TempDir(), "does-not-exist.yaml"))

	LoadConfig()

	assert.NotEmpty(t, Config.Server.Port, "expected embedded config.example.yaml to be loaded")
	assert.NotEmpty(t, Config.Postgres.DBName)
}

func TestSetDefaults_FillsZeroValues(t *testing.T) {
	defer resetConfig()
	Config = Configuration{}

	setDefaults()

	assert.Equal(t, 10, Config.Server.ReadTimeout)
	assert.Equal(t, 10, Config.Server.WriteTimeout)
	assert.Equal(t, 120, Config.Server.IdleTimeout)
	assert.Equal(t, "debug", Config.Server.Mode)

	assert.Equal(t, 25, Config.Postgres.MaxOpenConns)
	assert.Equal(t, 5, Config.Postgres.MaxIdleConns)
	assert.Equal(t, 5, Config.Postgres.ConnMaxLifetime)
	assert.Equal(t, 5, Config.Postgres.ConnMaxIdleTime)
	assert.Equal(t, "disable", Config.Postgres.SSLMode)
	assert.Equal(t, 3, Config.Postgres.ConnectRetries)
	assert.Equal(t, 500, Config.Postgres.ConnectRetryDelayMs)

	assert.Equal(t, 10, Config.Redis.PoolSize)
	assert.Equal(t, 2, Config.Redis.MinIdleConns)
	assert.Equal(t, 3, Config.Redis.ConnectRetries)
	assert.Equal(t, 500, Config.Redis.ConnectRetryDelayMs)

	assert.Equal(t, 30, Config.WebSocket.PingInterval)
	assert.Equal(t, 1024*1024, Config.WebSocket.MaxMessageSize)

	assert.Equal(t, "UTC", Config.Exchange.Timezone)

	assert.Equal(t, 10, Config.Markets.DefaultMakerFeeBPS)
	assert.Equal(t, 20, Config.Markets.DefaultTakerFeeBPS)

	assert.Equal(t, 100, Config.PriceAggregator.AggregationInterval)
	assert.Equal(t, []int{1, 5, 60, 300}, Config.PriceAggregator.VWAPWindows)
	assert.Equal(t, 3.0, Config.PriceAggregator.OutlierThreshold)
	assert.Equal(t, 2, Config.PriceAggregator.MinProviders)
	assert.Equal(t, 5000, Config.PriceAggregator.StaleThreshold)

	assert.Equal(t, "info", Config.Observability.Logging.Level)
	assert.Equal(t, "json", Config.Observability.Logging.Format)
	assert.Equal(t, 9090, Config.Observability.Metrics.Port)
	assert.Equal(t, "/metrics", Config.Observability.Metrics.Path)
	assert.Equal(t, "encrypted-db-exchange", Config.Observability.Tracing.ServiceName)
	assert.Equal(t, 0.1, Config.Observability.Tracing.SampleRate)

	assert.Equal(t, 100, Config.RateLimits.REST.RequestsPerSecond)
	assert.Equal(t, 200, Config.RateLimits.REST.Burst)
	assert.Equal(t, 50, Config.RateLimits.WS.MessagesPerSecond)
	assert.Equal(t, 100, Config.RateLimits.WS.Burst)
	assert.Equal(t, 10, Config.RateLimits.Trading.OrdersPerSecond)
	assert.Equal(t, 20, Config.RateLimits.Trading.Burst)
	assert.Equal(t, 5, Config.RateLimits.Auth.LoginAttemptsPerMinute)
	assert.Equal(t, 10, Config.RateLimits.Auth.OTPRequestsPerHour)

	assert.Equal(t, 587, Config.Notifications.Email.Port)
	assert.Equal(t, 10, Config.Notifications.Email.Timeout)
	assert.Equal(t, DefaultOTPEmailSubject, Config.Notifications.Email.OTPSubject)
	assert.Equal(t, DefaultOTPEmailBody, Config.Notifications.Email.OTPBody)
	assert.Empty(t, Config.Notifications.Email.Host, "email stays disabled without a host")
	assert.Empty(t, Config.Notifications.SMS.Provider, "SMS stays disabled without a provider")
	assert.Equal(t, DefaultKavenegarBaseURL, Config.Notifications.SMS.Kavenegar.BaseURL)
	assert.Equal(t, 10, Config.Notifications.SMS.Kavenegar.Timeout)
	assert.Equal(t, 4, Config.Notifications.Worker.Concurrency)
	assert.Equal(t, 3, Config.Notifications.Worker.MaxAttempts)
}

func TestSetDefaults_DoesNotOverrideExplicitValues(t *testing.T) {
	defer resetConfig()
	Config = Configuration{}
	Config.Server.ReadTimeout = 42
	Config.Postgres.MaxOpenConns = 7
	Config.Postgres.ConnectRetries = 1
	Config.Exchange.Timezone = "America/New_York"

	setDefaults()

	assert.Equal(t, 42, Config.Server.ReadTimeout)
	assert.Equal(t, 7, Config.Postgres.MaxOpenConns)
	assert.Equal(t, 1, Config.Postgres.ConnectRetries)
	assert.Equal(t, "America/New_York", Config.Exchange.Timezone)
}

func TestOverlayEnvVars(t *testing.T) {
	defer resetConfig()
	Config = Configuration{}
	Config.PriceAggregator.Providers = []PriceProviderConfig{{Name: "binance"}}
	Config.Fiat.Providers = []FiatProviderConfig{{Name: "stripe"}}
	Config.Compliance.Sanctions.Providers = []SanctionsProviderConfig{{Name: "chainalysis"}}
	Config.Compliance.KYC.Providers = []KYCProviderConfig{{Name: "sumsub"}}

	envVars := map[string]string{
		"POSTGRES_PASSWORD":        "pgpass",
		"POSTGRES_HOST":            "pghost",
		"POSTGRES_PORT":            "5433",
		"POSTGRES_USER":            "pguser",
		"POSTGRES_DB":              "pgdb",
		"REDIS_PASSWORD":           "redispass",
		"REDIS_HOST":               "redishost",
		"REDIS_PORT":               "6380",
		"RABBITMQ_USER":            "mquser",
		"RABBITMQ_PASSWORD":        "mqpass",
		"RABBITMQ_HOST":            "mqhost",
		"RABBITMQ_PORT":            "5673",
		"JWT_ISSUER":               "issuer1",
		"OTP_SECRET":               "otp-secret",
		"SMTP_PASSWORD":            "smtppass",
		"KAVENEGAR_API_KEY":        "kavkey",
		"BINANCE_API_KEY":          "bkey",
		"BINANCE_API_SECRET":       "bsecret",
		"FIAT_PROVIDER_API_KEY":    "fkey",
		"FIAT_PROVIDER_API_SECRET": "fsecret",
		"CHAINALYSIS_API_KEY":      "ckey",
		"TRM_API_KEY":              "trmkey",
		"SUMSUB_API_KEY":           "skey",
		"ONFIDO_API_KEY":           "okey",
		"OTEL_ENDPOINT":            "http://otel:4318",
		"LOG_LEVEL":                "debug",
	}
	for k, v := range envVars {
		t.Setenv(k, v)
	}

	overlayEnvVars()

	assert.Equal(t, "pgpass", Config.Postgres.Password)
	assert.Equal(t, "pghost", Config.Postgres.Host)
	assert.Equal(t, "5433", Config.Postgres.Port)
	assert.Equal(t, "pguser", Config.Postgres.User)
	assert.Equal(t, "pgdb", Config.Postgres.DBName)
	assert.Equal(t, "redispass", Config.Redis.Password)
	assert.Equal(t, "redishost", Config.Redis.Host)
	assert.Equal(t, "6380", Config.Redis.Port)
	assert.Equal(t, "mquser", Config.RabbitMQ.Username)
	assert.Equal(t, "mqpass", Config.RabbitMQ.Password)
	assert.Equal(t, "mqhost", Config.RabbitMQ.Host)
	assert.Equal(t, "5673", Config.RabbitMQ.Port)
	assert.Equal(t, "issuer1", Config.JWT.Issuer)
	assert.Equal(t, "otp-secret", Config.OTP.Auth.Secret)
	assert.Equal(t, "smtppass", Config.Notifications.Email.Password)
	assert.Equal(t, "kavkey", Config.Notifications.SMS.Kavenegar.APIKey)
	assert.Equal(t, "bkey", Config.PriceAggregator.Providers[0].APIKey)
	assert.Equal(t, "bsecret", Config.PriceAggregator.Providers[0].APISecret)
	assert.Equal(t, "fkey", Config.Fiat.Providers[0].APIKey)
	assert.Equal(t, "fsecret", Config.Fiat.Providers[0].APISecret)
	assert.Equal(t, "ckey", Config.Compliance.Sanctions.Providers[0].APIKey)
	assert.Equal(t, "skey", Config.Compliance.KYC.Providers[0].APIKey)
	assert.Equal(t, "http://otel:4318", Config.Observability.Tracing.Endpoint)
	assert.Equal(t, "debug", Config.Observability.Logging.Level)
}

func TestOverlayEnvVars_NoEnvVarsSetLeavesConfigUnchanged(t *testing.T) {
	defer resetConfig()
	Config = Configuration{}
	Config.Postgres.Host = "unchanged-host"

	overlayEnvVars()

	assert.Equal(t, "unchanged-host", Config.Postgres.Host)
}

func TestValidateConfig_DoesNotPanicOnEmptyConfig(t *testing.T) {
	defer resetConfig()
	Config = Configuration{}

	assert.NotPanics(t, func() {
		validateConfig()
	})
}

func TestValidateConfig_DoesNotPanicWithProvidersConfigured(t *testing.T) {
	defer resetConfig()
	Config = Configuration{}
	Config.PriceAggregator.Enabled = true
	Config.PriceAggregator.Providers = []PriceProviderConfig{{Name: "binance", Enabled: true}}
	Config.Custody.Providers = []CustodyProviderConfig{{ProviderType: "eth", Enabled: true}}
	Config.Fiat.Providers = []FiatProviderConfig{{Name: "stripe", Enabled: true}}
	Config.Compliance.Sanctions.Providers = []SanctionsProviderConfig{{Name: "chainalysis", Enabled: true}}
	Config.Compliance.KYC.Providers = []KYCProviderConfig{{Name: "sumsub", Enabled: true}}
	Config.Postgres.Password = "x"
	Config.RabbitMQ.Username = "x"
	Config.RabbitMQ.Password = "x"
	Config.JWT.SSL.User.PrivateKey = []string{"a"}
	Config.JWT.SSL.User.PublicKey = []string{"b"}
	Config.Exchange.Name = "test-exchange"

	assert.NotPanics(t, func() {
		validateConfig()
	})
}

func TestJWTKeyPaths(t *testing.T) {
	defer resetConfig()
	c := &Configuration{}
	c.JWT.SSL.User.PrivateKey = []string{"config", "ssl", "user_private.pem"}
	c.JWT.SSL.User.PublicKey = []string{"config", "ssl", "user_public.pem"}

	assert.Equal(t, filepath.Join("config", "ssl", "user_private.pem"), c.JWTPrivateKeyPath())
	assert.Equal(t, filepath.Join("config", "ssl", "user_public.pem"), c.JWTPublicKeyPath())
}

func TestGetRabbitMQURL(t *testing.T) {
	defer resetConfig()
	Config = Configuration{}
	Config.RabbitMQ.Username = "guest"
	Config.RabbitMQ.Password = "guest"
	Config.RabbitMQ.Host = "localhost"
	Config.RabbitMQ.Port = "5672"

	url := GetRabbitMQURL()

	assert.Equal(t, "amqp://guest:guest@localhost:5672/", url)
}

func TestGetServerAddr(t *testing.T) {
	defer resetConfig()
	Config = Configuration{}
	Config.Server.IP = "0.0.0.0"
	Config.Server.Port = "8080"

	assert.Equal(t, "0.0.0.0:8080", GetServerAddr())
}

func TestGetServerTimeouts(t *testing.T) {
	defer resetConfig()
	Config = Configuration{}
	Config.Server.ReadTimeout = 5
	Config.Server.WriteTimeout = 7
	Config.Server.IdleTimeout = 60

	read, write, idle := GetServerTimeouts()

	assert.Equal(t, 5*time.Second, read)
	assert.Equal(t, 7*time.Second, write)
	assert.Equal(t, 60*time.Second, idle)
}
