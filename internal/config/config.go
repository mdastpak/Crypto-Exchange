package config

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	configfiles "github.com/mdastpak/crypto-exchange/config"

	"go.yaml.in/yaml/v3"
)

var Config Configuration

var configPath string = "config/config.yaml"
var migrationsPath string = "file://internal/db/migrations"

func SetConfigPath(path string) {
	configPath = path
}

func SetMigrationsPath(path string) {
	migrationsPath = path
}

func ConfigPath() string {
	return configPath
}

func MigrationsPath() string {
	return migrationsPath
}

type Configuration struct {
	Server struct {
		IP           string `yaml:"ip"`
		Port         string `yaml:"port"`
		ReadTimeout  int    `yaml:"read_timeout_seconds"`
		WriteTimeout int    `yaml:"write_timeout_seconds"`
		IdleTimeout  int    `yaml:"idle_timeout_seconds"`
		Mode         string `yaml:"mode"`
		// TrustedProxies lists the IP addresses or CIDR ranges of reverse
		// proxies whose X-Forwarded-For and X-Real-IP headers are trusted.
		// Empty means none: the client address is the TCP peer address.
		TrustedProxies []string `yaml:"trusted_proxies"`
		TLS            struct {
			Enabled  bool   `yaml:"enabled"`
			CertFile string `yaml:"cert_file"`
			KeyFile  string `yaml:"key_file"`
			// BehindProxy says that a reverse proxy terminates TLS in front
			// of the server. Release mode needs it or Enabled.
			BehindProxy bool `yaml:"behind_proxy"`
		} `yaml:"tls"`
		CORS struct {
			// AllowedOrigins lists the browser origins (scheme://host[:port])
			// that may call the API with credentials. Empty means none.
			AllowedOrigins []string `yaml:"allowed_origins"`
		} `yaml:"cors"`
		Swagger struct {
			// Enabled serves the Swagger UI at /swagger/. Unset, it is
			// served outside release mode only.
			Enabled *bool `yaml:"enabled"`
		} `yaml:"swagger"`
		// MaxBodyBytes is the largest request body the server accepts.
		MaxBodyBytes int64 `yaml:"max_body_bytes"`
		// ShutdownTimeout bounds the whole shutdown. DrainSeconds is how
		// long the instance keeps serving after it reports not ready, so
		// that load balancers stop sending it requests first.
		ShutdownTimeout int `yaml:"shutdown_timeout_seconds"`
		DrainSeconds    int `yaml:"drain_seconds"`
	} `yaml:"server"`

	Postgres struct {
		Host                string `yaml:"host"`
		Port                string `yaml:"port"`
		User                string `yaml:"user"`
		Password            string `yaml:"password"`
		DBName              string `yaml:"dbname"`
		SSLMode             string `yaml:"sslmode"`
		MaxOpenConns        int    `yaml:"max_open_conns"`
		MaxIdleConns        int    `yaml:"max_idle_conns"`
		ConnMaxLifetime     int    `yaml:"conn_max_lifetime_minutes"`
		ConnMaxIdleTime     int    `yaml:"conn_max_idle_time_minutes"`
		ConnectRetries      int    `yaml:"connect_retries"`
		ConnectRetryDelayMs int    `yaml:"connect_retry_delay_ms"`
	} `yaml:"postgres"`

	Redis struct {
		Host                string `yaml:"host"`
		Port                string `yaml:"port"`
		Password            string `yaml:"password"`
		DB                  int    `yaml:"db"`
		PoolSize            int    `yaml:"pool_size"`
		MinIdleConns        int    `yaml:"min_idle_conns"`
		ConnectRetries      int    `yaml:"connect_retries"`
		ConnectRetryDelayMs int    `yaml:"connect_retry_delay_ms"`
		// The client's timeouts, in milliseconds. Redis is on the path of
		// most requests, so a command that waits holds the request.
		DialTimeoutMs  int `yaml:"dial_timeout_ms"`
		ReadTimeoutMs  int `yaml:"read_timeout_ms"`
		WriteTimeoutMs int `yaml:"write_timeout_ms"`
		PoolTimeoutMs  int `yaml:"pool_timeout_ms"`
		// MaxRetries is how often a command is retried after a network
		// error; -1 turns retries off.
		MaxRetries int `yaml:"max_retries"`
		// CurrencyCacheTTL is how long the list of public currencies stays
		// cached. Changes invalidate it at once; the time bounds how long a
		// missed invalidation can leave it out of date.
		CurrencyCacheTTL int `yaml:"currency_cache_ttl_seconds"`
	} `yaml:"redis"`

	RabbitMQ struct {
		Username  string `yaml:"username"`
		Password  string `yaml:"password"`
		Host      string `yaml:"host"`
		Port      string `yaml:"port"`
		Exchanges struct {
			PriceUpdates  string `yaml:"price_updates"`
			TradeEvents   string `yaml:"trade_events"`
			OrderEvents   string `yaml:"order_events"`
			Settlement    string `yaml:"settlement"`
			Notifications string `yaml:"notifications"`
		} `yaml:"exchanges"`
	} `yaml:"rabbitmq"`

	JWT struct {
		SSL struct {
			// Admin keys are optional: without them, admin routes reject
			// every token. They must differ from the user keys.
			Admin JWTKeyPaths `yaml:"admin"`
			User  JWTKeyPaths `yaml:"user"`
		} `yaml:"ssl"`
		Issuer      string `yaml:"issuer"`
		AccessToken struct {
			Expiration int `yaml:"expiration_minutes"`
		} `yaml:"access_token"`
		RefreshToken struct {
			Expiration int `yaml:"expiration_minutes"`
		} `yaml:"refresh_token"`
	} `yaml:"jwt"`

	OTP struct {
		Auth struct {
			Name       string `yaml:"name"`
			TTL        int    `yaml:"ttl_seconds"`
			Length     int    `yaml:"length"`
			RetryLimit int    `yaml:"retry_limit"`
			// DevLogCodes writes sign-in codes to the log for local
			// development. The server refuses to start with it in release mode.
			DevLogCodes bool `yaml:"dev_log_codes"`
			// Secret keys the hashes of sign-in codes and the encryption of
			// contacts in Redis. Every server instance must use the same
			// secret, and it is required in release mode.
			Secret string `yaml:"secret"`
			// ContactRequestsPerHour bounds the codes issued to one contact.
			ContactRequestsPerHour int `yaml:"contact_requests_per_hour"`
			// ContactAttemptsPerHour bounds the verification attempts for
			// one contact across all of its codes.
			ContactAttemptsPerHour int `yaml:"contact_attempts_per_hour"`
		} `yaml:"auth"`
	} `yaml:"otp"`

	Notifications NotificationsConfig `yaml:"notifications"`

	Admin struct {
		// TOTPKey encrypts the authenticator (TOTP) secrets of
		// administrators in the database: 32 random bytes, base64-encoded.
		// Admin sign-in is disabled without it.
		TOTPKey string `yaml:"totp_key"`
	} `yaml:"admin"`

	// Keys says where the key-encryption key (KEK) of the data-protection
	// keys lives. See docs/data-protection.md.
	Keys KeysConfig `yaml:"keys"`

	WebSocket struct {
		AllowedOrigins []string `yaml:"allowed_origins"`
		// PingInterval is how often the server pings a client. A client
		// that does not answer within two intervals is disconnected.
		PingInterval int `yaml:"ping_interval_seconds"`
		// MaxMessageSize bounds a message from a client; a larger one ends
		// the connection.
		MaxMessageSize int `yaml:"max_message_size_bytes"`
		WriteTimeout   int `yaml:"write_timeout_seconds"`
		// ClientMessagesPerSecond and ClientMessageBurst limit the messages
		// of each connection; a client that sends faster is disconnected.
		// rate_limits.ws limits the upgrade requests instead.
		ClientMessagesPerSecond int `yaml:"client_messages_per_second"`
		ClientMessageBurst      int `yaml:"client_message_burst"`
		// MaxConnections and MaxConnectionsPerIP cap the open connections of
		// this instance.
		MaxConnections      int `yaml:"max_connections"`
		MaxConnectionsPerIP int `yaml:"max_connections_per_ip"`
	} `yaml:"websocket"`

	Exchange struct {
		Name              string `yaml:"name"`
		Timezone          string `yaml:"timezone"`
		MaintenanceWindow struct {
			Enabled  bool   `yaml:"enabled"`
			Start    string `yaml:"start"`
			End      string `yaml:"end"`
			Timezone string `yaml:"timezone"`
		} `yaml:"maintenance_window"`
	} `yaml:"exchange"`

	Assets struct {
		RegistryPath string `yaml:"registry_path"`
		AutoSync     bool   `yaml:"auto_sync"`
	} `yaml:"assets"`

	Markets struct {
		DefaultMakerFeeBPS int  `yaml:"default_maker_fee_bps"`
		DefaultTakerFeeBPS int  `yaml:"default_taker_fee_bps"`
		EnableMarketOrders bool `yaml:"enable_market_orders"`
		EnableStopOrders   bool `yaml:"enable_stop_orders"`
	} `yaml:"markets"`

	PriceAggregator struct {
		Enabled             bool                  `yaml:"enabled"`
		Providers           []PriceProviderConfig `yaml:"providers"`
		AggregationInterval int                   `yaml:"aggregation_interval_ms"`
		VWAPWindows         []int                 `yaml:"vwap_windows_seconds"`
		OutlierThreshold    float64               `yaml:"outlier_threshold_mad"`
		MinProviders        int                   `yaml:"min_providers"`
		StaleThreshold      int                   `yaml:"stale_threshold_ms"`
		PublishToRedis      bool                  `yaml:"publish_to_redis"`
		PublishToRabbitMQ   bool                  `yaml:"publish_to_rabbitmq"`
	} `yaml:"price_aggregator"`

	Custody struct {
		Providers []CustodyProviderConfig `yaml:"providers"`
	} `yaml:"custody"`

	Fiat struct {
		Providers []FiatProviderConfig `yaml:"providers"`
	} `yaml:"fiat"`

	Compliance struct {
		Sanctions struct {
			Providers             []SanctionsProviderConfig `yaml:"providers"`
			ScreenOnDeposit       bool                      `yaml:"screen_on_deposit"`
			ScreenOnWithdrawal    bool                      `yaml:"screen_on_withdrawal"`
			ScreenOnTrade         bool                      `yaml:"screen_on_trade"`
			PeriodicRescreenHours int                       `yaml:"periodic_rescreen_hours"`
		} `yaml:"sanctions"`
		AML struct {
			Enabled   bool   `yaml:"enabled"`
			RulesPath string `yaml:"rules_path"`
		} `yaml:"aml"`
		KYC struct {
			Providers          []KYCProviderConfig `yaml:"providers"`
			DefaultTier        string              `yaml:"default_tier"`
			AutoApproveLowRisk bool                `yaml:"auto_approve_low_risk"`
		} `yaml:"kyc"`
	} `yaml:"compliance"`

	Observability struct {
		Logging struct {
			Level    string `yaml:"level"`
			Format   string `yaml:"format"`
			Output   string `yaml:"output"`
			FilePath string `yaml:"file_path"`
		} `yaml:"logging"`
		Metrics struct {
			Enabled bool   `yaml:"enabled"`
			Port    int    `yaml:"port"`
			Path    string `yaml:"path"`
		} `yaml:"metrics"`
		Tracing struct {
			Enabled     bool    `yaml:"enabled"`
			Endpoint    string  `yaml:"endpoint"`
			ServiceName string  `yaml:"service_name"`
			SampleRate  float64 `yaml:"sample_rate"`
		} `yaml:"tracing"`
	} `yaml:"observability"`

	// RateLimits configures the limiters. OnError says what a limiter does
	// with a request while Redis cannot be reached: "allow" lets it through,
	// "deny" rejects it with 503. TimeoutMs bounds each limiter's call to
	// Redis; a call that takes longer counts as Redis being unreachable.
	RateLimits struct {
		TimeoutMs int `yaml:"timeout_ms"`
		REST      struct {
			RequestsPerSecond int    `yaml:"requests_per_second"`
			Burst             int    `yaml:"burst"`
			OnError           string `yaml:"on_error"`
		} `yaml:"rest"`
		WS struct {
			MessagesPerSecond int    `yaml:"messages_per_second"`
			Burst             int    `yaml:"burst"`
			OnError           string `yaml:"on_error"`
		} `yaml:"ws"`
		Trading struct {
			OrdersPerSecond int    `yaml:"orders_per_second"`
			Burst           int    `yaml:"burst"`
			OnError         string `yaml:"on_error"`
		} `yaml:"trading"`
		Auth struct {
			LoginAttemptsPerMinute int    `yaml:"login_attempts_per_minute"`
			OTPRequestsPerHour     int    `yaml:"otp_requests_per_hour"`
			OnError                string `yaml:"on_error"`
		} `yaml:"auth"`
	} `yaml:"rate_limits"`
}

