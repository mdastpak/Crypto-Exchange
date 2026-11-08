package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// EnvVar overrides one setting from the environment. A variable that is
// unset or empty leaves the setting as the configuration file has it.
type EnvVar struct {
	Name string
	// Key is the setting it overrides, as written in the configuration file.
	Key string
	set func(c *Configuration, value string) error
}

func stringVar(name, key string, field func(*Configuration) *string) EnvVar {
	return EnvVar{name, key, func(c *Configuration, value string) error {
		*field(c) = value
		return nil
	}}
}

func pathVar(name, key string, field func(*Configuration) *KeyPath) EnvVar {
	return EnvVar{name, key, func(c *Configuration, value string) error {
		*field(c) = KeyPath(value)
		return nil
	}}
}

func intVar(name, key string, field func(*Configuration) *int) EnvVar {
	return EnvVar{name, key, func(c *Configuration, value string) error {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("%q is not an integer", value)
		}
		*field(c) = n
		return nil
	}}
}

func boolVar(name, key string, field func(*Configuration) *bool) EnvVar {
	return EnvVar{name, key, func(c *Configuration, value string) error {
		b, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("%q is not true or false", value)
		}
		*field(c) = b
		return nil
	}}
}

// optionalBoolVar sets a setting whose absence has a meaning of its own.
func optionalBoolVar(name, key string, field func(*Configuration) **bool) EnvVar {
	return EnvVar{name, key, func(c *Configuration, value string) error {
		b, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("%q is not true or false", value)
		}
		*field(c) = &b
		return nil
	}}
}

func int64Var(name, key string, field func(*Configuration) *int64) EnvVar {
	return EnvVar{name, key, func(c *Configuration, value string) error {
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return fmt.Errorf("%q is not an integer", value)
		}
		*field(c) = n
		return nil
	}}
}

// listVar reads a comma-separated list. Spaces around the items and empty
// items are dropped.
func listVar(name, key string, field func(*Configuration) *[]string) EnvVar {
	return EnvVar{name, key, func(c *Configuration, value string) error {
		var items []string
		for _, item := range strings.Split(value, ",") {
			if item = strings.TrimSpace(item); item != "" {
				items = append(items, item)
			}
		}
		*field(c) = items
		return nil
	}}
}

// providerVar sets a credential of the providers that match.
func providerVar(name, key string, set func(c *Configuration, value string)) EnvVar {
	return EnvVar{name, key, func(c *Configuration, value string) error {
		set(c, value)
		return nil
	}}
}

