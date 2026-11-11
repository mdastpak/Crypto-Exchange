package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// secureRelease returns a configuration that release mode accepts, with the
// defaults that Load applies.
func secureRelease(t *testing.T) Configuration {
	t.Helper()
	saved := Config
	t.Cleanup(func() { Config = saved })

	Config = Configuration{}
	c := &Config
	c.Server.Mode, c.Server.Port = "release", "8080"
	c.Server.TLS.BehindProxy = true
	c.Server.TrustedProxies = []string{"10.0.0.0/8"}
	c.Server.CORS.AllowedOrigins = []string{"https://app.example.com"}
	c.WebSocket.AllowedOrigins = []string{"https://app.example.com"}
	c.Postgres.Host, c.Postgres.Port, c.Postgres.User, c.Postgres.DBName = "db", "5432", "exchange", "exchange"
	c.Postgres.Password, c.Postgres.SSLMode = "a-password-of-its-own", "verify-full"
	c.Postgres.Migrations.Mode = MigrationsModeExternal
	c.Redis.Host, c.Redis.Port, c.Redis.Password = "redis", "6379", "a-redis-password"
	c.RabbitMQ.Host, c.RabbitMQ.Port, c.RabbitMQ.Username, c.RabbitMQ.Password = "mq", "5672", "exchange", "an-mq-password"
	// One dependency with TLS, the other on an acknowledged private network.
	c.Redis.TLS.Enabled = true
	c.RabbitMQ.TLS.PrivateNetwork = true
	c.JWT.SSL.User.PrivateKey, c.JWT.SSL.User.PublicKey = "/run/secrets/private.pem", "/run/secrets/public.pem"
	c.JWT.AccessToken.Expiration, c.JWT.RefreshToken.Expiration = 15, 10080
	setDefaults()
	return Config
}

// problemsOf returns the problems that validateServer reports, one per line.
func problemsOf(c Configuration) []string {
	err := c.validateServer()
	if err == nil {
		return nil
	}
	return strings.Split(err.Error(), "\n")
}

func TestValidateServer_AcceptsASecureReleaseConfiguration(t *testing.T) {
	assert.Empty(t, problemsOf(secureRelease(t)))
}

