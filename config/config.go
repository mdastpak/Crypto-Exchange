package config

import (
	"embed"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v2"
)

//go:embed config.example.yaml
var configEmbed embed.FS

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
		} `yaml:"tls"`
		CORS struct {
			// AllowedOrigins lists the browser origins (scheme://host[:port])
			// that may call the API with credentials. Empty means none.
			AllowedOrigins []string `yaml:"allowed_origins"`
		} `yaml:"cors"`
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

	WebSocket struct {
		AllowedOrigins []string `yaml:"allowed_origins"`
		PingInterval   int      `yaml:"ping_interval_seconds"`
		MaxMessageSize int      `yaml:"max_message_size_bytes"`
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

	RateLimits struct {
		REST struct {
			RequestsPerSecond int `yaml:"requests_per_second"`
			Burst             int `yaml:"burst"`
		} `yaml:"rest"`
		WS struct {
			MessagesPerSecond int `yaml:"messages_per_second"`
			Burst             int `yaml:"burst"`
		} `yaml:"ws"`
		Trading struct {
			OrdersPerSecond int `yaml:"orders_per_second"`
			Burst           int `yaml:"burst"`
		} `yaml:"trading"`
		Auth struct {
			LoginAttemptsPerMinute int `yaml:"login_attempts_per_minute"`
			OTPRequestsPerHour     int `yaml:"otp_requests_per_hour"`
		} `yaml:"auth"`
	} `yaml:"rate_limits"`
}

// JWTKeyPaths locates the PEM files of one role. Each path is a list of
// segments that are joined with the platform's separator.
type JWTKeyPaths struct {
	PrivateKey []string `yaml:"private_key"`
	PublicKey  []string `yaml:"public_key"`
	// RetiredPublicKeys lists the public keys of earlier key pairs. Tokens
	// they signed are accepted until they expire.
	RetiredPublicKeys [][]string `yaml:"retired_public_keys"`
}

// DefaultJWTIssuer is the "iss" claim when jwt.issuer is not set.
const DefaultJWTIssuer = "encrypted-db-exchange"

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

