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
	c.Redis.Host, c.Redis.Port, c.Redis.Password = "redis", "6379", "a-redis-password"
	c.RabbitMQ.Host, c.RabbitMQ.Port, c.RabbitMQ.Username, c.RabbitMQ.Password = "mq", "5672", "exchange", "an-mq-password"
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
		"no Redis password":            {func(c *Configuration) { c.Redis.Password = "" }, "redis.password (REDIS_PASSWORD) is empty"},
		"RabbitMQ guest user":          {func(c *Configuration) { c.RabbitMQ.Username = "guest" }, "rabbitmq.username (RABBITMQ_USER) and rabbitmq.password (RABBITMQ_PASSWORD) use the default guest account"},
		"RabbitMQ guest password":      {func(c *Configuration) { c.RabbitMQ.Password = "guest" }, "use the default guest account"},
		"CORS wildcard":                {func(c *Configuration) { c.Server.CORS.AllowedOrigins = []string{"*"} }, `server.cors.allowed_origins (SERVER_CORS_ALLOWED_ORIGINS) contains "*"`},
		"CORS over http":               {func(c *Configuration) { c.Server.CORS.AllowedOrigins = []string{"http://app.example.com"} }, `contains "http://app.example.com"; release mode allows only https origins`},
		"WebSocket wildcard":           {func(c *Configuration) { c.WebSocket.AllowedOrigins = []string{" * "} }, `websocket.allowed_origins (WS_ALLOWED_ORIGINS) contains "*"`},
		"WebSocket over http":          {func(c *Configuration) { c.WebSocket.AllowedOrigins = []string{"http://localhost:3000"} }, `websocket.allowed_origins (WS_ALLOWED_ORIGINS) contains "http://localhost:3000"`},

		// Ranges, in every mode.
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
		"refresh shorter than access":      {func(c *Configuration) { c.JWT.RefreshToken.Expiration = 10 }, "jwt.refresh_token.expiration_minutes (JWT_REFRESH_TOKEN_EXPIRATION_MINUTES) is 10; it must be at least jwt.access_token.expiration_minutes (15)"},
		"negative ping interval":           {func(c *Configuration) { c.WebSocket.PingInterval = -1 }, "websocket.ping_interval_seconds (WS_PING_INTERVAL_SECONDS) is -1"},
		"WebSocket message over 1 MiB":     {func(c *Configuration) { c.WebSocket.MaxMessageSize = 2 << 20 }, "websocket.max_message_size_bytes (WS_MAX_MESSAGE_SIZE_BYTES) is 2097152; it must be at most 1048576"},
		"negative WebSocket write timeout": {func(c *Configuration) { c.WebSocket.WriteTimeout = -1 }, "websocket.write_timeout_seconds (WS_WRITE_TIMEOUT_SECONDS) is -1"},
		"negative client message rate":     {func(c *Configuration) { c.WebSocket.ClientMessagesPerSecond = -1 }, "websocket.client_messages_per_second (WS_CLIENT_MESSAGES_PER_SECOND) is -1"},
		"more per address than in total":   {func(c *Configuration) { c.WebSocket.MaxConnectionsPerIP = 20000 }, "websocket.max_connections_per_ip (WS_MAX_CONNECTIONS_PER_IP) is 20000; it must not exceed websocket.max_connections (10000)"},
		"negative worker count":            {func(c *Configuration) { c.Notifications.Worker.Concurrency = -1 }, "notifications.worker.concurrency is -1"},
		"negative rate limit":              {func(c *Configuration) { c.RateLimits.Auth.OTPRequestsPerHour = -1 }, "rate_limits.auth.otp_requests_per_hour (RATE_LIMITS_AUTH_OTP_REQUESTS_PER_HOUR) is -1"},

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
	assert.Empty(t, problemsOf(c), "development settings are fine outside release mode")

	c.Server.Mode = "release"
	assert.Len(t, problemsOf(c), 7, "release mode lists every problem at once")
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