// TestValidateServer_Rules breaks one rule at a time, starting from a
// configuration that passes, and checks that exactly that problem, naming
// the setting and its environment variable, is reported.
func TestValidateServer_Rules(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(c *Configuration)
		want   string
	}{
		// Release mode.
		"no TLS":                       {func(c *Configuration) { c.Server.TLS.BehindProxy = false }, "server.tls.enabled (SERVER_TLS_ENABLED) is false; release mode needs TLS"},
		"behind a proxy without proxy": {func(c *Configuration) { c.Server.TrustedProxies = nil }, "server.trusted_proxies (SERVER_TRUSTED_PROXIES) is empty although server.tls.behind_proxy is set"},
		"sslmode disable":              {func(c *Configuration) { c.Postgres.SSLMode = "disable" }, `postgres.sslmode (POSTGRES_SSLMODE) is "disable"`},
		"sslmode prefer":               {func(c *Configuration) { c.Postgres.SSLMode = "prefer" }, `postgres.sslmode (POSTGRES_SSLMODE) is "prefer"`},
		"sslmode allow":                {func(c *Configuration) { c.Postgres.SSLMode = "allow" }, `postgres.sslmode (POSTGRES_SSLMODE) is "allow"`},
		"empty database password":      {func(c *Configuration) { c.Postgres.Password = "" }, "postgres.password (POSTGRES_PASSWORD) is empty or the default"},
		"default database password":    {func(c *Configuration) { c.Postgres.Password = "postgres" }, "postgres.password (POSTGRES_PASSWORD) is empty or the default"},
		"migrations at startup":        {func(c *Configuration) { c.Postgres.Migrations.Mode = MigrationsModeStartup }, `postgres.migrations.mode (POSTGRES_MIGRATIONS_MODE) is "startup"; release mode needs "external"`},
		"migration password in server": {func(c *Configuration) {
			c.Postgres.Migrations.User, c.Postgres.Migrations.Password = "exchange_owner", "secret"
		}, "postgres.migrations.password (POSTGRES_MIGRATIONS_PASSWORD) is set; in release mode only the release step"},
		"no Redis password":         {func(c *Configuration) { c.Redis.Password = "" }, "redis.password (REDIS_PASSWORD) is empty"},
		"RabbitMQ guest user":       {func(c *Configuration) { c.RabbitMQ.Username = "guest" }, "rabbitmq.username (RABBITMQ_USER) and rabbitmq.password (RABBITMQ_PASSWORD) use the default guest account"},
		"RabbitMQ guest password":   {func(c *Configuration) { c.RabbitMQ.Password = "guest" }, "use the default guest account"},
		"Redis without TLS":         {func(c *Configuration) { c.Redis.TLS.Enabled = false }, "redis.tls.enabled (REDIS_TLS_ENABLED) is false; release mode needs TLS for redis: enable it, or set redis.tls.private_network (REDIS_TLS_PRIVATE_NETWORK)"},
		"RabbitMQ without TLS":      {func(c *Configuration) { c.RabbitMQ.TLS.PrivateNetwork = false }, "rabbitmq.tls.enabled (RABBITMQ_TLS_ENABLED) is false; release mode needs TLS for rabbitmq"},
		"Redis CA file missing":     {func(c *Configuration) { c.Redis.TLS.CAFile = "/does/not/exist.pem" }, "redis.tls cannot be used: read ca_file"},
		"IPv6 prefix too short":     {func(c *Configuration) { c.RateLimits.IPv6PrefixLength = 16 }, "rate_limits.ipv6_prefix_length (RATE_LIMITS_IPV6_PREFIX_LENGTH) is 16; it must be between 32 and 128"},
		"IPv6 prefix too long":      {func(c *Configuration) { c.RateLimits.IPv6PrefixLength = 129 }, "rate_limits.ipv6_prefix_length (RATE_LIMITS_IPV6_PREFIX_LENGTH) is 129"},
		"locales without English":   {func(c *Configuration) { c.I18n.Locales = []string{"fa"} }, `i18n.locales (I18N_LOCALES) and i18n.default_locale (I18N_DEFAULT_LOCALE) are not valid: the locales must include "en"`},
		"default locale not listed": {func(c *Configuration) { c.I18n.DefaultLocale = "ar" }, `default locale "ar" is not one of the locales (en, fa)`},
		"locale not a tag":          {func(c *Configuration) { c.I18n.Locales = []string{"en", "not a tag"} }, "not a BCP 47 language tag"},
		"GeoIP without a source":    {func(c *Configuration) { c.GeoIP.Enabled = true }, "geoip.enabled (GEOIP_ENABLED) is true, but neither geoip.header (GEOIP_HEADER) nor geoip.database_path (GEOIP_DATABASE_PATH) is set"},
		"GeoIP reload too often":    {func(c *Configuration) { c.GeoIP.ReloadIntervalSeconds = 1 }, "geoip.reload_interval_seconds (GEOIP_RELOAD_INTERVAL_SECONDS) is 1; it must be from 10 to 86400"},
		"GeoIP max age too long":    {func(c *Configuration) { c.GeoIP.MaxAgeDays = 400 }, "geoip.max_age_days (GEOIP_MAX_AGE_DAYS) is 400; it must be from 1 to 365"},
		"GeoIP overrides released":  {func(c *Configuration) { c.GeoIP.Overrides = map[string]string{"10.0.0.0/8": "IR"} }, "geoip.overrides is set; it fixes the country of addresses for local tests, and release mode refuses it"},
		"RabbitMQ half a client pair": {func(c *Configuration) {
			c.RabbitMQ.TLS.Enabled, c.RabbitMQ.TLS.CertFile = true, "/run/secrets/client.pem"
		}, "rabbitmq.tls cannot be used: cert_file and key_file must be set together"},
		"CORS wildcard":       {func(c *Configuration) { c.Server.CORS.AllowedOrigins = []string{"*"} }, `server.cors.allowed_origins (SERVER_CORS_ALLOWED_ORIGINS) contains "*"`},
		"CORS over http":      {func(c *Configuration) { c.Server.CORS.AllowedOrigins = []string{"http://app.example.com"} }, `contains "http://app.example.com"; release mode allows only https origins`},
		"WebSocket wildcard":  {func(c *Configuration) { c.WebSocket.AllowedOrigins = []string{" * "} }, `websocket.allowed_origins (WS_ALLOWED_ORIGINS) contains "*"`},
		"WebSocket over http": {func(c *Configuration) { c.WebSocket.AllowedOrigins = []string{"http://localhost:3000"} }, `websocket.allowed_origins (WS_ALLOWED_ORIGINS) contains "http://localhost:3000"`},

		// Ranges, in every mode.
		"admin network not a range":        {func(c *Configuration) { c.Admin.AllowedCIDRs = []string{"10.0.0.0/8", "office"} }, `admin.allowed_cidrs (ADMIN_ALLOWED_CIDRS) has "office", which is neither a CIDR range nor an IP address`},
		"admin address without a port":     {func(c *Configuration) { c.Server.AdminListenAddr = "10.0.0.5" }, `server.admin_listen_addr (SERVER_ADMIN_LISTEN_ADDR) is "10.0.0.5"; it must be host:port`},
		"admin address of the server":      {func(c *Configuration) { c.Server.AdminListenAddr = "0.0.0.0:" + c.Server.Port }, "which is the address of the server"},
		"port zero":                        {func(c *Configuration) { c.Server.Port = "0" }, `server.port (SERVER_PORT) is "0"; it must be a port number from 1 to 65535`},
		"port too large":                   {func(c *Configuration) { c.Postgres.Port = "65536" }, `postgres.port (POSTGRES_PORT) is "65536"`},
		"port not a number":                {func(c *Configuration) { c.Redis.Port = "redis" }, `redis.port (REDIS_PORT) is "redis"`},
		"negative read timeout":            {func(c *Configuration) { c.Server.ReadTimeout = -1 }, "server.read_timeout_seconds (SERVER_READ_TIMEOUT_SECONDS) is -1; it must be at least 1"},
		"no shutdown time":                 {func(c *Configuration) { c.Server.ShutdownTimeout = -1 }, "server.shutdown_timeout_seconds (SERVER_SHUTDOWN_TIMEOUT_SECONDS) is -1"},
		"drain as long as the shutdown":    {func(c *Configuration) { c.Server.DrainSeconds = 30 }, "server.drain_seconds (SERVER_DRAIN_SECONDS) is 30; it must be less than server.shutdown_timeout_seconds (30)"},
		"negative body limit":              {func(c *Configuration) { c.Server.MaxBodyBytes = -1 }, "server.max_body_bytes (SERVER_MAX_BODY_BYTES) is -1"},
		"no database connections":          {func(c *Configuration) { c.Postgres.MaxOpenConns = -3 }, "postgres.max_open_conns (POSTGRES_MAX_OPEN_CONNS) is -3"},
		"more idle than open":              {func(c *Configuration) { c.Postgres.MaxIdleConns = 30 }, "postgres.max_idle_conns (POSTGRES_MAX_IDLE_CONNS) is 30; it must not exceed postgres.max_open_conns (25)"},
		"negative Redis pool":              {func(c *Configuration) { c.Redis.PoolSize = -1 }, "redis.pool_size (REDIS_POOL_SIZE) is -1"},
		"more Redis idle than pool":        {func(c *Configuration) { c.Redis.MinIdleConns = 11 }, "redis.min_idle_conns (REDIS_MIN_IDLE_CONNS) is 11; it must not exceed redis.pool_size (10)"},
		"access token over a day":          {func(c *Configuration) { c.JWT.AccessToken.Expiration = 1441 }, "jwt.access_token.expiration_minutes (JWT_ACCESS_TOKEN_EXPIRATION_MINUTES) is 1441; it must be at most 1440"},
		"session lifetime below refresh":   {func(c *Configuration) { c.JWT.Sessions.MaxLifetimeMinutes = 10079 }, "jwt.sessions.max_lifetime_minutes (JWT_SESSIONS_MAX_LIFETIME_MINUTES) is 10079; it must be at least jwt.refresh_token.expiration_minutes (10080)"},
		"no session lifetime":              {func(c *Configuration) { c.JWT.Sessions.MaxLifetimeMinutes = -1 }, "jwt.sessions.max_lifetime_minutes (JWT_SESSIONS_MAX_LIFETIME_MINUTES) is -1; it must be at least 1"},
		"refresh shorter than access":      {func(c *Configuration) { c.JWT.RefreshToken.Expiration = 10 }, "jwt.refresh_token.expiration_minutes (JWT_REFRESH_TOKEN_EXPIRATION_MINUTES) is 10; it must be at least jwt.access_token.expiration_minutes (15)"},
		"negative ping interval":           {func(c *Configuration) { c.WebSocket.PingInterval = -1 }, "websocket.ping_interval_seconds (WS_PING_INTERVAL_SECONDS) is -1"},
		"WebSocket message over 1 MiB":     {func(c *Configuration) { c.WebSocket.MaxMessageSize = 2 << 20 }, "websocket.max_message_size_bytes (WS_MAX_MESSAGE_SIZE_BYTES) is 2097152; it must be at most 1048576"},
		"negative WebSocket write timeout": {func(c *Configuration) { c.WebSocket.WriteTimeout = -1 }, "websocket.write_timeout_seconds (WS_WRITE_TIMEOUT_SECONDS) is -1"},
		"negative client message rate":     {func(c *Configuration) { c.WebSocket.ClientMessagesPerSecond = -1 }, "websocket.client_messages_per_second (WS_CLIENT_MESSAGES_PER_SECOND) is -1"},
		"more per address than in total":   {func(c *Configuration) { c.WebSocket.MaxConnectionsPerIP = 20000 }, "websocket.max_connections_per_ip (WS_MAX_CONNECTIONS_PER_IP) is 20000; it must not exceed websocket.max_connections (10000)"},
		"negative worker count":            {func(c *Configuration) { c.Notifications.Worker.Concurrency = -1 }, "notifications.worker.concurrency is -1"},
		"unknown migrations mode":          {func(c *Configuration) { c.Postgres.Migrations.Mode = "later" }, `postgres.migrations.mode (POSTGRES_MIGRATIONS_MODE) is "later"; it must be one of startup, external`},
		"migration password without user":  {func(c *Configuration) { c.Server.Mode, c.Postgres.Migrations.Password = "debug", "secret" }, "postgres.migrations.password (POSTGRES_MIGRATIONS_PASSWORD) is set without postgres.migrations.user"},
		"no contact cooldown":              {func(c *Configuration) { c.Contacts.NewContactCooldownHours = -1 }, "contacts.new_contact_cooldown_hours (CONTACTS_NEW_CONTACT_COOLDOWN_HOURS) is -1; it must be from 1 to 720"},
		"contact cooldown over 30 days":    {func(c *Configuration) { c.Contacts.NewContactCooldownHours = 721 }, "contacts.new_contact_cooldown_hours (CONTACTS_NEW_CONTACT_COOLDOWN_HOURS) is 721"},
		"no security hold":                 {func(c *Configuration) { c.Security.ChangeHoldHours = -1 }, "security.change_hold_hours (SECURITY_CHANGE_HOLD_HOURS) is -1; it must be from 1 to 720"},
		"stub ownership in release":        {func(c *Configuration) { c.Identity.MobileOwnership.Provider = MobileOwnershipStub }, "identity.mobile_ownership.provider (IDENTITY_MOBILE_OWNERSHIP_PROVIDER) is \"stub\""},
		"unknown ownership provider":       {func(c *Configuration) { c.Identity.MobileOwnership.Provider = "shahkar" }, `identity.mobile_ownership.provider (IDENTITY_MOBILE_OWNERSHIP_PROVIDER) is "shahkar"; it must be one of none, stub, jibit`},
		"jibit without credentials": {func(c *Configuration) {
			c.Identity.MobileOwnership.Provider, c.Identity.MobileOwnership.Endpoint = MobileOwnershipJibit, DefaultJibitEndpoint
		}, "must both be set for the jibit provider"},
		"jibit over http": {func(c *Configuration) {
			m := &c.Identity.MobileOwnership
			m.Provider, m.Endpoint, m.APIKey, m.SecretKey = MobileOwnershipJibit, "http://napi.jibit.ir/ide", "k", "s"
		}, `identity.mobile_ownership.endpoint (IDENTITY_MOBILE_OWNERSHIP_ENDPOINT) is "http://napi.jibit.ir/ide"; it must be an https URL`},
		"ownership timeout over a minute": {func(c *Configuration) { c.Identity.MobileOwnership.TimeoutSeconds = 61 }, "identity.mobile_ownership.timeout_seconds (IDENTITY_MOBILE_OWNERSHIP_TIMEOUT_SECONDS) is 61"},
		"negative rate limit":             {func(c *Configuration) { c.RateLimits.Auth.OTPRequestsPerHour = -1 }, "rate_limits.auth.otp_requests_per_hour (RATE_LIMITS_AUTH_OTP_REQUESTS_PER_HOUR) is -1"},

		// The market data routes (#269).
		"unknown market data region":    {func(c *Configuration) { c.MarketData.API.Regions = []string{"global", "eu"} }, `market_data.api.regions has "eu"; the regions are global and ir`},
		"market data cache over 1 hour": {func(c *Configuration) { c.MarketData.API.CacheSeconds = 3601 }, "market_data.api.cache_seconds is 3601; it must be from 1 to 3600"},
		"outlier share over 100":        {func(c *Configuration) { c.MarketData.Tickers.OutlierPercent = 101 }, "market_data.tickers.outlier_percent is 101; it must be from 1 to 100"},
		"median of one venue":           {func(c *Configuration) { c.MarketData.Tickers.MinVenues = 1 }, "market_data.tickers.min_venues is 1; it must be from 2 to 20"},

		// TLS files, in every mode.
		"TLS without files": {func(c *Configuration) { c.Server.TLS.Enabled = true }, "server.tls.enabled (SERVER_TLS_ENABLED) is true, but server.tls.cert_file (SERVER_TLS_CERT_FILE) and server.tls.key_file (SERVER_TLS_KEY_FILE) must both be set"},
	} {
		t.Run(name, func(t *testing.T) {
			c := secureRelease(t)
			tc.change(&c)
			got := problemsOf(c)
			require.Len(t, got, 1, "exactly one problem: %q", got)
			assert.Contains(t, got[0], tc.want)
		})
	}
}

