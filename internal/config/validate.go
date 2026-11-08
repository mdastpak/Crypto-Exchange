package config

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
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
	c.redisModeProblems(&p)
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
	// postgres.hosts replaces host and port.
	require(c.Postgres.Host != "" || len(c.Postgres.Hosts) > 0, "postgres.host")
	require(c.Postgres.Port != "" || len(c.Postgres.Hosts) > 0, "postgres.port")
	require(c.Postgres.User != "", "postgres.user")
	require(c.Postgres.DBName != "", "postgres.dbname")
	// Only the standalone mode connects to redis.host and redis.port; the
	// other modes check their own lists in redisModeProblems.
	standalone := c.Redis.Mode == "" || c.Redis.Mode == RedisModeStandalone
	require(c.Redis.Host != "" || !standalone, "redis.host")
	require(c.Redis.Port != "" || !standalone, "redis.port")
	// rabbitmq.hosts replaces host and port.
	require(c.RabbitMQ.Host != "" || len(c.RabbitMQ.Hosts) > 0, "rabbitmq.host")
	require(c.RabbitMQ.Port != "" || len(c.RabbitMQ.Hosts) > 0, "rabbitmq.port")
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
	for i, hostPort := range c.Postgres.Hosts {
		if !validHostPort(hostPort) {
			p.add("postgres.hosts", "entry %d is %q; each entry must be host:port, such as pg-1.internal:5432 or [2001:db8::1]:5432", i+1, hostPort)
		}
	}
	for i, hostPort := range c.RabbitMQ.Hosts {
		if !validHostPort(hostPort) {
			p.add("rabbitmq.hosts", "entry %d is %q; each entry must be host:port, such as rmq-1.internal:5671", i+1, hostPort)
		}
	}
	if len(c.RabbitMQ.Hosts) > 0 && c.RabbitMQ.Host != "" {
		p.add("rabbitmq.host", "is set together with rabbitmq.hosts; set one of them")
	}
	if len(c.Postgres.Hosts) > 0 && c.Postgres.Host != "" {
		p.add("postgres.host", "is set together with postgres.hosts; set one of them")
	}
	if !contains(PostgresTargetSessionAttrs, c.Postgres.TargetSessionAttrs) {
		p.add("postgres.target_session_attrs", "is %q; it must be one of %s", c.Postgres.TargetSessionAttrs, strings.Join(PostgresTargetSessionAttrs, ", "))
	}
	for _, timeout := range []struct {
		key   string
		value int
	}{
		{"postgres.statement_timeout_ms", c.Postgres.StatementTimeoutMs},
		{"postgres.lock_timeout_ms", c.Postgres.LockTimeoutMs},
		{"postgres.idle_in_transaction_timeout_ms", c.Postgres.IdleInTransactionTimeoutMs},
	} {
		if timeout.value < -1 {
			p.add(timeout.key, "is %d; it must be a number of milliseconds, or -1 to turn it off", timeout.value)
		}
	}
	atLeast("postgres.migration_lock_timeout_seconds", c.Postgres.MigrationLockTimeoutSeconds, 1)
	if !contains(MigrationsModes, c.Postgres.Migrations.Mode) {
		p.add("postgres.migrations.mode", "is %q; it must be one of %s", c.Postgres.Migrations.Mode, strings.Join(MigrationsModes, ", "))
	}
	if c.Postgres.Migrations.Password != "" && c.Postgres.Migrations.User == "" {
		p.add("postgres.migrations.password", "is set without postgres.migrations.user; set the user of the migration role too")
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
	if lifetime := c.JWT.Sessions.MaxLifetimeMinutes; lifetime < 1 {
		p.add("jwt.sessions.max_lifetime_minutes", "is %d; it must be at least 1", lifetime)
	} else if refresh > 0 && lifetime < refresh {
		p.add("jwt.sessions.max_lifetime_minutes", "is %d; it must be at least jwt.refresh_token.expiration_minutes (%d), the lifetime of one refresh token", lifetime, refresh)
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
	atLeast("websocket.max_connections_per_user", ws.MaxConnectionsPerUser, 1)
	atLeast("websocket.session_check_seconds", ws.SessionCheckSeconds, 1)
	if v := c.JWT.RefreshToken.ReuseGraceSeconds; v < 1 || v > MaxRefreshReuseGraceSeconds {
		p.add("jwt.refresh_token.reuse_grace_seconds", "is %d; it must be from 1 to %d", v, MaxRefreshReuseGraceSeconds)
	}
	if v := c.Contacts.NewContactCooldownHours; v < 1 || v > MaxNewContactCooldownHours {
		p.add("contacts.new_contact_cooldown_hours", "is %d; it must be from 1 to %d", v, MaxNewContactCooldownHours)
	}
	if v := c.Security.ChangeHoldHours; v < 1 || v > MaxSecurityChangeHoldHours {
		p.add("security.change_hold_hours", "is %d; it must be from 1 to %d", v, MaxSecurityChangeHoldHours)
	}
	mobileOwnershipProblems(p, c.Identity.MobileOwnership)
	if v := c.Favorites.MaxPerUser; v < 1 || v > MaxFavoritesPerUser {
		p.add("favorites.max_per_user", "is %d; it must be from 1 to %d", v, MaxFavoritesPerUser)
	}
	c.marketDataProblems(p)
	if v := c.Delisting.IntervalSeconds; v < 1 || v > MaxDelistingIntervalSeconds {
		p.add("delisting.interval_seconds", "is %d; it must be from 1 to %d", v, MaxDelistingIntervalSeconds)
	}
	if v := c.Platform.PollIntervalMs; v < MinPlatformPollIntervalMs || v > MaxPlatformPollIntervalMs {
		p.add("platform.poll_interval_ms", "is %d; it must be from %d to %d", v, MinPlatformPollIntervalMs, MaxPlatformPollIntervalMs)
	}
	if v := c.Platform.StaleAfterSeconds; v < 1 || v > MaxPlatformStaleAfterSeconds {
		p.add("platform.stale_after_seconds", "is %d; it must be from 1 to %d", v, MaxPlatformStaleAfterSeconds)
	} else if v*1000 <= c.Platform.PollIntervalMs {
		p.add("platform.stale_after_seconds", "is %d; it must be longer than platform.poll_interval_ms (%d ms), or every instance closes the fail-closed features between two reads", v, c.Platform.PollIntervalMs)
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
	if c.Identity.MobileOwnership.Provider == MobileOwnershipStub {
		p.add("identity.mobile_ownership.provider", "is \"stub\", which matches every mobile number; release mode needs a real provider, or none")
	}
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
	// The server must not be able to change the schema (#180): the
	// migrations run as a release step with a role of their own, and the
	// server checks at startup that its own role owns nothing.
	if c.Postgres.Migrations.Mode == MigrationsModeStartup {
		p.add("postgres.migrations.mode", "is %q; release mode needs %q: run \"admin migrate\" with the migration role as a step of the release, before the new instances start (docs/configuration.md#database-roles)",
			c.Postgres.Migrations.Mode, MigrationsModeExternal)
	}
	if c.Postgres.Migrations.Password != "" {
		p.add("postgres.migrations.password", "is set; in release mode only the release step that runs \"admin migrate\" may hold the migration role's password, never a server instance")
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

// redisModeProblems checks redis.mode and the settings of the mode (#231).
func (c *Configuration) redisModeProblems(p *problems) {
	r := c.Redis
	addresses := func(key string, list []string) {
		if len(list) == 0 {
			p.add(key, "is empty; redis.mode %s needs at least one host:port", r.Mode)
		}
		for i, hostPort := range list {
			if !validHostPort(hostPort) {
				p.add(key, "entry %d is %q; each entry must be host:port, such as redis-1.internal:26379", i+1, hostPort)
			}
		}
	}
	switch r.Mode {
	case "", RedisModeStandalone:
		if len(r.Sentinel.Addresses) > 0 || len(r.Cluster.Addresses) > 0 {
			p.add("redis.mode", "is %s, but redis.sentinel.addresses or redis.cluster.addresses is set; set redis.mode to the mode they are for", RedisModeStandalone)
		}
	case RedisModeSentinel:
		if r.Sentinel.MasterName == "" {
			p.add("redis.sentinel.master_name", "is empty; redis.mode sentinel needs the name of the primary that the sentinels monitor")
		}
		addresses("redis.sentinel.addresses", r.Sentinel.Addresses)
	case RedisModeCluster:
		addresses("redis.cluster.addresses", r.Cluster.Addresses)
		if r.DB != 0 {
			p.add("redis.db", "is %d; Redis Cluster has only database 0", r.DB)
		}
	default:
		p.add("redis.mode", "is %q; it must be one of %s", r.Mode, strings.Join(RedisModes, ", "))
	}
	if r.Mode != RedisModeSentinel && (r.Sentinel.MasterName != "" || r.Sentinel.Username != "" || r.Sentinel.Password != "") {
		p.add("redis.sentinel.master_name", "or the sentinel credentials are set, but redis.mode is %q, not sentinel", r.Mode)
	}
	if r.DurableWrites.Replicas < 0 {
		p.add("redis.durable_writes.replicas", "is %d; it must be 0, which does not wait, or more", r.DurableWrites.Replicas)
	}
	if r.DurableWrites.TimeoutMs < 1 {
		p.add("redis.durable_writes.timeout_ms", "is %d; it must be at least 1", r.DurableWrites.TimeoutMs)
	}
}

// ValidateMarketWorker checks the settings that cmd/market-worker uses
// (#263), and reports every problem at once. The worker needs PostgreSQL
// and its own section, not the settings of the server.
func ValidateMarketWorker() error {
	var p problems
	c := &Config
	var missing []string
	for _, r := range []struct {
		ok  bool
		key string
	}{
		{c.Postgres.Host != "" || len(c.Postgres.Hosts) > 0, "postgres.host"},
		{c.Postgres.Port != "" || len(c.Postgres.Hosts) > 0, "postgres.port"},
		{c.Postgres.User != "", "postgres.user"},
		{c.Postgres.DBName != "", "postgres.dbname"},
	} {
		if !r.ok {
			missing = append(missing, setting(r.key))
		}
	}
	if len(missing) > 0 {
		p = append(p, "required settings are not set: "+strings.Join(missing, ", "))
	}
	c.marketDataProblems(&p)
	return p.err()
}

// marketDataProblems checks the market_data section.
func (c *Configuration) marketDataProblems(p *problems) {
	if m := c.MarketData.Mode; m != MarketDataModeDryRun && m != MarketDataModeLive {
		p.add("market_data.mode", "is %q; it must be %q or %q", m, MarketDataModeDryRun, MarketDataModeLive)
	}
	if !validListenAddr(c.MarketData.HealthAddr) {
		p.add("market_data.health_addr", "is %q; it must be a port with an optional host, such as :8081", c.MarketData.HealthAddr)
	}
	m := c.MarketData
	for _, r := range []struct {
		key           string
		value, lo, hi int
	}{
		{"market_data.poll_seconds", m.PollSeconds, 1, MaxMarketDataPollSeconds},
		{"market_data.lease_seconds", m.LeaseSeconds, MinMarketDataLeaseSeconds, MaxMarketDataLeaseSeconds},
		{"market_data.max_concurrent_jobs", m.MaxConcurrentJobs, 1, MaxMarketDataMaxConcurrentJobs},
		{"market_data.retry_base_seconds", m.RetryBaseSeconds, 1, MaxMarketDataRetryBaseSeconds},
	} {
		if r.value < r.lo || r.value > r.hi {
			p.add(r.key, "is %d; it must be from %d to %d", r.value, r.lo, r.hi)
		}
	}
}

// validListenAddr reports whether addr is [host]:port with a port from 1 to
// 65535; an empty host listens on every interface.
func validListenAddr(addr string) bool {
	_, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	n, portErr := strconv.Atoi(port)
	return err == nil && portErr == nil && n >= 1 && n <= 65535
}

// validHostPort reports whether hostPort is host:port with a port from 1
// to 65535.
func validHostPort(hostPort string) bool {
	host, port, err := net.SplitHostPort(strings.TrimSpace(hostPort))
	n, portErr := strconv.Atoi(port)
	return err == nil && host != "" && portErr == nil && n >= 1 && n <= 65535
}

// mobileOwnershipProblems checks identity.mobile_ownership (#253) in every
// mode.
func mobileOwnershipProblems(p *problems, m MobileOwnershipConfig) {
	switch m.Provider {
	case MobileOwnershipNone, MobileOwnershipStub:
	case MobileOwnershipJibit:
		if u, err := url.Parse(m.Endpoint); err != nil || u.Scheme != "https" || u.Host == "" {
			p.add("identity.mobile_ownership.endpoint", "is %q; it must be an https URL", m.Endpoint)
		}
		if m.APIKey == "" || m.SecretKey == "" {
			p.add("identity.mobile_ownership.api_key", "and identity.mobile_ownership.secret_key (IDENTITY_MOBILE_OWNERSHIP_SECRET_KEY) must both be set for the jibit provider")
		}
	default:
		p.add("identity.mobile_ownership.provider", "is %q; it must be one of none, stub, jibit", m.Provider)
	}
	if v := m.TimeoutSeconds; v < 1 || v > 60 {
		p.add("identity.mobile_ownership.timeout_seconds", "is %d; it must be from 1 to 60", v)
	}
	if v := m.RecheckDays; v < 1 || v > 365 {
		p.add("identity.mobile_ownership.recheck_days", "is %d; it must be from 1 to 365", v)
	}
}