// JWTKeyPaths locates the PEM files of one role.
type JWTKeyPaths struct {
	PrivateKey KeyPath `yaml:"private_key"`
	PublicKey  KeyPath `yaml:"public_key"`
	// RetiredPublicKeys lists the public keys of earlier key pairs. Tokens
	// they signed are accepted until they expire.
	RetiredPublicKeys []KeyPath `yaml:"retired_public_keys"`
}

// KeyPath is the path of a key file. In YAML it is a string. A list of path
// segments, which earlier versions required, is still accepted and joined
// with the platform's separator.
type KeyPath string

// UnmarshalYAML accepts a string or a list of path segments.
func (p *KeyPath) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var path string
	if err := unmarshal(&path); err == nil {
		*p = KeyPath(path)
		return nil
	}
	var segments []string
	if err := unmarshal(&segments); err != nil {
		return errors.New("a key path must be a string")
	}
	*p = KeyPath(filepath.Join(segments...))
	return nil
}

// KeysConfig configures the provider of the key-encryption key that wraps
// the data-protection keys.
type KeysConfig struct {
	// Provider is "transit" (HashiCorp Vault or OpenBao), "local"
	// (development only), or empty, which leaves the keys unused.
	Provider string `yaml:"provider"`
	Local    struct {
		// KEK is base64 of 32 random bytes. Release mode refuses it.
		KEK string `yaml:"kek"`
	} `yaml:"local"`
	Transit struct {
		Address string `yaml:"address"`
		Token   string `yaml:"token"`
		// TokenFile is read on every request, so that an agent can renew
		// the token in place.
		TokenFile string `yaml:"token_file"`
		Mount     string `yaml:"mount"`
		Key       string `yaml:"key"`
		CACert    string `yaml:"ca_cert"`
		Timeout   int    `yaml:"timeout_seconds"`
	} `yaml:"transit"`
}