func TestValidateServer_ReleaseRulesOnlyApplyInReleaseMode(t *testing.T) {
	c := secureRelease(t)
	c.Server.Mode = "debug"
	c.Server.TLS.BehindProxy = false
	c.Postgres.SSLMode, c.Postgres.Password, c.Redis.Password = "disable", "", ""
	c.RabbitMQ.Username, c.RabbitMQ.Password = "guest", "guest"
	c.Server.CORS.AllowedOrigins = []string{"http://localhost:3000"}
	c.WebSocket.AllowedOrigins = []string{"*"}
	c.Postgres.Migrations.Mode = MigrationsModeStartup
	assert.Empty(t, problemsOf(c), "development settings are fine outside release mode")

	c.Server.Mode = "release"
	assert.Len(t, problemsOf(c), 8, "release mode lists every problem at once")
}

// writeKeyPair writes a self-signed certificate and its key, and returns
// their paths.
func writeKeyPair(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	certPath, keyPath := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certPath, keyPath
}

func TestValidateServer_ChecksTheTLSFiles(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeKeyPair(t, dir, "server")
	_, otherKey := writeKeyPair(t, dir, "other")

	c := secureRelease(t)
	c.Server.TLS.Enabled, c.Server.TLS.BehindProxy = true, false
	c.Server.TLS.CertFile, c.Server.TLS.KeyFile = cert, key
	assert.Empty(t, problemsOf(c), "a matching pair serves TLS, which release mode accepts")

	c.Server.TLS.KeyFile = otherKey
	got := problemsOf(c)
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "server.tls.cert_file (SERVER_TLS_CERT_FILE) and server.tls.key_file do not hold a usable certificate and key")

	c.Server.TLS.KeyFile = filepath.Join(dir, "missing.key")
	got = problemsOf(c)
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "missing.key")
}

