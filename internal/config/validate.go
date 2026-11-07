package config

import (
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// problems collects what is wrong with a configuration, so that the server
// reports everything at once instead of one problem per start.
type problems []string

// add records a problem with the setting key, which it names together with
// its environment variable.
func (p *problems) add(key, format string, args ...interface{}) {
	*p = append(*p, setting(key)+" "+fmt.Sprintf(format, args...))
}

func (p problems) err() error {
	if len(p) == 0 {
		return nil
	}
	return errors.New(strings.Join(p, "\n"))
}

// setting names a setting with the environment variable that overrides it.
func setting(key string) string {
	if v := envVarFor(key); v != "" {
		return key + " (" + v + ")"
	}
	return key
}

// validateServer reports every setting that the server cannot start with,
// one problem per line, each naming the setting and its environment
// variable: required settings that are not set, values out of range,
// unusable TLS files, and, in release mode, insecure settings. Load has
// already applied the defaults.
func (c *Configuration) validateServer() error {
	var p problems
	c.requiredProblems(&p)
	c.rangeProblems(&p)
	c.tlsProblems(&p)
	release := c.Server.Mode == "release"
	clientTLSProblems(&p, "redis", c.Redis.Host, c.Redis.TLS, release)
	clientTLSProblems(&p, "rabbitmq", c.RabbitMQ.Host, c.RabbitMQ.TLS, release)
	if release {
		c.releaseProblems(&p)
	}
	return p.err()
}

func (c *Configuration) requiredProblems(p *problems) {
	var missing []string
	require := func(ok bool, key string) {
		if !ok {
			missing = append(missing, setting(key))
		}
	}
	require(c.Server.Port != "", "server.port")
	require(c.Postgres.Host != "", "postgres.host")
	require(c.Postgres.Port != "", "postgres.port")
	require(c.Postgres.User != "", "postgres.user")
	require(c.Postgres.DBName != "", "postgres.dbname")
	require(c.Redis.Host != "", "redis.host")
	require(c.Redis.Port != "", "redis.port")
	require(c.RabbitMQ.Host != "", "rabbitmq.host")
	require(c.RabbitMQ.Port != "", "rabbitmq.port")
	require(c.RabbitMQ.Username != "", "rabbitmq.username")
	require(c.RabbitMQ.Password != "", "rabbitmq.password")
	require(c.JWT.SSL.User.PrivateKey != "", "jwt.ssl.user.private_key")
	require(c.JWT.SSL.User.PublicKey != "", "jwt.ssl.user.public_key")
	require(c.JWT.AccessToken.Expiration > 0, "jwt.access_token.expiration_minutes")
	require(c.JWT.RefreshToken.Expiration > 0, "jwt.refresh_token.expiration_minutes")
	if len(missing) > 0 {
		*p = append(*p, "required settings are not set: "+strings.Join(missing, ", "))
	}
}

// maxAccessTokenMinutes bounds the lifetime of access tokens: a day. Tokens
// are rechecked against their session on every request, but a stolen token
// should still expire soon.
const maxAccessTokenMinutes = 24 * 60

func (c *Configuration) rangeProblems(p *problems) {
	for _, port := range []struct{ key, value string }{
		{"server.port", c.Server.Port},
		{"postgres.port", c.Postgres.Port},
		{"redis.port", c.Redis.Port},
		{"rabbitmq.port", c.RabbitMQ.Port},
	} {
		if port.value == "" {
			continue // reported as missing
		}
		if n, err := strconv.Atoi(port.value); err != nil || n < 1 || n > 65535 {
			p.add(port.key, "is %q; it must be a port number from 1 to 65535", port.value)
		}
	}

	atLeast := func(key string, value, minimum int) {
		if value < minimum {
			p.add(key, "is %d; it must be at least %d", value, minimum)
		}
	}
	atLeast("server.read_timeout_seconds", c.Server.ReadTimeout, 1)
	atLeast("server.write_timeout_seconds", c.Server.WriteTimeout, 1)
	atLeast("server.idle_timeout_seconds", c.Server.IdleTimeout, 1)
	atLeast("server.shutdown_timeout_seconds", c.Server.ShutdownTimeout, 1)
	atLeast("server.drain_seconds", c.Server.DrainSeconds, 0)
	atLeast("server.hsts.max_age_seconds", c.Server.HSTS.MaxAgeSeconds, 1)
	if c.Server.DrainSeconds >= c.Server.ShutdownTimeout && c.Server.ShutdownTimeout >= 1 {
		p.add("server.drain_seconds", "is %d; it must be less than server.shutdown_timeout_seconds (%d), which includes it", c.Server.DrainSeconds, c.Server.ShutdownTimeout)
	}
	if c.Server.MaxBodyBytes < 1 {
		p.add("server.max_body_bytes", "is %d; it must be at least 1", c.Server.MaxBodyBytes)
	}
	atLeast("postgres.max_open_conns", c.Postgres.MaxOpenConns, 1)
	atLeast("postgres.max_idle_conns", c.Postgres.MaxIdleConns, 0)
	if c.Postgres.MaxIdleConns > c.Postgres.MaxOpenConns && c.Postgres.MaxOpenConns >= 1 {
		p.add("postgres.max_idle_conns", "is %d; it must not exceed postgres.max_open_conns (%d)", c.Postgres.MaxIdleConns, c.Postgres.MaxOpenConns)
	}
	atLeast("postgres.conn_max_lifetime_minutes", c.Postgres.ConnMaxLifetime, 0)
	atLeast("postgres.conn_max_idle_time_minutes", c.Postgres.ConnMaxIdleTime, 0)
	atLeast("redis.db", c.Redis.DB, 0)
	atLeast("redis.pool_size", c.Redis.PoolSize, 1)
	atLeast("redis.min_idle_conns", c.Redis.MinIdleConns, 0)
	if c.Redis.MinIdleConns > c.Redis.PoolSize && c.Redis.PoolSize >= 1 {
		p.add("redis.min_idle_conns", "is %d; it must not exceed redis.pool_size (%d)", c.Redis.MinIdleConns, c.Redis.PoolSize)
	}
	atLeast("redis.currency_cache_ttl_seconds", c.Redis.CurrencyCacheTTL, 1)

	access, refresh := c.JWT.AccessToken.Expiration, c.JWT.RefreshToken.Expiration
	if access > maxAccessTokenMinutes {
		p.add("jwt.access_token.expiration_minutes", "is %d; it must be at most %d (a day)", access, maxAccessTokenMinutes)
	}
	if access > 0 && refresh > 0 && refresh < access {
		p.add("jwt.refresh_token.expiration_minutes", "is %d; it must be at least jwt.access_token.expiration_minutes (%d), or the session ends before its access token", refresh, access)
	}

	ws := c.WebSocket
	atLeast("websocket.ping_interval_seconds", ws.PingInterval, 1)
	atLeast("websocket.max_message_size_bytes", ws.MaxMessageSize, 1)
	if ws.MaxMessageSize > MaxWSMaxMessageSize {
		p.add("websocket.max_message_size_bytes", "is %d; it must be at most %d (1 MiB)", ws.MaxMessageSize, MaxWSMaxMessageSize)
	}
	atLeast("websocket.write_timeout_seconds", ws.WriteTimeout, 1)
	atLeast("websocket.client_messages_per_second", ws.ClientMessagesPerSecond, 1)
	atLeast("websocket.client_message_burst", ws.ClientMessageBurst, 1)
	atLeast("websocket.max_connections", ws.MaxConnections, 1)
	atLeast("websocket.max_connections_per_ip", ws.MaxConnectionsPerIP, 1)
	atLeast("websocket.session_check_seconds", ws.SessionCheckSeconds, 1)
	if v := c.JWT.RefreshToken.ReuseGraceSeconds; v < 1 || v > MaxRefreshReuseGraceSeconds {
		p.add("jwt.refresh_token.reuse_grace_seconds", "is %d; it must be from 1 to %d", v, MaxRefreshReuseGraceSeconds)
	}
	if v := c.Favorites.MaxPerUser; v < 1 || v > MaxFavoritesPerUser {
		p.add("favorites.max_per_user", "is %d; it must be from 1 to %d", v, MaxFavoritesPerUser)
	}
	if v := c.JWT.Sessions.MaxPerUser; v < 1 || v > MaxSessionsPerUser {
		p.add("jwt.sessions.max_per_user", "is %d; it must be from 1 to %d", v, MaxSessionsPerUser)
	}
	if v := c.Admin.SignInMinResponseMS; v < 1 || v > MaxAdminSignInMinResponseMS {
		p.add("admin.sign_in_min_response_ms", "is %d; it must be from 1 to %d", v, MaxAdminSignInMinResponseMS)
	}
	if ws.MaxConnectionsPerIP > ws.MaxConnections && ws.MaxConnections >= 1 {
		p.add("websocket.max_connections_per_ip", "is %d; it must not exceed websocket.max_connections (%d)", ws.MaxConnectionsPerIP, ws.MaxConnections)
	}
	atLeast("notifications.worker.concurrency", c.Notifications.Worker.Concurrency, 1)
	atLeast("notifications.worker.max_attempts", c.Notifications.Worker.MaxAttempts, 1)
	limits := c.RateLimits
	atLeast("rate_limits.rest.requests_per_second", limits.REST.RequestsPerSecond, 1)
	atLeast("rate_limits.rest.burst", limits.REST.Burst, 1)
	atLeast("rate_limits.ws.messages_per_second", limits.WS.MessagesPerSecond, 1)
	atLeast("rate_limits.ws.burst", limits.WS.Burst, 1)
	atLeast("rate_limits.trading.orders_per_second", limits.Trading.OrdersPerSecond, 1)
	atLeast("rate_limits.trading.burst", limits.Trading.Burst, 1)
	atLeast("rate_limits.auth.login_attempts_per_minute", limits.Auth.LoginAttemptsPerMinute, 1)
	atLeast("rate_limits.auth.otp_requests_per_hour", limits.Auth.OTPRequestsPerHour, 1)
	if n := limits.IPv6PrefixLength; n < MinIPv6PrefixLength || n > 128 {
		p.add("rate_limits.ipv6_prefix_length", "is %d; it must be between %d and 128", n, MinIPv6PrefixLength)
	}
}

// MinIPv6PrefixLength is the shortest prefix that IPv6 clients may be
// grouped by. A shorter one would let a single client's limits cover a
// whole provider.
const MinIPv6PrefixLength = 32

// tlsProblems checks the certificate and key when TLS is enabled, so that a
// broken pair stops the server at startup instead of when it starts to
// listen.
func (c *Configuration) tlsProblems(p *problems) {
	t := c.Server.TLS
	if !t.Enabled {
		return
	}
	if t.CertFile == "" || t.KeyFile == "" {
		p.add("server.tls.enabled", "is true, but server.tls.cert_file (%s) and server.tls.key_file (%s) must both be set",
			envVarFor("server.tls.cert_file"), envVarFor("server.tls.key_file"))
		return
	}
	if _, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile); err != nil {
		p.add("server.tls.cert_file", "and server.tls.key_file do not hold a usable certificate and key: %v", err)
	}
}