// Key providers, as keys.provider spells them.
const (
	KeysProviderTransit = "transit"
	KeysProviderLocal   = "local"
)

// DefaultJWTIssuer is the "iss" claim when jwt.issuer is not set.
const DefaultJWTIssuer = "crypto-exchange"

// NotificationsConfig configures how messages such as sign-in codes reach
// users. A channel without configuration is disabled.
type NotificationsConfig struct {
	Email EmailConfig `yaml:"email"`
	SMS   SMSConfig   `yaml:"sms"`
	// Worker configures the consumer that delivers queued messages.
	Worker struct {
		// Concurrency is the number of messages delivered in parallel.
		Concurrency int `yaml:"concurrency"`
		// MaxAttempts bounds the delivery attempts per message. A message is
		// also given up once its sign-in code expires.
		MaxAttempts int `yaml:"max_attempts"`
	} `yaml:"worker"`
}

// EmailConfig configures delivery by SMTP. Email is disabled when Host is empty.
type EmailConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// From is the sender address, optionally with a display name.
	From string `yaml:"from"`
	// ImplicitTLS connects with TLS from the start (usually port 465).
	// Otherwise the connection is upgraded with STARTTLS, which is required
	// before credentials are sent to any host other than localhost.
	ImplicitTLS bool `yaml:"implicit_tls"`
	Timeout     int  `yaml:"timeout_seconds"`
	// OTPSubject and OTPBody are Go text/template templates for sign-in code
	// emails. They can use {{.Code}} and {{.TTLSeconds}}.
	OTPSubject string `yaml:"otp_subject"`
	OTPBody    string `yaml:"otp_body"`
}