func TestValidateServer_RefusesKeyFilesThatOthersMayWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not checked on Windows")
	}
	path := filepath.Join(t.TempDir(), "private.pem")
	require.NoError(t, os.WriteFile(path, []byte("key"), 0o600))
	c := secureRelease(t)
	c.JWT.SSL.User.PrivateKey = KeyPath(path)

	for _, mode := range []os.FileMode{0o600, 0o640, 0o644, 0o444} {
		require.NoError(t, os.Chmod(path, mode))
		assert.Empty(t, problemsOf(c), "mode %v: read access is not checked", mode)
	}
	for _, mode := range []os.FileMode{0o620, 0o602, 0o666} {
		require.NoError(t, os.Chmod(path, mode))
		got := problemsOf(c)
		require.Len(t, got, 1, "mode %v", mode)
		assert.Contains(t, got[0], "jwt.ssl.user.private_key (JWT_SSL_USER_PRIVATE_KEY) is "+path)
		assert.Contains(t, got[0], "writable by other users")
	}
}

// TestValidateServer_RedisModes checks the settings of each redis.mode
// (#231): a valid configuration of each mode passes without host and port
// outside the standalone mode, and each broken rule is reported alone.
func TestValidateServer_RedisModes(t *testing.T) {
	sentinel := func(c *Configuration) {
		c.Redis.Mode, c.Redis.Host, c.Redis.Port = RedisModeSentinel, "", ""
		c.Redis.Sentinel.MasterName = "crypto-exchange"
		c.Redis.Sentinel.Addresses = []string{"s-1:26379", " s-2:26379", "[2001:db8::3]:26379"}
	}
	cluster := func(c *Configuration) {
		c.Redis.Mode, c.Redis.Host, c.Redis.Port = RedisModeCluster, "", ""
		c.Redis.Cluster.Addresses = []string{"r-1:6379", "r-2:6379"}
	}
	for name, change := range map[string]func(c *Configuration){"sentinel": sentinel, "cluster": cluster} {
		c := secureRelease(t)
		change(&c)
		assert.Empty(t, problemsOf(c), name)
	}

	for name, tc := range map[string]struct {
		change func(c *Configuration)
		want   string
	}{
		"unknown mode":                {func(c *Configuration) { c.Redis.Mode = "ring" }, `redis.mode (REDIS_MODE) is "ring"; it must be one of standalone, sentinel, cluster`},
		"sentinel without a name":     {func(c *Configuration) { sentinel(c); c.Redis.Sentinel.MasterName = "" }, "redis.sentinel.master_name (REDIS_SENTINEL_MASTER_NAME) is empty"},
		"sentinel without sentinels":  {func(c *Configuration) { sentinel(c); c.Redis.Sentinel.Addresses = nil }, "redis.sentinel.addresses (REDIS_SENTINEL_ADDRESSES) is empty; redis.mode sentinel needs"},
		"sentinel without a port":     {func(c *Configuration) { sentinel(c); c.Redis.Sentinel.Addresses[1] = "s-2" }, `redis.sentinel.addresses (REDIS_SENTINEL_ADDRESSES) entry 2 is "s-2"`},
		"cluster without nodes":       {func(c *Configuration) { cluster(c); c.Redis.Cluster.Addresses = nil }, "redis.cluster.addresses (REDIS_CLUSTER_ADDRESSES) is empty; redis.mode cluster needs"},
		"cluster with a bad port":     {func(c *Configuration) { cluster(c); c.Redis.Cluster.Addresses[0] = "r-1:0" }, `entry 1 is "r-1:0"`},
		"cluster with database 2":     {func(c *Configuration) { cluster(c); c.Redis.DB = 2 }, "redis.db (REDIS_DB) is 2; Redis Cluster has only database 0"},
		"standalone with a cluster":   {func(c *Configuration) { c.Redis.Cluster.Addresses = []string{"r-1:6379"} }, "redis.mode (REDIS_MODE) is standalone, but redis.sentinel.addresses or redis.cluster.addresses is set"},
		"cluster with a sentinel":     {func(c *Configuration) { cluster(c); c.Redis.Sentinel.MasterName = "x" }, `redis.sentinel.master_name (REDIS_SENTINEL_MASTER_NAME) or the sentinel credentials are set, but redis.mode is "cluster"`},
		"negative durable replicas":   {func(c *Configuration) { c.Redis.DurableWrites.Replicas = -1 }, "redis.durable_writes.replicas (REDIS_DURABLE_WRITES_REPLICAS) is -1"},
		"negative durable wait":       {func(c *Configuration) { c.Redis.DurableWrites.TimeoutMs = -5 }, "redis.durable_writes.timeout_ms (REDIS_DURABLE_WRITES_TIMEOUT_MS) is -5; it must be at least 1"},
		"standalone without its host": {func(c *Configuration) { c.Redis.Host = "" }, "required settings are not set: redis.host (REDIS_HOST)"},
	} {
		t.Run(name, func(t *testing.T) {
			c := secureRelease(t)
			tc.change(&c)
			got := problemsOf(c)
			require.Len(t, got, 1, "exactly one problem: %q", got)
			assert.Contains(t, got[0], tc.want)
		})
	}
}