// postgresTLSModes are the sslmode values that always encrypt the
// connection. "prefer" and "allow" fall back to plain text.
var postgresTLSModes = []string{"require", "verify-ca", "verify-full"}

// releaseProblems refuses settings that are acceptable for development but
// not in production.
func (c *Configuration) releaseProblems(p *problems) {
	t := c.Server.TLS
	if !t.Enabled && !t.BehindProxy {
		p.add("server.tls.enabled", "is false; release mode needs TLS: enable it, or set server.tls.behind_proxy (%s) when a reverse proxy terminates TLS in front of the server",
			envVarFor("server.tls.behind_proxy"))
	}
	if t.BehindProxy && len(c.Server.TrustedProxies) == 0 {
		p.add("server.trusted_proxies", "is empty although server.tls.behind_proxy is set; list the proxy's addresses, or every client looks like the proxy, also to the rate limits")
	}

	if !contains(postgresTLSModes, c.Postgres.SSLMode) {
		p.add("postgres.sslmode", "is %q; release mode needs one of %s, so that the connection to PostgreSQL is encrypted",
			c.Postgres.SSLMode, strings.Join(postgresTLSModes, ", "))
	}
	if c.Postgres.Password == "" || c.Postgres.Password == "postgres" {
		p.add("postgres.password", "is empty or the default \"postgres\"; release mode needs a password of its own")
	}
	if c.Redis.Password == "" {
		p.add("redis.password", "is empty; release mode needs Redis to require a password")
	}
	if c.RabbitMQ.Username == "guest" || c.RabbitMQ.Password == "guest" {
		p.add("rabbitmq.username", "and rabbitmq.password (%s) use the default guest account; release mode needs a user of its own",
			envVarFor("rabbitmq.password"))
	}

	for _, origins := range []struct {
		key  string
		list []string
	}{
		{"server.cors.allowed_origins", c.Server.CORS.AllowedOrigins},
		{"websocket.allowed_origins", c.WebSocket.AllowedOrigins},
	} {
		for _, origin := range origins.list {
			switch {
			case strings.TrimSpace(origin) == "*":
				p.add(origins.key, "contains \"*\"; list the exact origins that may connect")
			case !strings.HasPrefix(strings.ToLower(strings.TrimSpace(origin)), "https://"):
				p.add(origins.key, "contains %q; release mode allows only https origins", origin)
			}
		}
	}

	for _, key := range []struct {
		setting string
		path    string
	}{
		{"jwt.ssl.user.private_key", string(c.JWT.SSL.User.PrivateKey)},
		{"jwt.ssl.admin.private_key", string(c.JWT.SSL.Admin.PrivateKey)},
		{"server.tls.key_file", t.KeyFile},
	} {
		if key.path == "" {
			continue
		}
		if writable, mode := writableByOthers(key.path); writable {
			p.add(key.setting, "is %s with mode %v, writable by other users; allow writing only to its owner (chmod go-w)", key.path, mode)
		}
	}
}

// writableByOthers reports a file that users other than its owner may
// change. Windows has no such mode bits, so it is not checked there. Read
// access is not checked: container platforms mount secrets readable by
// every user of the container.
func writableByOthers(path string) (bool, os.FileMode) {
	if runtime.GOOS == "windows" {
		return false, 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, 0 // loading the key reports a missing file
	}
	return info.Mode().Perm()&0o022 != 0, info.Mode().Perm()
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