// SMSConfig selects and configures the SMS provider. SMS is disabled when
// Provider is empty.
type SMSConfig struct {
	// Provider is "kavenegar" or empty.
	Provider  string          `yaml:"provider"`
	Kavenegar KavenegarConfig `yaml:"kavenegar"`
}

// KavenegarConfig configures the Kavenegar verify lookup API.
type KavenegarConfig struct {
	APIKey string `yaml:"api_key"`
	// OTPTemplate is the name of a verify template approved in the Kavenegar
	// panel. Its %token placeholder receives the sign-in code, so the
	// message text and its language are defined in the panel.
	OTPTemplate string `yaml:"otp_template"`
	BaseURL     string `yaml:"base_url"`
	Timeout     int    `yaml:"timeout_seconds"`
}

// PriceProviderConfig holds configuration for a price data provider
type PriceProviderConfig struct {
	Name           string   `yaml:"name"`
	Enabled        bool     `yaml:"enabled"`
	Type           string   `yaml:"type"`
	WSEndpoint     string   `yaml:"ws_endpoint"`
	RESTEndpoint   string   `yaml:"rest_endpoint"`
	APIKey         string   `yaml:"api_key"`
	APISecret      string   `yaml:"api_secret"`
	Passphrase     string   `yaml:"passphrase"`
	Symbols        []string `yaml:"symbols"`
	RateLimit      int      `yaml:"rate_limit"`
	Timeout        int      `yaml:"timeout_seconds"`
	ReconnectDelay int      `yaml:"reconnect_delay_seconds"`
}

// CustodyProviderConfig holds configuration for a custody provider
type CustodyProviderConfig struct {
	ProviderType          string   `yaml:"provider_type"`
	AssetIDs              []string `yaml:"asset_ids"`
	Network               string   `yaml:"network"`
	Enabled               bool     `yaml:"enabled"`
	RPCEndpoints          []string `yaml:"rpc_endpoints"`
	WSEndpoints           []string `yaml:"ws_endpoints"`
	ExplorerAPI           string   `yaml:"explorer_api"`
	ExplorerWS            string   `yaml:"explorer_ws"`
	APIKey                string   `yaml:"api_key"`
	APISecret             string   `yaml:"api_secret"`
	JWTToken              string   `yaml:"jwt_token"`
	ChainID               string   `yaml:"chain_id"`
	ContractAddress       string   `yaml:"contract_address"`
	Decimals              int      `yaml:"decimals"`
	ConfirmationsRequired int      `yaml:"confirmations_required"`
	MinDepositAmount      string   `yaml:"min_deposit_amount"`
	MinWithdrawalAmount   string   `yaml:"min_withdrawal_amount"`
	MaxWithdrawalAmount   string   `yaml:"max_withdrawal_amount"`
	DefaultFeeLevel       string   `yaml:"default_fee_level"`
	FeeAssetID            string   `yaml:"fee_asset_id"`
	HotWalletAddress      string   `yaml:"hot_wallet_address"`
	ColdWalletAddress     string   `yaml:"cold_wallet_address"`
	BlockPollInterval     int      `yaml:"block_poll_interval_seconds"`
	ReorgDepth            int      `yaml:"reorg_depth"`
	SanctionsScreening    bool     `yaml:"sanctions_screening"`
}