func TestValidateMarketWorker_NeedsOnlyPostgresAndItsSection(t *testing.T) {
	saved := Config
	defer func() { Config = saved }()

	Config = Configuration{}
	Config.Postgres.Host, Config.Postgres.Port, Config.Postgres.User, Config.Postgres.DBName = "db", "5432", "app", "exchange"
	Config.MarketData = MarketDataConfig{Mode: MarketDataModeDryRun, HealthAddr: ":8081",
		PollSeconds: 5, LeaseSeconds: 60, MaxConcurrentJobs: 4, RetryBaseSeconds: 30,
		Egress: MarketDataEgress{Default: EgressDirect}, Catalog: validCatalog(), Tickers: validTickers(), Candles: validCandles(),
		Tiers: validTiers(), Streams: validStreams(), API: MarketDataAPI{CacheSeconds: 30}}
	assert.NoError(t, ValidateMarketWorker(), "no JWT, Redis, or RabbitMQ setting is needed")

	Config.Postgres.User = ""
	Config.MarketData.Mode = "shadow"
	Config.MarketData.HealthAddr = "8081"
	Config.MarketData.LeaseSeconds = 5
	err := ValidateMarketWorker()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "postgres.user")
	assert.Contains(t, err.Error(), "market_data.mode")
	assert.Contains(t, err.Error(), "market_data.health_addr")
	assert.Contains(t, err.Error(), "market_data.lease_seconds")
}