func LoadConfig() {
	var data []byte
	var err error

	if _, statErr := os.Stat(configPath); statErr == nil {
		data, err = os.ReadFile(configPath)
		if err != nil {
			log.Fatalf("Error reading config file: %v", err)
		}
	} else {
		data, err = configEmbed.ReadFile("config.example.yaml")
		if err != nil {
			log.Fatalf("Error reading embedded config: %v", err)
		}
	}

	err = yaml.Unmarshal(data, &Config)
	if err != nil {
		log.Fatalf("Error parsing config file: %v", err)
	}

	setDefaults()
	overlayEnvVars()
	validateConfig()
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

	setNotificationDefaults(&Config.Notifications)

	if Config.WebSocket.PingInterval == 0 {
		Config.WebSocket.PingInterval = 30
	}
	if Config.WebSocket.MaxMessageSize == 0 {
		Config.WebSocket.MaxMessageSize = 1024 * 1024
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
		Config.Observability.Tracing.ServiceName = "encrypted-db-exchange"
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

func overlayEnvVars() {
	if v := os.Getenv("POSTGRES_PASSWORD"); v != "" {
		Config.Postgres.Password = v
	}
	if v := os.Getenv("POSTGRES_HOST"); v != "" {
		Config.Postgres.Host = v
	}
	if v := os.Getenv("POSTGRES_PORT"); v != "" {
		Config.Postgres.Port = v
	}
	if v := os.Getenv("POSTGRES_USER"); v != "" {
		Config.Postgres.User = v
	}
	if v := os.Getenv("POSTGRES_DB"); v != "" {
		Config.Postgres.DBName = v
	}

	if v := os.Getenv("REDIS_PASSWORD"); v != "" {
		Config.Redis.Password = v
	}
	if v := os.Getenv("REDIS_HOST"); v != "" {
		Config.Redis.Host = v
	}
	if v := os.Getenv("REDIS_PORT"); v != "" {
		Config.Redis.Port = v
	}

	if v := os.Getenv("RABBITMQ_USER"); v != "" {
		Config.RabbitMQ.Username = v
	}
	if v := os.Getenv("RABBITMQ_PASSWORD"); v != "" {
		Config.RabbitMQ.Password = v
	}
	if v := os.Getenv("RABBITMQ_HOST"); v != "" {
		Config.RabbitMQ.Host = v
	}
	if v := os.Getenv("RABBITMQ_PORT"); v != "" {
		Config.RabbitMQ.Port = v
	}

	if v := os.Getenv("JWT_ISSUER"); v != "" {
		Config.JWT.Issuer = v
	}
	if v := os.Getenv("OTP_SECRET"); v != "" {
		Config.OTP.Auth.Secret = v
	}

	if v := os.Getenv("SMTP_PASSWORD"); v != "" {
		Config.Notifications.Email.Password = v
	}
	if v := os.Getenv("KAVENEGAR_API_KEY"); v != "" {
		Config.Notifications.SMS.Kavenegar.APIKey = v
	}

	if v := os.Getenv("BINANCE_API_KEY"); v != "" {
		for i := range Config.PriceAggregator.Providers {
			if Config.PriceAggregator.Providers[i].Name == "binance" {
				Config.PriceAggregator.Providers[i].APIKey = v
			}
		}
	}
	if v := os.Getenv("BINANCE_API_SECRET"); v != "" {
		for i := range Config.PriceAggregator.Providers {
			if Config.PriceAggregator.Providers[i].Name == "binance" {
				Config.PriceAggregator.Providers[i].APISecret = v
			}
		}
	}

	if v := os.Getenv("FIAT_PROVIDER_API_KEY"); v != "" {
		for i := range Config.Fiat.Providers {
			Config.Fiat.Providers[i].APIKey = v
		}
	}
	if v := os.Getenv("FIAT_PROVIDER_API_SECRET"); v != "" {
		for i := range Config.Fiat.Providers {
			Config.Fiat.Providers[i].APISecret = v
		}
	}

	if v := os.Getenv("CHAINALYSIS_API_KEY"); v != "" {
		for i := range Config.Compliance.Sanctions.Providers {
			if Config.Compliance.Sanctions.Providers[i].Name == "chainalysis" {
				Config.Compliance.Sanctions.Providers[i].APIKey = v
			}
		}
	}
	if v := os.Getenv("TRM_API_KEY"); v != "" {
		for i := range Config.Compliance.Sanctions.Providers {
			if Config.Compliance.Sanctions.Providers[i].Name == "trm" {
				Config.Compliance.Sanctions.Providers[i].APIKey = v
			}
		}
	}

	if v := os.Getenv("SUMSUB_API_KEY"); v != "" {
		for i := range Config.Compliance.KYC.Providers {
			if Config.Compliance.KYC.Providers[i].Name == "sumsub" {
				Config.Compliance.KYC.Providers[i].APIKey = v
			}
		}
	}
	if v := os.Getenv("ONFIDO_API_KEY"); v != "" {
		for i := range Config.Compliance.KYC.Providers {
			if Config.Compliance.KYC.Providers[i].Name == "onfido" {
				Config.Compliance.KYC.Providers[i].APIKey = v
			}
		}
	}

	if v := os.Getenv("OTEL_ENDPOINT"); v != "" {
		Config.Observability.Tracing.Endpoint = v
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		Config.Observability.Logging.Level = v
	}
}

func validateConfig() {
	if Config.Postgres.Password == "" {
		log.Println("WARNING: POSTGRES_PASSWORD is not set")
	}
	if Config.RabbitMQ.Username == "" || Config.RabbitMQ.Password == "" {
		log.Println("WARNING: RABBITMQ credentials are not set")
	}
	if len(Config.JWT.SSL.User.PrivateKey) == 0 || len(Config.JWT.SSL.User.PublicKey) == 0 {
		log.Println("WARNING: JWT SSL keys not configured")
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
	return filepath.Join(c.JWT.SSL.User.PrivateKey...)
}

func (c *Configuration) JWTPublicKeyPath() string {
	return filepath.Join(c.JWT.SSL.User.PublicKey...)
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

func GetServerAddr() string {
	return fmt.Sprintf("%s:%s", Config.Server.IP, Config.Server.Port)
}

func GetServerTimeouts() (readTimeout, writeTimeout, idleTimeout time.Duration) {
	return time.Duration(Config.Server.ReadTimeout) * time.Second,
		time.Duration(Config.Server.WriteTimeout) * time.Second,
		time.Duration(Config.Server.IdleTimeout) * time.Second
}