// FiatProviderConfig holds configuration for a fiat provider
type FiatProviderConfig struct {
	Name            string   `yaml:"name"`
	Enabled         bool     `yaml:"enabled"`
	Type            string   `yaml:"type"`
	Endpoint        string   `yaml:"endpoint"`
	APIKey          string   `yaml:"api_key"`
	APISecret       string   `yaml:"api_secret"`
	CertPath        string   `yaml:"cert_path"`
	KeyPath         string   `yaml:"key_path"`
	CAPath          string   `yaml:"ca_path"`
	SupportedAssets []string `yaml:"supported_assets"`
	WebhookSecret   string   `yaml:"webhook_secret"`
	Timeout         int      `yaml:"timeout_seconds"`
}

// SanctionsProviderConfig holds configuration for a sanctions screening provider
type SanctionsProviderConfig struct {
	Name      string `yaml:"name"`
	Enabled   bool   `yaml:"enabled"`
	APIKey    string `yaml:"api_key"`
	APISecret string `yaml:"api_secret"`
	Endpoint  string `yaml:"endpoint"`
	Priority  int    `yaml:"priority"`
	Timeout   int    `yaml:"timeout_seconds"`
}

// KYCProviderConfig holds configuration for a KYC provider
type KYCProviderConfig struct {
	Name          string `yaml:"name"`
	Enabled       bool   `yaml:"enabled"`
	APIKey        string `yaml:"api_key"`
	APISecret     string `yaml:"api_secret"`
	Endpoint      string `yaml:"endpoint"`
	WebhookSecret string `yaml:"webhook_secret"`
	Timeout       int    `yaml:"timeout_seconds"`
}

// LoadConfig loads the configuration as Load does, and stops the process
// when it is invalid.
func LoadConfig() {
	if err := Load(); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}
}

// Load reads the configuration file, applies the environment variables,
// fills in the defaults, and checks the values that every command uses. The
// settings that only the server needs are checked by ValidateServer.
func Load() error {
	var data []byte
	var err error

	if _, statErr := os.Stat(configPath); statErr == nil {
		data, err = os.ReadFile(configPath)
		if err != nil {
			return fmt.Errorf("reading %s: %w", configPath, err)
		}
	} else {
		log.Printf("%s does not exist: using the embedded config/config.example.yaml", configPath)
		data = configfiles.Example
	}

	Config = Configuration{}
	if err := yaml.Unmarshal(data, &Config); err != nil {
		return fmt.Errorf("parsing %s: %w", configPath, err)
	}
	if err := overlayEnvVars(); err != nil {
		return fmt.Errorf("invalid environment variables: %w", err)
	}
	setDefaults()
	if err := Config.validateMode(); err != nil {
		return err
	}
	if err := Config.validateRateLimits(); err != nil {
		return err
	}
	if err := Config.validateRedisTimeouts(); err != nil {
		return err
	}
	if err := Config.validateKeys(); err != nil {
		return err
	}
	validateConfig()
	return nil
}

func (c *Configuration) validateRedisTimeouts() error {
	for _, setting := range []struct {
		key string
		ms  int
	}{
		{"redis.dial_timeout_ms", c.Redis.DialTimeoutMs},
		{"redis.read_timeout_ms", c.Redis.ReadTimeoutMs},
		{"redis.write_timeout_ms", c.Redis.WriteTimeoutMs},
		{"redis.pool_timeout_ms", c.Redis.PoolTimeoutMs},
		{"rate_limits.timeout_ms", c.RateLimits.TimeoutMs},
	} {
		if setting.ms < 0 {
			return fmt.Errorf("%s (%s) is %d; it must be a positive number of milliseconds", setting.key, envVarFor(setting.key), setting.ms)
		}
	}
	if c.Redis.MaxRetries < -1 {
		return fmt.Errorf("redis.max_retries (%s) is %d; it must be -1, which turns retries off, or more", envVarFor("redis.max_retries"), c.Redis.MaxRetries)
	}
	return nil
}

// What a rate limiter does with a request while Redis cannot be reached.
const (
	RateLimitAllow = "allow"
	RateLimitDeny  = "deny"
)

func (c *Configuration) validateRateLimits() error {
	limits := c.RateLimits
	for key, onError := range map[string]string{
		"rate_limits.rest.on_error":    limits.REST.OnError,
		"rate_limits.ws.on_error":      limits.WS.OnError,
		"rate_limits.trading.on_error": limits.Trading.OnError,
		"rate_limits.auth.on_error":    limits.Auth.OnError,
	} {
		if onError != RateLimitAllow && onError != RateLimitDeny {
			return fmt.Errorf("%s (%s) is %q; it must be %q or %q", key, envVarFor(key), onError, RateLimitAllow, RateLimitDeny)
		}
	}
	return nil
}