func validCatalog() MarketDataCatalog {
	return MarketDataCatalog{Priority: []string{"coingecko", "coinmarketcap"}, Coverage: CatalogCoverageList,
		Top: 500, IntervalHours: 24, DetailsPerRun: 200, DetailsMaxAgeDays: 30, DeactivateAfterDays: 14}
}

func validTickers() MarketDataTickers {
	return MarketDataTickers{IntervalSeconds: 120, StaleAfterSeconds: 600, OutlierPercent: 5, MinVenues: 3,
		MapRankLimit: 1000, DeactivatePairsAfterHours: 24}
}

func validCandles() MarketDataCandles {
	return MarketDataCandles{Quotes: []string{"USDT"}, MaxPairsPerVenue: 200, RequestsPerRun: 300,
		Resolutions: DefaultCandleResolutions()}
}

func validTiers() MarketDataTiers {
	hotListed := true
	return MarketDataTiers{IntervalHours: 24, HotAssets: DefaultHotAssets(), HotListed: &hotListed, ActiveTop: 200, HysteresisPercent: 20}
}

func validStreams() MarketDataStreams {
	enabled := true
	return MarketDataStreams{Enabled: &enabled, Tiers: []string{TierHot}, MaxPairs: 200, MaxPerWorker: 2, RunMinutes: 60,
		FlushMillis: 1000, QuietSeconds: 60}
}

func TestValidateMarketWorker_ChecksTheStreams(t *testing.T) {
	saved := Config
	defer func() { Config = saved }()

	Config = validMarketWorker()
	Config.MarketData.Streams.Tiers = []string{"hot", "warm"}
	Config.MarketData.Streams.FlushMillis = 100
	Config.MarketData.Streams.MaxPerWorker = 17
	err := ValidateMarketWorker()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `market_data.streams.tiers has "warm"`)
	assert.Contains(t, err.Error(), "market_data.streams.flush_millis is 100; it must be from 250 to 60000")
	assert.Contains(t, err.Error(), "market_data.streams.max_per_worker is 17")
}

func TestValidateMarketWorker_ChecksTheTiers(t *testing.T) {
	saved := Config
	defer func() { Config = saved }()

	Config = validMarketWorker()
	Config.MarketData.Tiers.ActiveTop = 10001
	Config.MarketData.Tiers.HysteresisPercent = 101
	resolutions := DefaultCandleResolutions()
	resolutions["1m"] = MarketDataResolution{Enabled: true, IntervalMinutes: 1, BackfillDays: 1}
	resolutions["15m"] = MarketDataResolution{Enabled: true, IntervalMinutes: 15, BackfillDays: 1, Tiers: []string{"cold"}}
	resolutions["4h"] = MarketDataResolution{Enabled: true, IntervalMinutes: 240, BackfillDays: 1, Tiers: []string{TierHot}}
	Config.MarketData.Candles.Resolutions = resolutions
	err := ValidateMarketWorker()
	require.Error(t, err)
	for _, want := range []string{
		"market_data.tiers.active_top is 10001",
		"market_data.tiers.hysteresis_percent is 101",
		"market_data.candles.resolutions.1m.tiers is empty",
		`market_data.candles.resolutions.15m.tiers has "cold"`,
		"market_data.candles.resolutions.4h is not a resolution",
	} {
		assert.Contains(t, err.Error(), want)
	}
}