// envVars lists every environment variable that the configuration reads, in
// the order they are applied. docs/configuration.md documents each one, and
// the tests check that every variable in .env.example is listed here.
var envVars = []EnvVar{
	// GIN_MODE is an alias of SERVER_MODE, which comes later and wins.
	stringVar("GIN_MODE", "server.mode", func(c *Configuration) *string { return &c.Server.Mode }),
	stringVar("SERVER_MODE", "server.mode", func(c *Configuration) *string { return &c.Server.Mode }),
	stringVar("SERVER_IP", "server.ip", func(c *Configuration) *string { return &c.Server.IP }),
	stringVar("SERVER_PORT", "server.port", func(c *Configuration) *string { return &c.Server.Port }),
	intVar("SERVER_READ_TIMEOUT_SECONDS", "server.read_timeout_seconds", func(c *Configuration) *int { return &c.Server.ReadTimeout }),
	intVar("SERVER_WRITE_TIMEOUT_SECONDS", "server.write_timeout_seconds", func(c *Configuration) *int { return &c.Server.WriteTimeout }),
	intVar("SERVER_IDLE_TIMEOUT_SECONDS", "server.idle_timeout_seconds", func(c *Configuration) *int { return &c.Server.IdleTimeout }),
	listVar("SERVER_TRUSTED_PROXIES", "server.trusted_proxies", func(c *Configuration) *[]string { return &c.Server.TrustedProxies }),
	listVar("SERVER_CORS_ALLOWED_ORIGINS", "server.cors.allowed_origins", func(c *Configuration) *[]string { return &c.Server.CORS.AllowedOrigins }),
	boolVar("SERVER_TLS_ENABLED", "server.tls.enabled", func(c *Configuration) *bool { return &c.Server.TLS.Enabled }),
	stringVar("SERVER_TLS_CERT_FILE", "server.tls.cert_file", func(c *Configuration) *string { return &c.Server.TLS.CertFile }),
	stringVar("SERVER_TLS_KEY_FILE", "server.tls.key_file", func(c *Configuration) *string { return &c.Server.TLS.KeyFile }),
	boolVar("SERVER_TLS_BEHIND_PROXY", "server.tls.behind_proxy", func(c *Configuration) *bool { return &c.Server.TLS.BehindProxy }),
	optionalBoolVar("SERVER_HSTS_ENABLED", "server.hsts.enabled", func(c *Configuration) **bool { return &c.Server.HSTS.Enabled }),
	intVar("SERVER_HSTS_MAX_AGE_SECONDS", "server.hsts.max_age_seconds", func(c *Configuration) *int { return &c.Server.HSTS.MaxAgeSeconds }),
	boolVar("SERVER_HSTS_INCLUDE_SUBDOMAINS", "server.hsts.include_subdomains", func(c *Configuration) *bool { return &c.Server.HSTS.IncludeSubdomains }),
	optionalBoolVar("SERVER_SWAGGER_ENABLED", "server.swagger.enabled", func(c *Configuration) **bool { return &c.Server.Swagger.Enabled }),
	int64Var("SERVER_MAX_BODY_BYTES", "server.max_body_bytes", func(c *Configuration) *int64 { return &c.Server.MaxBodyBytes }),
	intVar("SERVER_SHUTDOWN_TIMEOUT_SECONDS", "server.shutdown_timeout_seconds", func(c *Configuration) *int { return &c.Server.ShutdownTimeout }),
	intVar("SERVER_DRAIN_SECONDS", "server.drain_seconds", func(c *Configuration) *int { return &c.Server.DrainSeconds }),

	stringVar("POSTGRES_HOST", "postgres.host", func(c *Configuration) *string { return &c.Postgres.Host }),
	stringVar("POSTGRES_PORT", "postgres.port", func(c *Configuration) *string { return &c.Postgres.Port }),
	stringVar("POSTGRES_USER", "postgres.user", func(c *Configuration) *string { return &c.Postgres.User }),
	stringVar("POSTGRES_PASSWORD", "postgres.password", func(c *Configuration) *string { return &c.Postgres.Password }),
	stringVar("POSTGRES_DB", "postgres.dbname", func(c *Configuration) *string { return &c.Postgres.DBName }),
	stringVar("POSTGRES_SSLMODE", "postgres.sslmode", func(c *Configuration) *string { return &c.Postgres.SSLMode }),
	listVar("POSTGRES_HOSTS", "postgres.hosts", func(c *Configuration) *[]string { return &c.Postgres.Hosts }),
	stringVar("POSTGRES_TARGET_SESSION_ATTRS", "postgres.target_session_attrs", func(c *Configuration) *string { return &c.Postgres.TargetSessionAttrs }),
	intVar("POSTGRES_STATEMENT_TIMEOUT_MS", "postgres.statement_timeout_ms", func(c *Configuration) *int { return &c.Postgres.StatementTimeoutMs }),
	intVar("POSTGRES_LOCK_TIMEOUT_MS", "postgres.lock_timeout_ms", func(c *Configuration) *int { return &c.Postgres.LockTimeoutMs }),
	intVar("POSTGRES_IDLE_IN_TRANSACTION_TIMEOUT_MS", "postgres.idle_in_transaction_timeout_ms", func(c *Configuration) *int { return &c.Postgres.IdleInTransactionTimeoutMs }),
	intVar("POSTGRES_MIGRATION_LOCK_TIMEOUT_SECONDS", "postgres.migration_lock_timeout_seconds", func(c *Configuration) *int { return &c.Postgres.MigrationLockTimeoutSeconds }),
	stringVar("POSTGRES_MIGRATIONS_MODE", "postgres.migrations.mode", func(c *Configuration) *string { return &c.Postgres.Migrations.Mode }),
	stringVar("POSTGRES_MIGRATIONS_USER", "postgres.migrations.user", func(c *Configuration) *string { return &c.Postgres.Migrations.User }),
	stringVar("POSTGRES_MIGRATIONS_PASSWORD", "postgres.migrations.password", func(c *Configuration) *string { return &c.Postgres.Migrations.Password }),
	intVar("POSTGRES_MAX_OPEN_CONNS", "postgres.max_open_conns", func(c *Configuration) *int { return &c.Postgres.MaxOpenConns }),
	intVar("POSTGRES_MAX_IDLE_CONNS", "postgres.max_idle_conns", func(c *Configuration) *int { return &c.Postgres.MaxIdleConns }),
	intVar("POSTGRES_CONN_MAX_LIFETIME_MINUTES", "postgres.conn_max_lifetime_minutes", func(c *Configuration) *int { return &c.Postgres.ConnMaxLifetime }),
	intVar("POSTGRES_CONN_MAX_IDLE_TIME_MINUTES", "postgres.conn_max_idle_time_minutes", func(c *Configuration) *int { return &c.Postgres.ConnMaxIdleTime }),

	stringVar("REDIS_MODE", "redis.mode", func(c *Configuration) *string { return &c.Redis.Mode }),
	stringVar("REDIS_HOST", "redis.host", func(c *Configuration) *string { return &c.Redis.Host }),
	stringVar("REDIS_PORT", "redis.port", func(c *Configuration) *string { return &c.Redis.Port }),
	stringVar("REDIS_USERNAME", "redis.username", func(c *Configuration) *string { return &c.Redis.Username }),
	stringVar("REDIS_PASSWORD", "redis.password", func(c *Configuration) *string { return &c.Redis.Password }),
	stringVar("REDIS_SENTINEL_MASTER_NAME", "redis.sentinel.master_name", func(c *Configuration) *string { return &c.Redis.Sentinel.MasterName }),
	listVar("REDIS_SENTINEL_ADDRESSES", "redis.sentinel.addresses", func(c *Configuration) *[]string { return &c.Redis.Sentinel.Addresses }),
	stringVar("REDIS_SENTINEL_USERNAME", "redis.sentinel.username", func(c *Configuration) *string { return &c.Redis.Sentinel.Username }),
	stringVar("REDIS_SENTINEL_PASSWORD", "redis.sentinel.password", func(c *Configuration) *string { return &c.Redis.Sentinel.Password }),
	listVar("REDIS_CLUSTER_ADDRESSES", "redis.cluster.addresses", func(c *Configuration) *[]string { return &c.Redis.Cluster.Addresses }),
	intVar("REDIS_DURABLE_WRITES_REPLICAS", "redis.durable_writes.replicas", func(c *Configuration) *int { return &c.Redis.DurableWrites.Replicas }),
	intVar("REDIS_DURABLE_WRITES_TIMEOUT_MS", "redis.durable_writes.timeout_ms", func(c *Configuration) *int { return &c.Redis.DurableWrites.TimeoutMs }),
	boolVar("REDIS_TLS_ENABLED", "redis.tls.enabled", func(c *Configuration) *bool { return &c.Redis.TLS.Enabled }),
	stringVar("REDIS_TLS_CA_FILE", "redis.tls.ca_file", func(c *Configuration) *string { return &c.Redis.TLS.CAFile }),
	stringVar("REDIS_TLS_SERVER_NAME", "redis.tls.server_name", func(c *Configuration) *string { return &c.Redis.TLS.ServerName }),
	stringVar("REDIS_TLS_CERT_FILE", "redis.tls.cert_file", func(c *Configuration) *string { return &c.Redis.TLS.CertFile }),
	stringVar("REDIS_TLS_KEY_FILE", "redis.tls.key_file", func(c *Configuration) *string { return &c.Redis.TLS.KeyFile }),
	boolVar("REDIS_TLS_PRIVATE_NETWORK", "redis.tls.private_network", func(c *Configuration) *bool { return &c.Redis.TLS.PrivateNetwork }),
	intVar("REDIS_DB", "redis.db", func(c *Configuration) *int { return &c.Redis.DB }),
	intVar("REDIS_POOL_SIZE", "redis.pool_size", func(c *Configuration) *int { return &c.Redis.PoolSize }),
	intVar("REDIS_MIN_IDLE_CONNS", "redis.min_idle_conns", func(c *Configuration) *int { return &c.Redis.MinIdleConns }),
	intVar("REDIS_DIAL_TIMEOUT_MS", "redis.dial_timeout_ms", func(c *Configuration) *int { return &c.Redis.DialTimeoutMs }),
	intVar("REDIS_READ_TIMEOUT_MS", "redis.read_timeout_ms", func(c *Configuration) *int { return &c.Redis.ReadTimeoutMs }),
	intVar("REDIS_WRITE_TIMEOUT_MS", "redis.write_timeout_ms", func(c *Configuration) *int { return &c.Redis.WriteTimeoutMs }),
	intVar("REDIS_POOL_TIMEOUT_MS", "redis.pool_timeout_ms", func(c *Configuration) *int { return &c.Redis.PoolTimeoutMs }),
	intVar("REDIS_MAX_RETRIES", "redis.max_retries", func(c *Configuration) *int { return &c.Redis.MaxRetries }),
	intVar("REDIS_CURRENCY_CACHE_TTL_SECONDS", "redis.currency_cache_ttl_seconds", func(c *Configuration) *int { return &c.Redis.CurrencyCacheTTL }),

	stringVar("RABBITMQ_HOST", "rabbitmq.host", func(c *Configuration) *string { return &c.RabbitMQ.Host }),
	stringVar("RABBITMQ_PORT", "rabbitmq.port", func(c *Configuration) *string { return &c.RabbitMQ.Port }),
	listVar("RABBITMQ_HOSTS", "rabbitmq.hosts", func(c *Configuration) *[]string { return &c.RabbitMQ.Hosts }),
	stringVar("RABBITMQ_USER", "rabbitmq.username", func(c *Configuration) *string { return &c.RabbitMQ.Username }),
	stringVar("RABBITMQ_PASSWORD", "rabbitmq.password", func(c *Configuration) *string { return &c.RabbitMQ.Password }),
	stringVar("RABBITMQ_VHOST", "rabbitmq.vhost", func(c *Configuration) *string { return &c.RabbitMQ.VHost }),
	boolVar("RABBITMQ_TLS_ENABLED", "rabbitmq.tls.enabled", func(c *Configuration) *bool { return &c.RabbitMQ.TLS.Enabled }),
	stringVar("RABBITMQ_TLS_CA_FILE", "rabbitmq.tls.ca_file", func(c *Configuration) *string { return &c.RabbitMQ.TLS.CAFile }),
	stringVar("RABBITMQ_TLS_SERVER_NAME", "rabbitmq.tls.server_name", func(c *Configuration) *string { return &c.RabbitMQ.TLS.ServerName }),
	stringVar("RABBITMQ_TLS_CERT_FILE", "rabbitmq.tls.cert_file", func(c *Configuration) *string { return &c.RabbitMQ.TLS.CertFile }),
	stringVar("RABBITMQ_TLS_KEY_FILE", "rabbitmq.tls.key_file", func(c *Configuration) *string { return &c.RabbitMQ.TLS.KeyFile }),
	boolVar("RABBITMQ_TLS_PRIVATE_NETWORK", "rabbitmq.tls.private_network", func(c *Configuration) *bool { return &c.RabbitMQ.TLS.PrivateNetwork }),

	pathVar("JWT_SSL_USER_PRIVATE_KEY", "jwt.ssl.user.private_key", func(c *Configuration) *KeyPath { return &c.JWT.SSL.User.PrivateKey }),
	pathVar("JWT_SSL_USER_PUBLIC_KEY", "jwt.ssl.user.public_key", func(c *Configuration) *KeyPath { return &c.JWT.SSL.User.PublicKey }),
	pathVar("JWT_SSL_ADMIN_PRIVATE_KEY", "jwt.ssl.admin.private_key", func(c *Configuration) *KeyPath { return &c.JWT.SSL.Admin.PrivateKey }),
	pathVar("JWT_SSL_ADMIN_PUBLIC_KEY", "jwt.ssl.admin.public_key", func(c *Configuration) *KeyPath { return &c.JWT.SSL.Admin.PublicKey }),
	stringVar("JWT_ISSUER", "jwt.issuer", func(c *Configuration) *string { return &c.JWT.Issuer }),
	intVar("JWT_ACCESS_TOKEN_EXPIRATION_MINUTES", "jwt.access_token.expiration_minutes", func(c *Configuration) *int { return &c.JWT.AccessToken.Expiration }),
	intVar("JWT_REFRESH_TOKEN_EXPIRATION_MINUTES", "jwt.refresh_token.expiration_minutes", func(c *Configuration) *int { return &c.JWT.RefreshToken.Expiration }),
	intVar("JWT_REFRESH_TOKEN_REUSE_GRACE_SECONDS", "jwt.refresh_token.reuse_grace_seconds", func(c *Configuration) *int { return &c.JWT.RefreshToken.ReuseGraceSeconds }),
	intVar("JWT_SESSIONS_MAX_PER_USER", "jwt.sessions.max_per_user", func(c *Configuration) *int { return &c.JWT.Sessions.MaxPerUser }),

	stringVar("OTP_AUTH_NAME", "otp.auth.name", func(c *Configuration) *string { return &c.OTP.Auth.Name }),
	intVar("OTP_AUTH_TTL_SECONDS", "otp.auth.ttl_seconds", func(c *Configuration) *int { return &c.OTP.Auth.TTL }),
	intVar("OTP_AUTH_LENGTH", "otp.auth.length", func(c *Configuration) *int { return &c.OTP.Auth.Length }),
	intVar("OTP_AUTH_RETRY_LIMIT", "otp.auth.retry_limit", func(c *Configuration) *int { return &c.OTP.Auth.RetryLimit }),
	stringVar("OTP_SECRET", "otp.auth.secret", func(c *Configuration) *string { return &c.OTP.Auth.Secret }),
	stringVar("ADMIN_TOTP_KEY", "admin.totp_key", func(c *Configuration) *string { return &c.Admin.TOTPKey }),
	intVar("ADMIN_SIGN_IN_MIN_RESPONSE_MS", "admin.sign_in_min_response_ms", func(c *Configuration) *int { return &c.Admin.SignInMinResponseMS }),
	intVar("FAVORITES_MAX_PER_USER", "favorites.max_per_user", func(c *Configuration) *int { return &c.Favorites.MaxPerUser }),
	intVar("DELISTING_INTERVAL_SECONDS", "delisting.interval_seconds", func(c *Configuration) *int { return &c.Delisting.IntervalSeconds }),
	intVar("PLATFORM_POLL_INTERVAL_MS", "platform.poll_interval_ms", func(c *Configuration) *int { return &c.Platform.PollIntervalMs }),
	intVar("PLATFORM_STALE_AFTER_SECONDS", "platform.stale_after_seconds", func(c *Configuration) *int { return &c.Platform.StaleAfterSeconds }),
	listVar("PLATFORM_FAIL_CLOSED", "platform.fail_closed", func(c *Configuration) *[]string { return &c.Platform.FailClosed }),
	listVar("PLATFORM_LIFT_APPROVAL", "platform.lift_approval", func(c *Configuration) *[]string { return &c.Platform.LiftApproval }),

	stringVar("KEYS_PROVIDER", "keys.provider", func(c *Configuration) *string { return &c.Keys.Provider }),
	stringVar("KEYS_LOCAL_KEK", "keys.local.kek", func(c *Configuration) *string { return &c.Keys.Local.KEK }),
	// The VAULT_* names are the ones the Vault and OpenBao tools read.
	stringVar("VAULT_ADDR", "keys.transit.address", func(c *Configuration) *string { return &c.Keys.Transit.Address }),
	stringVar("VAULT_TOKEN", "keys.transit.token", func(c *Configuration) *string { return &c.Keys.Transit.Token }),
	stringVar("VAULT_CACERT", "keys.transit.ca_cert", func(c *Configuration) *string { return &c.Keys.Transit.CACert }),
	stringVar("KEYS_TRANSIT_TOKEN_FILE", "keys.transit.token_file", func(c *Configuration) *string { return &c.Keys.Transit.TokenFile }),
	stringVar("KEYS_TRANSIT_MOUNT", "keys.transit.mount", func(c *Configuration) *string { return &c.Keys.Transit.Mount }),
	stringVar("KEYS_TRANSIT_KEY", "keys.transit.key", func(c *Configuration) *string { return &c.Keys.Transit.Key }),
	intVar("KEYS_TRANSIT_TIMEOUT_SECONDS", "keys.transit.timeout_seconds", func(c *Configuration) *int { return &c.Keys.Transit.Timeout }),
	intVar("KEYS_RELOAD_INTERVAL_SECONDS", "keys.reload_interval_seconds", func(c *Configuration) *int { return &c.Keys.ReloadIntervalSeconds }),
	intVar("KEYS_ACTIVATION_DELAY_SECONDS", "keys.activation_delay_seconds", func(c *Configuration) *int { return &c.Keys.ActivationDelaySeconds }),

	stringVar("SMTP_PASSWORD", "notifications.email.password", func(c *Configuration) *string { return &c.Notifications.Email.Password }),
	stringVar("KAVENEGAR_API_KEY", "notifications.sms.kavenegar.api_key", func(c *Configuration) *string { return &c.Notifications.SMS.Kavenegar.APIKey }),

	listVar("WS_ALLOWED_ORIGINS", "websocket.allowed_origins", func(c *Configuration) *[]string { return &c.WebSocket.AllowedOrigins }),
	intVar("WS_SESSION_CHECK_SECONDS", "websocket.session_check_seconds", func(c *Configuration) *int { return &c.WebSocket.SessionCheckSeconds }),
	intVar("WS_PING_INTERVAL_SECONDS", "websocket.ping_interval_seconds", func(c *Configuration) *int { return &c.WebSocket.PingInterval }),
	intVar("WS_MAX_MESSAGE_SIZE_BYTES", "websocket.max_message_size_bytes", func(c *Configuration) *int { return &c.WebSocket.MaxMessageSize }),
	intVar("WS_WRITE_TIMEOUT_SECONDS", "websocket.write_timeout_seconds", func(c *Configuration) *int { return &c.WebSocket.WriteTimeout }),
	intVar("WS_CLIENT_MESSAGES_PER_SECOND", "websocket.client_messages_per_second", func(c *Configuration) *int { return &c.WebSocket.ClientMessagesPerSecond }),
	intVar("WS_CLIENT_MESSAGE_BURST", "websocket.client_message_burst", func(c *Configuration) *int { return &c.WebSocket.ClientMessageBurst }),
	intVar("WS_MAX_CONNECTIONS", "websocket.max_connections", func(c *Configuration) *int { return &c.WebSocket.MaxConnections }),
	intVar("WS_MAX_CONNECTIONS_PER_IP", "websocket.max_connections_per_ip", func(c *Configuration) *int { return &c.WebSocket.MaxConnectionsPerIP }),
	intVar("WS_MAX_CONNECTIONS_PER_USER", "websocket.max_connections_per_user", func(c *Configuration) *int { return &c.WebSocket.MaxConnectionsPerUser }),

	intVar("RATE_LIMITS_TIMEOUT_MS", "rate_limits.timeout_ms", func(c *Configuration) *int { return &c.RateLimits.TimeoutMs }),
	intVar("RATE_LIMITS_IPV6_PREFIX_LENGTH", "rate_limits.ipv6_prefix_length", func(c *Configuration) *int { return &c.RateLimits.IPv6PrefixLength }),
	intVar("RATE_LIMITS_REST_REQUESTS_PER_SECOND", "rate_limits.rest.requests_per_second", func(c *Configuration) *int { return &c.RateLimits.REST.RequestsPerSecond }),
	intVar("RATE_LIMITS_REST_BURST", "rate_limits.rest.burst", func(c *Configuration) *int { return &c.RateLimits.REST.Burst }),
	intVar("RATE_LIMITS_WS_MESSAGES_PER_SECOND", "rate_limits.ws.messages_per_second", func(c *Configuration) *int { return &c.RateLimits.WS.MessagesPerSecond }),
	intVar("RATE_LIMITS_WS_BURST", "rate_limits.ws.burst", func(c *Configuration) *int { return &c.RateLimits.WS.Burst }),
	intVar("RATE_LIMITS_TRADING_ORDERS_PER_SECOND", "rate_limits.trading.orders_per_second", func(c *Configuration) *int { return &c.RateLimits.Trading.OrdersPerSecond }),
	intVar("RATE_LIMITS_TRADING_BURST", "rate_limits.trading.burst", func(c *Configuration) *int { return &c.RateLimits.Trading.Burst }),
	intVar("RATE_LIMITS_AUTH_LOGIN_ATTEMPTS_PER_MINUTE", "rate_limits.auth.login_attempts_per_minute", func(c *Configuration) *int { return &c.RateLimits.Auth.LoginAttemptsPerMinute }),
	intVar("RATE_LIMITS_AUTH_OTP_REQUESTS_PER_HOUR", "rate_limits.auth.otp_requests_per_hour", func(c *Configuration) *int { return &c.RateLimits.Auth.OTPRequestsPerHour }),
	stringVar("RATE_LIMITS_REST_ON_ERROR", "rate_limits.rest.on_error", func(c *Configuration) *string { return &c.RateLimits.REST.OnError }),
	stringVar("RATE_LIMITS_WS_ON_ERROR", "rate_limits.ws.on_error", func(c *Configuration) *string { return &c.RateLimits.WS.OnError }),
	stringVar("RATE_LIMITS_TRADING_ON_ERROR", "rate_limits.trading.on_error", func(c *Configuration) *string { return &c.RateLimits.Trading.OnError }),
	stringVar("RATE_LIMITS_AUTH_ON_ERROR", "rate_limits.auth.on_error", func(c *Configuration) *string { return &c.RateLimits.Auth.OnError }),

	stringVar("LOG_LEVEL", "observability.logging.level", func(c *Configuration) *string { return &c.Observability.Logging.Level }),
	stringVar("OTEL_ENDPOINT", "observability.tracing.endpoint", func(c *Configuration) *string { return &c.Observability.Tracing.Endpoint }),

	providerVar("BINANCE_API_KEY", "price_aggregator.providers[binance].api_key", func(c *Configuration, v string) {
		for i := range c.PriceAggregator.Providers {
			if c.PriceAggregator.Providers[i].Name == "binance" {
				c.PriceAggregator.Providers[i].APIKey = v
			}
		}
	}),
	providerVar("BINANCE_API_SECRET", "price_aggregator.providers[binance].api_secret", func(c *Configuration, v string) {
		for i := range c.PriceAggregator.Providers {
			if c.PriceAggregator.Providers[i].Name == "binance" {
				c.PriceAggregator.Providers[i].APISecret = v
			}
		}
	}),
	providerVar("FIAT_PROVIDER_API_KEY", "fiat.providers[*].api_key", func(c *Configuration, v string) {
		for i := range c.Fiat.Providers {
			c.Fiat.Providers[i].APIKey = v
		}
	}),
	providerVar("FIAT_PROVIDER_API_SECRET", "fiat.providers[*].api_secret", func(c *Configuration, v string) {
		for i := range c.Fiat.Providers {
			c.Fiat.Providers[i].APISecret = v
		}
	}),
	providerVar("CHAINALYSIS_API_KEY", "compliance.sanctions.providers[chainalysis].api_key", func(c *Configuration, v string) {
		for i := range c.Compliance.Sanctions.Providers {
			if c.Compliance.Sanctions.Providers[i].Name == "chainalysis" {
				c.Compliance.Sanctions.Providers[i].APIKey = v
			}
		}
	}),
	providerVar("TRM_API_KEY", "compliance.sanctions.providers[trm].api_key", func(c *Configuration, v string) {
		for i := range c.Compliance.Sanctions.Providers {
			if c.Compliance.Sanctions.Providers[i].Name == "trm" {
				c.Compliance.Sanctions.Providers[i].APIKey = v
			}
		}
	}),
	providerVar("SUMSUB_API_KEY", "compliance.kyc.providers[sumsub].api_key", func(c *Configuration, v string) {
		for i := range c.Compliance.KYC.Providers {
			if c.Compliance.KYC.Providers[i].Name == "sumsub" {
				c.Compliance.KYC.Providers[i].APIKey = v
			}
		}
	}),
	providerVar("ONFIDO_API_KEY", "compliance.kyc.providers[onfido].api_key", func(c *Configuration, v string) {
		for i := range c.Compliance.KYC.Providers {
			if c.Compliance.KYC.Providers[i].Name == "onfido" {
				c.Compliance.KYC.Providers[i].APIKey = v
			}
		}
	}),
}

// EnvVars returns the environment variables that the configuration reads.
func EnvVars() []EnvVar {
	return append([]EnvVar(nil), envVars...)
}

// envVarFor returns the variable that overrides key. GIN_MODE, the alias of
// SERVER_MODE, is never returned.
func envVarFor(key string) string {
	for _, v := range envVars {
		if v.Key == key && v.Name != "GIN_MODE" {
			return v.Name
		}
	}
	return ""
}

// overlayEnv applies the environment variables to c. Every invalid value is
// reported, not only the first, on one line.
func overlayEnv(c *Configuration, lookup func(string) string) error {
	var problems []string
	for _, v := range envVars {
		value := lookup(v.Name)
		if value == "" {
			continue
		}
		if err := v.set(c, value); err != nil {
			problems = append(problems, fmt.Sprintf("%s (%s): %v", v.Name, v.Key, err))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func overlayEnvVars() error {
	return overlayEnv(&Config, os.Getenv)
}