func (c *Configuration) validateKeys() error {
	switch c.Keys.Provider {
	case "", KeysProviderTransit, KeysProviderLocal:
	default:
		return fmt.Errorf("keys.provider (%s) is %q; it must be %q, %q, or empty",
			envVarFor("keys.provider"), c.Keys.Provider, KeysProviderTransit, KeysProviderLocal)
	}
	if c.Keys.Transit.Timeout < 0 {
		return fmt.Errorf("keys.transit.timeout_seconds (%s) is %d; it must be a positive number of seconds",
			envVarFor("keys.transit.timeout_seconds"), c.Keys.Transit.Timeout)
	}
	return nil
}

// Server modes. They are the modes of Gin.
var serverModes = []string{"debug", "release", "test"}

func (c *Configuration) validateMode() error {
	for _, mode := range serverModes {
		if c.Server.Mode == mode {
			return nil
		}
	}
	return fmt.Errorf("server.mode (%s) is %q; it must be one of %s",
		envVarFor("server.mode"), c.Server.Mode, strings.Join(serverModes, ", "))
}

// ValidateServer reports the settings that the server cannot start without,
// all at once, each with the environment variable that can set it.
func ValidateServer() error {
	return Config.validateServer()
}

func setDefaults() {
	if Config.Server.ReadTimeout == 0 {
		Config.Server.ReadTimeout = 10
	}
	if Config.Server.WriteTimeout == 0 {
		Config.Server.WriteTimeout = 10
	}
	if Config.Server.IdleTimeout == 0 {
		Config.Server.IdleTimeout = 120
	}
	if Config.Server.Mode == "" {
		Config.Server.Mode = "debug"
	}
	if Config.Server.MaxBodyBytes == 0 {
		Config.Server.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if Config.Server.ShutdownTimeout == 0 {
		Config.Server.ShutdownTimeout = DefaultShutdownTimeoutSeconds
	}

	if Config.JWT.Issuer == "" {
		Config.JWT.Issuer = DefaultJWTIssuer
	}

	if Config.Postgres.MaxOpenConns == 0 {
		Config.Postgres.MaxOpenConns = 25
	}
	if Config.Postgres.MaxIdleConns == 0 {
		Config.Postgres.MaxIdleConns = 5
	}
	if Config.Postgres.ConnMaxLifetime == 0 {
		Config.Postgres.ConnMaxLifetime = 5
	}
	if Config.Postgres.ConnMaxIdleTime == 0 {
		Config.Postgres.ConnMaxIdleTime = 5
	}
	if Config.Postgres.SSLMode == "" {
		Config.Postgres.SSLMode = "disable"
	}
	if Config.Postgres.ConnectRetries == 0 {
		Config.Postgres.ConnectRetries = 3
	}
	if Config.Postgres.ConnectRetryDelayMs == 0 {
		Config.Postgres.ConnectRetryDelayMs = 500
	}

	if Config.Redis.PoolSize == 0 {
		Config.Redis.PoolSize = 10
	}
	if Config.Redis.MinIdleConns == 0 {
		Config.Redis.MinIdleConns = 2
	}
	if Config.Redis.ConnectRetries == 0 {
		Config.Redis.ConnectRetries = 3
	}
	if Config.Redis.ConnectRetryDelayMs == 0 {
		Config.Redis.ConnectRetryDelayMs = 500
	}
	for _, ms := range []struct {
		value    *int
		fallback int
	}{
		{&Config.Redis.DialTimeoutMs, DefaultRedisDialTimeoutMs},
		{&Config.Redis.ReadTimeoutMs, DefaultRedisReadTimeoutMs},
		{&Config.Redis.WriteTimeoutMs, DefaultRedisWriteTimeoutMs},
		{&Config.Redis.PoolTimeoutMs, DefaultRedisPoolTimeoutMs},
		{&Config.Redis.MaxRetries, DefaultRedisMaxRetries},
		{&Config.Redis.CurrencyCacheTTL, DefaultCurrencyCacheTTLSeconds},
		{&Config.RateLimits.TimeoutMs, DefaultRateLimitTimeoutMs},
	} {
		if *ms.value == 0 {
			*ms.value = ms.fallback
		}
	}

	setNotificationDefaults(&Config.Notifications)

	if Config.Keys.Transit.Mount == "" {
		Config.Keys.Transit.Mount = "transit"
	}
	if Config.Keys.Transit.Timeout == 0 {
		Config.Keys.Transit.Timeout = 10
	}

	if Config.WebSocket.PingInterval == 0 {
		Config.WebSocket.PingInterval = 30
	}
	if Config.WebSocket.MaxMessageSize == 0 {
		Config.WebSocket.MaxMessageSize = DefaultWSMaxMessageSize
	}
	for _, setting := range []struct {
		value    *int
		fallback int
	}{
		{&Config.WebSocket.WriteTimeout, DefaultWSWriteTimeoutSeconds},
		{&Config.WebSocket.ClientMessagesPerSecond, DefaultWSClientMessagesPerSecond},
		{&Config.WebSocket.ClientMessageBurst, DefaultWSClientMessageBurst},
		{&Config.WebSocket.MaxConnections, DefaultWSMaxConnections},
		{&Config.WebSocket.MaxConnectionsPerIP, DefaultWSMaxConnectionsPerIP},
	} {
		if *setting.value == 0 {
			*setting.value = setting.fallback
		}
	}

	if Config.Exchange.Timezone == "" {
		Config.Exchange.Timezone = "UTC"
	}

	if Config.Markets.DefaultMakerFeeBPS == 0 {
		Config.Markets.DefaultMakerFeeBPS = 10
	}
	if Config.Markets.DefaultTakerFeeBPS == 0 {
		Config.Markets.DefaultTakerFeeBPS = 20
	}

	if Config.PriceAggregator.AggregationInterval == 0 {
		Config.PriceAggregator.AggregationInterval = 100
	}
	if len(Config.PriceAggregator.VWAPWindows) == 0 {
		Config.PriceAggregator.VWAPWindows = []int{1, 5, 60, 300}
	}
	if Config.PriceAggregator.OutlierThreshold == 0 {
		Config.PriceAggregator.OutlierThreshold = 3.0
	}
	if Config.PriceAggregator.MinProviders == 0 {
		Config.PriceAggregator.MinProviders = 2
	}
	if Config.PriceAggregator.StaleThreshold == 0 {
		Config.PriceAggregator.StaleThreshold = 5000
	}

	if Config.Observability.Logging.Level == "" {
		Config.Observability.Logging.Level = "info"
	}
	if Config.Observability.Logging.Format == "" {
		Config.Observability.Logging.Format = "json"
	}
	if Config.Observability.Metrics.Port == 0 {
		Config.Observability.Metrics.Port = 9090
	}
	if Config.Observability.Metrics.Path == "" {
		Config.Observability.Metrics.Path = "/metrics"
	}
	if Config.Observability.Tracing.ServiceName == "" {
		Config.Observability.Tracing.ServiceName = "crypto-exchange"
	}
	if Config.Observability.Tracing.SampleRate == 0 {
		Config.Observability.Tracing.SampleRate = 0.1
	}

	if Config.RateLimits.REST.RequestsPerSecond == 0 {
		Config.RateLimits.REST.RequestsPerSecond = 100
	}
	if Config.RateLimits.REST.Burst == 0 {
		Config.RateLimits.REST.Burst = 200
	}
	if Config.RateLimits.WS.MessagesPerSecond == 0 {
		Config.RateLimits.WS.MessagesPerSecond = 50
	}
	if Config.RateLimits.WS.Burst == 0 {
		Config.RateLimits.WS.Burst = 100
	}
	if Config.RateLimits.Trading.OrdersPerSecond == 0 {
		Config.RateLimits.Trading.OrdersPerSecond = 10
	}
	if Config.RateLimits.Trading.Burst == 0 {
		Config.RateLimits.Trading.Burst = 20
	}
	if Config.RateLimits.Auth.LoginAttemptsPerMinute == 0 {
		Config.RateLimits.Auth.LoginAttemptsPerMinute = 5
	}
	if Config.RateLimits.Auth.OTPRequestsPerHour == 0 {
		Config.RateLimits.Auth.OTPRequestsPerHour = 10
	}
	// Sign-in limits protect against guessing, so they fail closed; the
	// others keep the API available while Redis is unreachable.
	for _, onError := range []*string{&Config.RateLimits.REST.OnError, &Config.RateLimits.WS.OnError, &Config.RateLimits.Trading.OnError} {
		if *onError == "" {
			*onError = RateLimitAllow
		}
	}
	if Config.RateLimits.Auth.OnError == "" {
		Config.RateLimits.Auth.OnError = RateLimitDeny
	}
}

// DefaultOTPEmailSubject and DefaultOTPEmailBody are used when the
// configuration does not define the sign-in code email.
const (
	DefaultOTPEmailSubject = "Your sign-in code"
	DefaultOTPEmailBody    = "Your sign-in code is {{.Code}}. It expires in {{.TTLSeconds}} seconds.\n\n" +
		"If you did not request this code, you can ignore this email. Never share it with anyone.\n"
	DefaultKavenegarBaseURL = "https://api.kavenegar.com"
)

func setNotificationDefaults(n *NotificationsConfig) {
	if n.Email.Port == 0 {
		n.Email.Port = 587
	}
	if n.Email.Timeout == 0 {
		n.Email.Timeout = 10
	}
	if n.Email.OTPSubject == "" {
		n.Email.OTPSubject = DefaultOTPEmailSubject
	}
	if n.Email.OTPBody == "" {
		n.Email.OTPBody = DefaultOTPEmailBody
	}
	if n.SMS.Kavenegar.BaseURL == "" {
		n.SMS.Kavenegar.BaseURL = DefaultKavenegarBaseURL
	}
	if n.SMS.Kavenegar.Timeout == 0 {
		n.SMS.Kavenegar.Timeout = 10
	}
	if n.Worker.Concurrency == 0 {
		n.Worker.Concurrency = 4
	}
	if n.Worker.MaxAttempts == 0 {
		n.Worker.MaxAttempts = 3
	}
}

func validateConfig() {
	if Config.Postgres.Password == "" {
		log.Println("WARNING: POSTGRES_PASSWORD is not set")
	}
	if Config.RabbitMQ.Username == "" || Config.RabbitMQ.Password == "" {
		log.Println("WARNING: RABBITMQ credentials are not set")
	}
	if Config.Exchange.Name == "" {
		log.Println("WARNING: Exchange name not set")
	}
	if !Config.PriceAggregator.Enabled {
		log.Println("WARNING: Price aggregator is disabled")
	}
	if len(Config.PriceAggregator.Providers) == 0 {
		log.Println("WARNING: No price providers configured")
	}

	for _, p := range Config.PriceAggregator.Providers {
		if p.Enabled && (p.WSEndpoint == "" && p.RESTEndpoint == "") {
			log.Printf("WARNING: Price provider %s enabled but no endpoint configured", p.Name)
		}
	}
	for _, p := range Config.Custody.Providers {
		if p.Enabled && len(p.RPCEndpoints) == 0 {
			log.Printf("WARNING: Custody provider %s enabled but no RPC endpoints", p.ProviderType)
		}
	}
	for _, p := range Config.Fiat.Providers {
		if p.Enabled && p.Endpoint == "" {
			log.Printf("WARNING: Fiat provider %s enabled but no endpoint", p.Name)
		}
	}
	for _, p := range Config.Compliance.Sanctions.Providers {
		if p.Enabled && p.Endpoint == "" {
			log.Printf("WARNING: Sanctions provider %s enabled but no endpoint", p.Name)
		}
	}
	for _, p := range Config.Compliance.KYC.Providers {
		if p.Enabled && p.Endpoint == "" {
			log.Printf("WARNING: KYC provider %s enabled but no endpoint", p.Name)
		}
	}
}

func (c *Configuration) JWTPrivateKeyPath() string {
	return string(c.JWT.SSL.User.PrivateKey)
}

func (c *Configuration) JWTPublicKeyPath() string {
	return string(c.JWT.SSL.User.PublicKey)
}

func GetRabbitMQURL() string {
	return fmt.Sprintf(
		"amqp://%s:%s@%s:%s/",
		Config.RabbitMQ.Username,
		Config.RabbitMQ.Password,
		Config.RabbitMQ.Host,
		Config.RabbitMQ.Port,
	)
}

// The defaults of the websocket settings. Clients send small control
// messages, such as pings and subscriptions, so a message is at most 4 KiB.
const (
	DefaultWSMaxMessageSize          = 4 << 10
	MaxWSMaxMessageSize              = 1 << 20
	DefaultWSWriteTimeoutSeconds     = 10
	DefaultWSClientMessagesPerSecond = 10
	DefaultWSClientMessageBurst      = 20
	DefaultWSMaxConnections          = 10000
	DefaultWSMaxConnectionsPerIP     = 20
)

// DefaultCurrencyCacheTTLSeconds is redis.currency_cache_ttl_seconds when
// it is not set.
const DefaultCurrencyCacheTTLSeconds = 300

// DefaultShutdownTimeoutSeconds is server.shutdown_timeout_seconds when it
// is not set.
const DefaultShutdownTimeoutSeconds = 30

// DefaultMaxBodyBytes is server.max_body_bytes when it is not set: 1 MiB.
const DefaultMaxBodyBytes = 1 << 20

// The defaults of the Redis client and of rate_limits.timeout_ms. They are
// short, so that a Redis outage delays requests by a moment instead of the
// seconds that the client library waits by default.
const (
	DefaultRedisDialTimeoutMs  = 1000
	DefaultRedisReadTimeoutMs  = 1000
	DefaultRedisWriteTimeoutMs = 1000
	DefaultRedisPoolTimeoutMs  = 2000
	DefaultRedisMaxRetries     = 1
	DefaultRateLimitTimeoutMs  = 250
)

// Milliseconds returns ms milliseconds, or fallback milliseconds when ms is
// not set, as in a configuration that Load did not fill in.
func Milliseconds(ms, fallback int) time.Duration {
	if ms <= 0 {
		ms = fallback
	}
	return time.Duration(ms) * time.Millisecond
}

// SwaggerEnabled reports whether the Swagger UI is served: as
// server.swagger.enabled says, or, when it is not set, outside release mode.
func (c *Configuration) SwaggerEnabled() bool {
	if c.Server.Swagger.Enabled != nil {
		return *c.Server.Swagger.Enabled
	}
	return c.Server.Mode != "release"
}

func GetServerAddr() string {
	return fmt.Sprintf("%s:%s", Config.Server.IP, Config.Server.Port)
}

func GetServerTimeouts() (readTimeout, writeTimeout, idleTimeout time.Duration) {
	return time.Duration(Config.Server.ReadTimeout) * time.Second,
		time.Duration(Config.Server.WriteTimeout) * time.Second,
		time.Duration(Config.Server.IdleTimeout) * time.Second
}