func validMarketWorker() Configuration {
	var c Configuration
	c.Postgres.Host, c.Postgres.Port, c.Postgres.User, c.Postgres.DBName = "db", "5432", "app", "exchange"
	c.MarketData = MarketDataConfig{Mode: MarketDataModeLive, HealthAddr: ":8081",
		PollSeconds: 5, LeaseSeconds: 60, MaxConcurrentJobs: 4, RetryBaseSeconds: 30,
		Egress:  MarketDataEgress{Default: EgressProxy, ProxyURL: "socks5://user:secret@gw.example:1080"},
		Catalog: validCatalog(),
		Tickers: validTickers(),
		Candles: validCandles(),
		Tiers:   validTiers(),
		Streams: validStreams(),
		API:     MarketDataAPI{CacheSeconds: 30},
		Sources: map[string]MarketDataSource{
			"coingecko": {Enabled: true, RequestsPerMinute: 30, MonthlyRequests: 10000, TimeoutSeconds: 20},
			"nobitex":   {Enabled: true, Egress: EgressDirect, TimeoutSeconds: 20},
			"binance":   {Enabled: false, TimeoutSeconds: 20},
		}}
	return c
}

func TestMarketData_RouteOfEachSource(t *testing.T) {
	m := validMarketWorker().MarketData
	assert.Equal(t, EgressProxy, m.Route("coingecko"), "the node's default")
	assert.Equal(t, EgressDirect, m.Route("nobitex"), "a source's own route wins")
	assert.Equal(t, EgressNone, m.Route("binance"), "a disabled source is not called")
	assert.Equal(t, EgressNone, m.Route("kraken"), "an unlisted source is not called")
	assert.True(t, m.Reachable("nobitex"))
	assert.False(t, m.Reachable("binance"))

	m.Egress.Default = EgressNone
	assert.False(t, m.Reachable("coingecko"))
	assert.True(t, m.Reachable("nobitex"))
}

func TestValidateMarketWorker_ChecksTheEgressAndTheSources(t *testing.T) {
	saved := Config
	defer func() { Config = saved }()

	Config = validMarketWorker()
	require.NoError(t, ValidateMarketWorker())

	Config.MarketData.Egress.ProxyURL = ""
	err := ValidateMarketWorker()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "market_data.sources.coingecko goes through the proxy")

	Config = validMarketWorker()
	Config.MarketData.Egress.ProxyURL = "ftp://user:secret@gw.example"
	Config.MarketData.Egress.Default = "vpn"
	Config.MarketData.Sources["Bad Name"] = MarketDataSource{TimeoutSeconds: 20}
	s := Config.MarketData.Sources["nobitex"]
	s.Egress, s.BaseURL, s.TimeoutSeconds, s.RequestsPerMinute = "tor", "http://api.nobitex.ir", 0, -1
	Config.MarketData.Sources["nobitex"] = s
	err = ValidateMarketWorker()
	require.Error(t, err)
	for _, want := range []string{"market_data.egress.proxy_url", "market_data.egress.default",
		"market_data.sources.Bad Name", "market_data.sources.nobitex.egress", "market_data.sources.nobitex.base_url",
		"market_data.sources.nobitex.timeout_seconds", "negative request limit"} {
		assert.Contains(t, err.Error(), want)
	}
	assert.NotContains(t, err.Error(), "secret", "the proxy credentials are never repeated")
}

func TestValidateMarketWorker_ChecksTheCatalog(t *testing.T) {
	saved := Config
	defer func() { Config = saved }()

	Config = validMarketWorker()
	Config.MarketData.Catalog = MarketDataCatalog{Priority: []string{"coingecko", "nomics", "coingecko"},
		Coverage: "most", Top: -1, IntervalHours: 1000, DetailsPerRun: 0, DetailsMaxAgeDays: 400, DeactivateAfterDays: 0}
	Config.MarketData.Sources["coinmarketcap"] = MarketDataSource{Enabled: true, TimeoutSeconds: 20}
	err := ValidateMarketWorker()
	require.Error(t, err)
	for _, want := range []string{`has "nomics"`, `has "coingecko" twice`, "market_data.catalog.coverage",
		"market_data.catalog.top", "market_data.catalog.interval_hours", "market_data.catalog.details_per_run",
		"market_data.catalog.details_max_age_days", "market_data.catalog.deactivate_after_days",
		"market_data.sources.coinmarketcap.api_key"} {
		assert.Contains(t, err.Error(), want)
	}

	Config = validMarketWorker()
	Config.MarketData.Sources["coinmarketcap"] = MarketDataSource{Enabled: true, APIKey: "key", TimeoutSeconds: 20, Egress: EgressDirect}
	assert.NoError(t, ValidateMarketWorker())
}

func TestValidateMarketWorker_ChecksTheTickers(t *testing.T) {
	saved := Config
	defer func() { Config = saved }()

	Config = validMarketWorker()
	Config.MarketData.Tickers = MarketDataTickers{IntervalSeconds: 10, StaleAfterSeconds: 5, OutlierPercent: 0,
		MinVenues: 1, MapRankLimit: 0, DeactivatePairsAfterHours: 0}
	err := ValidateMarketWorker()
	require.Error(t, err)
	for _, want := range []string{"market_data.tickers.interval_seconds", "market_data.tickers.outlier_percent",
		"market_data.tickers.min_venues", "market_data.tickers.map_rank_limit", "market_data.tickers.deactivate_pairs_after_hours",
		"every ticker turns stale"} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestValidateMarketWorker_ChecksTheCandles(t *testing.T) {
	saved := Config
	defer func() { Config = saved }()

	Config = validMarketWorker()
	Config.MarketData.Candles = MarketDataCandles{MaxPairsPerVenue: 0, RequestsPerRun: 0, Resolutions: map[string]MarketDataResolution{
		"2h": {Enabled: true, IntervalMinutes: 60, BackfillDays: 1},
		"1h": {Enabled: true, IntervalMinutes: 0, BackfillDays: 0, RetentionDays: -1},
	}}
	err := ValidateMarketWorker()
	require.Error(t, err)
	for _, want := range []string{"market_data.candles.quotes", "market_data.candles.max_pairs_per_venue",
		"market_data.candles.requests_per_run", "market_data.candles.resolutions.2h is not a resolution",
		"resolutions.1h.interval_minutes", "resolutions.1h.backfill_days", "resolutions.1h.retention_days"} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestSourceKeyVar_AddsTheKeyWithoutEnablingTheSource(t *testing.T) {
	var c Configuration
	v := sourceKeyVar("MARKET_DATA_COINGECKO_API_KEY", "coingecko")
	require.NoError(t, v.set(&c, "cg-key"))
	assert.Equal(t, "cg-key", c.MarketData.Sources["coingecko"].APIKey)
	assert.False(t, c.MarketData.Sources["coingecko"].Enabled)
}

func TestLocales_DefaultsAndEnvironment(t *testing.T) {
	c := secureRelease(t)
	assert.Equal(t, "en", c.I18n.DefaultLocale)
	assert.Equal(t, []string{"en", "fa"}, c.I18n.Locales)

	// A configuration built without defaults, as tests build them.
	r, err := (&Configuration{}).Locales()
	require.NoError(t, err)
	assert.Equal(t, []string{"en", "fa"}, r.Codes())

	require.NoError(t, overlayEnv(&c, func(name string) string {
		return map[string]string{"I18N_LOCALES": "en, ar", "I18N_DEFAULT_LOCALE": "ar"}[name]
	}))
	r, err = c.Locales()
	require.NoError(t, err)
	assert.Equal(t, []string{"en", "ar"}, r.Codes())
	assert.Equal(t, "ar", r.Default().Code)
	assert.Equal(t, "rtl", r.Default().Dir)
}

func TestValidateServer_GeoIPOverridesOutsideRelease(t *testing.T) {
	c := secureRelease(t)
	c.Server.Mode = "debug"
	c.GeoIP.Enabled = true
	c.GeoIP.Overrides = map[string]string{"192.168.0.0/16": "IR"}
	assert.Empty(t, problemsOf(c), "overrides alone are a source, outside release mode")
	c.GeoIP.Overrides = map[string]string{"192.168.0.0/16": "Iran"}
	assert.Contains(t, strings.Join(problemsOf(c), "\n"), "geoip.overrides is not valid")
	c.GeoIP.Overrides, c.GeoIP.Header, c.Server.TLS.BehindProxy, c.Server.TrustedProxies = nil, "CF-IPCountry", false, nil
	assert.Equal(t, []string{"geoip.header (GEOIP_HEADER) is set, but server.trusted_proxies (SERVER_TRUSTED_PROXIES) is empty; the header is only trusted from the proxies that set it"}, problemsOf(c))
	assert.Equal(t, DefaultGeoIPReloadIntervalSeconds, c.GeoIP.ReloadIntervalSeconds)
	assert.Equal(t, DefaultGeoIPMaxAgeDays, c.GeoIP.MaxAgeDays)
}

func TestValidateServer_Sites(t *testing.T) {
	c := secureRelease(t)
	c.Sites.Default = "global"
	c.Sites.List = []SiteConfig{
		{ID: "global", Domain: "example.com", Aliases: []string{"www.example.com"}, Locales: []string{"en"}},
		{ID: "ir", Domain: "example.ir", Countries: []string{"IR"}, Locales: []string{"fa", "en"}},
	}
	assert.Empty(t, problemsOf(c))
	assert.Equal(t, []string{"https://example.com", "https://www.example.com", "https://example.ir"}, c.SiteOrigins())

	r, err := c.SiteRegistry(mustLocales(t, c))
	require.NoError(t, err)
	assert.Equal(t, "ir", r.ByHost("example.ir").ID)

	c.Sites.List[1].Locales = []string{"ar"}
	assert.Equal(t, []string{`sites is not valid: site "ir": locale "ar" is not one of i18n.locales (en, fa)`}, problemsOf(c))
	c.Sites.List[1].Locales = []string{"fa"}

	c.Sites.Default = "world"
	assert.Contains(t, strings.Join(problemsOf(c), "\n"), `the default site "world" is not one of the sites (global, ir)`)
	c.Sites.Default = "global"

	c.Sites.Scheme = "http"
	assert.Equal(t, []string{`sites.scheme (SITES_SCHEME) is "http"; release mode serves the sites over https`}, problemsOf(c))
	c.Server.Mode = "debug"
	assert.Empty(t, problemsOf(c))
	assert.Equal(t, "http://example.ir", c.SiteOrigins()[2])
}

func mustLocales(t *testing.T, c Configuration) *i18n.Registry {
	t.Helper()
	r, err := c.Locales()
	require.NoError(t, err)
	return r
}
