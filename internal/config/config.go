package config

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	configfiles "github.com/mdastpak/crypto-exchange/config"
	"github.com/mdastpak/crypto-exchange/internal/clientip"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/region"
	"github.com/mdastpak/crypto-exchange/internal/sites"

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
	// Deployment describes the release.
	Deployment struct {
		// Region is the market that the release serves (#272): ir (the
		// default) for users in Iran, or international for users outside
		// it, where the Iran-only features must be off. See Region.
		Region string `yaml:"region"`
	} `yaml:"deployment"`
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
		// HSTS is the Strict-Transport-Security header (#191).
		HSTS struct {
			// Enabled sends it. Unset, it is sent when the server or a
			// proxy in front of it terminates TLS.
			Enabled *bool `yaml:"enabled"`
			// MaxAgeSeconds is how long browsers remember to use HTTPS.
			MaxAgeSeconds int `yaml:"max_age_seconds"`
			// IncludeSubdomains applies it to every subdomain too.
			IncludeSubdomains bool `yaml:"include_subdomains"`
		} `yaml:"hsts"`
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
		// Hosts lists the nodes of a high-availability cluster as
		// host:port, in place of host and port (#230). The driver tries
		// them in order and takes the first that TargetSessionAttrs
		// accepts, so after a failover new connections find the new
		// primary.
		Hosts []string `yaml:"hosts"`
		// TargetSessionAttrs is the libpq target_session_attrs: which
		// node a connection accepts. read-write, the default, refuses a
		// node that only reads, such as a demoted primary.
		TargetSessionAttrs string `yaml:"target_session_attrs"`
		// The server-side timeouts of every connection of the server
		// (#230), so that a slow query or a lock wait cannot hold a pool
		// connection for long. 0 keeps the default; -1 turns one off.
		StatementTimeoutMs         int `yaml:"statement_timeout_ms"`
		LockTimeoutMs              int `yaml:"lock_timeout_ms"`
		IdleInTransactionTimeoutMs int `yaml:"idle_in_transaction_timeout_ms"`
		// MigrationLockTimeoutSeconds bounds the wait for the migration
		// lock at startup; an instance that cannot get it stops, naming
		// the session that holds it.
		MigrationLockTimeoutSeconds int `yaml:"migration_lock_timeout_seconds"`
		// Migrations separates the role that changes the schema from the
		// role that the server runs with (#180).
		Migrations PostgresMigrations `yaml:"migrations"`
	} `yaml:"postgres"`

	Redis struct {
		// Mode is how the server reaches Redis (#231): "standalone" (the
		// default) connects to host and port; "sentinel" asks the
		// sentinels for the current primary and follows its failover;
		// "cluster" connects to a Redis Cluster. Every mode uses the
		// username, password, timeouts, pool, and TLS settings below.
		Mode string `yaml:"mode"`
		// Host and Port are the one node of the standalone mode.
		Host string `yaml:"host"`
		Port string `yaml:"port"`
		// Sentinel names the primary and lists the sentinels, as host:port,
		// of the sentinel mode. Username and Password are the sentinels' own
		// credentials, when they require any; the data nodes use
		// redis.username and redis.password.
		Sentinel struct {
			MasterName string   `yaml:"master_name"`
			Addresses  []string `yaml:"addresses"`
			Username   string   `yaml:"username"`
			Password   string   `yaml:"password"`
		} `yaml:"sentinel"`
		// Cluster lists some nodes of the cluster mode, as host:port; the
		// client learns the others from them.
		Cluster struct {
			Addresses []string `yaml:"addresses"`
		} `yaml:"cluster"`
		// DurableWrites bounds what a failover can lose of the security
		// counters (#231): the OTP attempts and code requests and the
		// blacklist of logged-out tokens. With Replicas above 0, each such
		// write waits, with WAIT, until that many replicas hold it, for at
		// most TimeoutMs; a write that fewer replicas acknowledge fails the
		// request, so that a promoted replica can never hand back attempts
		// that were already made. 0 does not wait.
		DurableWrites struct {
			Replicas  int `yaml:"replicas"`
			TimeoutMs int `yaml:"timeout_ms"`
		} `yaml:"durable_writes"`
		// Username selects a Redis ACL user. Empty is the default user.
		Username            string `yaml:"username"`
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
		CurrencyCacheTTL int       `yaml:"currency_cache_ttl_seconds"`
		TLS              ClientTLS `yaml:"tls"`
		// AssumeNoEviction says that Redis runs with maxmemory-policy
		// noeviction where the server cannot read the policy, as on a
		// managed Redis that refuses CONFIG GET (#305). Release mode
		// otherwise refuses to start without reading noeviction: under any
		// other policy, Redis may drop the blacklist of logged-out tokens,
		// the attempt counters of sign-in codes, and the rate limits, and
		// their checks then pass without an error.
		AssumeNoEviction bool `yaml:"assume_noeviction"`
	} `yaml:"redis"`

	RabbitMQ struct {
		Username string `yaml:"username"`
		Password string `yaml:"password"`
		Host     string `yaml:"host"`
		Port     string `yaml:"port"`
		// Hosts lists the nodes of a RabbitMQ cluster, as host:port, in
		// place of Host and Port (#232). The service dials them in random
		// order, on startup and after every lost connection, so that it
		// reaches a healthy node while one is down.
		Hosts []string `yaml:"hosts"`
		// VHost is the virtual host to connect to. Empty is the broker's
		// default virtual host, "/".
		VHost     string    `yaml:"vhost"`
		TLS       ClientTLS `yaml:"tls"`
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
			// ReuseGraceSeconds is how long the previous refresh token of
			// a session still works after a rotation, for two tabs that
			// refresh at once (#194). Later, it revokes the session.
			ReuseGraceSeconds int `yaml:"reuse_grace_seconds"`
		} `yaml:"refresh_token"`
		Sessions struct {
			// MaxPerUser caps the active sessions of one user (#212). A
			// sign-in past it is refused until the user ends one of them.
			MaxPerUser int `yaml:"max_per_user"`
			// MaxLifetimeMinutes is the absolute lifetime of a session
			// (#144): no refresh extends a session past its creation plus
			// this, and its tokens stop working then, so a refresh token
			// that is kept rotating, as a stolen one may be, still ends.
			// It must be at least jwt.refresh_token.expiration_minutes.
			MaxLifetimeMinutes int `yaml:"max_lifetime_minutes"`
		} `yaml:"sessions"`
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
			// ContactRequestsPerHour bounds the codes issued to one contact
			// for one client.
			ContactRequestsPerHour int `yaml:"contact_requests_per_hour"`
			// ContactAttemptsPerHour bounds the verification attempts for
			// one contact across all of its codes, from one client.
			ContactAttemptsPerHour int `yaml:"contact_attempts_per_hour"`
			// ContactRequestsTotalPerHour bounds the codes issued to one
			// contact for all clients together, except the clients that
			// signed in with it before (#187).
			ContactRequestsTotalPerHour int `yaml:"contact_requests_total_per_hour"`
		} `yaml:"auth"`
	} `yaml:"otp"`

	Notifications NotificationsConfig `yaml:"notifications"`

	Admin struct {
		// SignInMinResponseMS is the least time an answer to an admin
		// sign-in request takes, so that its duration does not tell an
		// administrator's address from an unknown one (#190).
		SignInMinResponseMS int `yaml:"sign_in_min_response_ms"`
		Sessions            struct {
			// MaxPerAdmin caps the active sessions of one administrator
			// (#308). A sign-in past it ends the oldest ones.
			MaxPerAdmin int `yaml:"max_per_admin"`
		} `yaml:"sessions"`
	} `yaml:"admin"`

	// Contacts configures the email and mobile changes of users (#199).
	Contacts struct {
		// NewContactCooldownHours is how long a newly verified contact
		// cannot prove a replacement or removal of another contact of the
		// account (#250), so that a contact added with a stolen session
		// cannot take over the account at once.
		NewContactCooldownHours int `yaml:"new_contact_cooldown_hours"`
	} `yaml:"contacts"`

	// Security configures the protections that follow a change of the
	// security settings of a user.
	Security struct {
		// ChangeHoldHours is how long the operations that a second factor
		// protects, such as withdrawals, are held after a change of the
		// email address, the mobile number, or the second factor of a user
		// (#251).
		ChangeHoldHours int `yaml:"change_hold_hours"`
	} `yaml:"security"`

	// Identity configures the checks of a user's identity with a KYC
	// provider.
	Identity struct {
		// MobileOwnership checks that an Iranian mobile number belongs to
		// the national ID of its user (#253), as the Shahkar service of the
		// Communications Regulatory Authority answers it.
		MobileOwnership MobileOwnershipConfig `yaml:"mobile_ownership"`
	} `yaml:"identity"`

	// I18n lists the locales that the product supports and its fallback
	// (#283). The direction (RTL or LTR) and the names of each locale come
	// from its language.
	I18n struct {
		// DefaultLocale answers a request that names no supported locale.
		DefaultLocale string `yaml:"default_locale"`
		// Locales are BCP 47 tags, in the order a language switcher lists
		// them. "en" is required: it is the language of every message that
		// has no translation.
		Locales []string `yaml:"locales"`
	} `yaml:"i18n"`

	// GeoIP finds the country of a client (#284), for the site of their
	// country (#285). Off by default: no request has a country.
	GeoIP struct {
		Enabled bool `yaml:"enabled"`
		// Header names the request header in which a CDN or load balancer
		// in front of the server sends the country, such as CF-IPCountry.
		// It is trusted only from server.trusted_proxies.
		Header string `yaml:"header"`
		// DatabasePath is a MaxMind-format country or city database
		// (.mmdb), such as GeoLite2-Country or DB-IP Lite Country.
		DatabasePath string `yaml:"database_path"`
		// ReloadIntervalSeconds is how often the file is checked for a new
		// version, which is then loaded without a restart.
		ReloadIntervalSeconds int `yaml:"reload_interval_seconds"`
		// MaxAgeDays is the age of the database after which the readiness
		// check reports it as stale.
		MaxAgeDays int `yaml:"max_age_days"`
		// Overrides map addresses or networks to countries, to try the
		// flow locally. Release mode refuses them.
		Overrides map[string]string `yaml:"overrides"`
	} `yaml:"geoip"`

	// Sites are the domains of the product, the countries each serves, and
	// its locales (#285). Empty means one site and no redirects.
	Sites struct {
		// Default names the site of a visitor whose country no site serves,
		// or is not known.
		Default string `yaml:"default"`
		// Scheme of the sites' URLs: https, or http for development.
		Scheme string       `yaml:"scheme"`
		List   []SiteConfig `yaml:"list"`
	} `yaml:"sites"`

	Favorites struct {
		// MaxPerUser caps the favorite currencies and pairs of one user
		// (#217), together.
		MaxPerUser int `yaml:"max_per_user"`
	} `yaml:"favorites"`

	Delisting struct {
		// IntervalSeconds is how often the scheduler of each instance
		// applies the steps of delistings that are due (#216).
		IntervalSeconds int `yaml:"interval_seconds"`
	} `yaml:"delisting"`

	// MarketData configures cmd/market-worker, which collects the global
	// market data catalog (#262).
	MarketData MarketDataConfig `yaml:"market_data"`

	// Platform configures the maintenance and lockdown controls (#235).
	Platform struct {
		// PollIntervalMs is how often each instance reads the version of
		// the controls, so that a change applies on every instance within
		// it.
		PollIntervalMs int `yaml:"poll_interval_ms"`
		// StaleAfterSeconds is how long an instance applies the last
		// controls it read, when it cannot read them, before it closes the
		// features of FailClosed too.
		StaleAfterSeconds int `yaml:"stale_after_seconds"`
		// FailClosed are the features that an instance closes while it
		// cannot read the controls. Unset is deposits, withdrawals, and
		// trading; ["none"] is none.
		FailClosed []string `yaml:"fail_closed"`
		// LiftApproval are the features whose active lockdown a second
		// administrator must approve to lift. Unset is withdrawals and
		// trading; ["none"] is none.
		LiftApproval []string `yaml:"lift_approval"`
	} `yaml:"platform"`

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
		// MaxConnections caps the open connections of this instance.
		// MaxConnectionsPerIP and MaxConnectionsPerUser cap those of one
		// client address and of one user over all instances, counted in
		// Redis (#234).
		MaxConnections        int `yaml:"max_connections"`
		MaxConnectionsPerIP   int `yaml:"max_connections_per_ip"`
		MaxConnectionsPerUser int `yaml:"max_connections_per_user"`
		// SessionCheckSeconds is how often each instance checks the
		// sessions of its authenticated connections, and closes those whose
		// session has ended or whose user can no longer sign in (#189).
		SessionCheckSeconds int `yaml:"session_check_seconds"`
	} `yaml:"websocket"`

	Exchange struct {
		Name     string `yaml:"name"`
		Timezone string `yaml:"timezone"`
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
		// IPv6PrefixLength groups IPv6 clients by this prefix for every
		// per-client limit and connection count (#188), since one
		// subscriber holds at least a /64. 0 is the default of
		// clientip.DefaultIPv6PrefixLength.
		IPv6PrefixLength int `yaml:"ipv6_prefix_length"`
		REST             struct {
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
	// NextPublicKeys lists the public keys of the key pair that will sign
	// next (#233): every instance accepts its tokens before any instance
	// signs with it, which is the first phase of a rotation.
	NextPublicKeys []KeyPath `yaml:"next_public_keys"`
}

// KeyPath is the path of a key file.
type KeyPath string

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
	// ReloadIntervalSeconds is how often each instance checks data_keys
	// and loads a rotation, a retirement, or a re-wrap (#233).
	ReloadIntervalSeconds int `yaml:"reload_interval_seconds"`
	// ActivationDelaySeconds is how long after keys rotate the new
	// version starts writing. Every instance reads with it from its next
	// reload, so it must be at least twice the reload interval.
	ActivationDelaySeconds int `yaml:"activation_delay_seconds"`
	// RowSeals configures the row seals (#139), which need a provider.
	RowSeals RowSealsConfig `yaml:"row_seals"`
}

// RowSealsConfig configures the row seals (#139).
type RowSealsConfig struct {
	// Mode is what a row that does not match its seal does: "report", the
	// default, records it and still serves it, and the verifier seals it
	// again; "enforce" records it and refuses it until an administrator
	// re-seals it.
	Mode string `yaml:"mode"`
	// VerifyIntervalMinutes is how often the background verifier walks the
	// protected tables.
	VerifyIntervalMinutes int `yaml:"verify_interval_minutes"`
}

// The modes of keys.row_seals.mode, as internal/rowseal names them.
const (
	RowSealsReport  = "report"
	RowSealsEnforce = "enforce"
)

// Defaults and bounds of keys.row_seals.verify_interval_minutes.
const (
	DefaultRowSealsVerifyIntervalMinutes = 60
	MaxRowSealsVerifyIntervalMinutes     = 7 * 24 * 60
)

// Defaults and bounds of the reloading of the data keys (#233).
const (
	DefaultKeysReloadIntervalSeconds  = 60
	MinKeysReloadIntervalSeconds      = 5
	MaxKeysReloadIntervalSeconds      = 3600
	DefaultKeysActivationDelaySeconds = 300
)

// Key providers, as keys.provider spells them.
const (
	KeysProviderTransit = "transit"
	KeysProviderLocal   = "local"
)

// Defaults of the PostgreSQL availability settings (#230).
const (
	DefaultPostgresTargetSessionAttrs          = "read-write"
	DefaultPostgresStatementTimeoutMs          = 30000
	DefaultPostgresLockTimeoutMs               = 5000
	DefaultPostgresIdleInTransactionTimeoutMs  = 60000
	DefaultPostgresMigrationLockTimeoutSeconds = 300
)

// PostgresMigrations configures who migrates the schema, and when (#180).
//
// The server runs with postgres.user, the runtime role, which reads and
// writes rows but owns nothing and cannot change the schema. The
// migrations run with User, the migration role, which owns the schema.
// After every migration, the migration role grants the runtime role what
// it needs: SELECT, INSERT, UPDATE, and DELETE on the tables, only SELECT
// and INSERT on the append-only ones, and USAGE on the sequences.
type PostgresMigrations struct {
	// Mode is when the migrations run. "startup", the default, migrates
	// at the start of every instance, under an advisory lock; that suits
	// development and a single instance. "external" never migrates in the
	// server: a release step runs "admin migrate" before the new instances
	// start, and an instance refuses to start while the database is behind
	// its migrations. Release mode needs "external".
	Mode string `yaml:"mode"`
	// User and Password are the migration role. Empty, the migrations use
	// postgres.user and postgres.password, as one role that owns
	// everything, which release mode refuses.
	User     string `yaml:"user"`
	Password string `yaml:"password"`
}

// The values of postgres.migrations.mode.
const (
	MigrationsModeStartup  = "startup"
	MigrationsModeExternal = "external"
)

// MigrationsModes lists the values of postgres.migrations.mode.
var MigrationsModes = []string{MigrationsModeStartup, MigrationsModeExternal}

// PostgresTargetSessionAttrs are the values of postgres.target_session_attrs.
var PostgresTargetSessionAttrs = []string{"any", "read-write", "read-only", "primary", "standby", "prefer-standby"}

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

// Providers of identity.mobile_ownership.provider.
const (
	MobileOwnershipNone  = "none"
	MobileOwnershipStub  = "stub"
	MobileOwnershipJibit = "jibit"
)

// MobileOwnershipConfig configures the provider of the mobile ownership
// check (#253).
type MobileOwnershipConfig struct {
	// Provider is none, stub, or jibit. With none, a user who has a
	// verified national ID cannot add or replace a mobile number, since
	// it cannot be checked. stub matches every number, for development,
	// and is refused in release mode.
	Provider string `yaml:"provider"`
	// Endpoint is the base URL of the provider's API.
	Endpoint string `yaml:"endpoint"`
	// APIKey and SecretKey are the provider's credentials. Set them in
	// the environment.
	APIKey    string `yaml:"api_key"`
	SecretKey string `yaml:"secret_key"`
	// TimeoutSeconds bounds one check, with its retries.
	TimeoutSeconds int `yaml:"timeout_seconds"`
	// RecheckDays is how long a successful check of a number counts
	// before a sign-in with it checks it again (#252).
	RecheckDays int `yaml:"recheck_days"`
}

// Timeout is TimeoutSeconds, or ten seconds when it is not set.
func (m MobileOwnershipConfig) Timeout() time.Duration {
	if m.TimeoutSeconds <= 0 {
		return 10 * time.Second
	}
	return time.Duration(m.TimeoutSeconds) * time.Second
}

// Recheck is RecheckDays, or thirty days when it is not set.
func (m MobileOwnershipConfig) Recheck() time.Duration {
	if m.RecheckDays <= 0 {
		return 30 * 24 * time.Hour
	}
	return time.Duration(m.RecheckDays) * 24 * time.Hour
}

// MarketDataConfig configures cmd/market-worker (#262), which collects
// asset metadata and the markets of exchanges into the unverified catalog.
// The server does not read it.
type MarketDataConfig struct {
	// Enabled runs the jobs of the worker. A worker that is not enabled
	// only answers its health probes, so that it can be deployed first and
	// switched on later.
	Enabled bool `yaml:"enabled"`
	// Mode is "dry_run", where the jobs collect and check what they would
	// write and record only their runs, or "live", where they write.
	Mode string `yaml:"mode"`
	// HealthAddr is the address of the worker's health probes,
	// /system/live and /system/ready.
	HealthAddr string `yaml:"health_addr"`
	// PollSeconds is how often each worker looks for due jobs.
	PollSeconds int `yaml:"poll_seconds"`
	// LeaseSeconds is how long a worker holds a job without renewing it:
	// how soon another worker takes over the job of a worker that died.
	LeaseSeconds int `yaml:"lease_seconds"`
	// MaxConcurrentJobs bounds the runs of one worker at once.
	MaxConcurrentJobs int `yaml:"max_concurrent_jobs"`
	// RetryBaseSeconds is the wait after a failed run; it doubles with
	// each further failure in a row, up to the interval of the job.
	RetryBaseSeconds int `yaml:"retry_base_seconds"`
	// Egress says how this node reaches the sources (#265): directly, or
	// through a proxy or a VPN gateway. Each node sets its own.
	Egress MarketDataEgress `yaml:"egress"`
	// Sources configures each source by name, such as coingecko or
	// nobitex. A source that is not listed, or not enabled, is not used by
	// this node.
	Sources map[string]MarketDataSource `yaml:"sources"`
	// Catalog configures the catalog sync (#266).
	Catalog MarketDataCatalog `yaml:"catalog"`
	// Tickers configures the tickers of the venues (#267).
	Tickers MarketDataTickers `yaml:"tickers"`
	// Candles configures the candles of the venues (#268).
	Candles MarketDataCandles `yaml:"candles"`
	// Tiers configures the collection tiers of the pairs (#279).
	Tiers MarketDataTiers `yaml:"tiers"`
	// Streams configures the live tickers of the venues' WebSocket
	// streams (#281).
	Streams MarketDataStreams `yaml:"streams"`
	// API configures the public read API of the server (#269), which
	// also applies Tickers' rules of quality.
	API MarketDataAPI `yaml:"api"`
}

// MarketDataAPI configures the public routes of the market data catalog,
// which the server serves.
type MarketDataAPI struct {
	// Regions are the regions of the venues that the API shows: global,
	// ir, or both when empty. A release for users outside Iran sets
	// [global] (#272).
	Regions []string `yaml:"regions"`
	// CacheSeconds is how long an answer is cached in Redis, shared by
	// every instance, and by clients.
	CacheSeconds int `yaml:"cache_seconds"`
}

// The defaults and bounds of market_data.api.
const (
	DefaultMarketDataAPICacheSeconds = 30
	MaxMarketDataAPICacheSeconds     = 3600
)

// MarketDataCandles configures the candle jobs of the venues.
type MarketDataCandles struct {
	// Quotes are the quote symbols of the pairs whose candles are kept.
	Quotes []string `yaml:"quotes"`
	// MaxPairsPerVenue bounds the pairs of a venue, the best-ranked
	// bases first.
	MaxPairsPerVenue int `yaml:"max_pairs_per_venue"`
	// RequestsPerRun bounds the requests of one run of a venue and
	// resolution.
	RequestsPerRun int `yaml:"requests_per_run"`
	// Resolutions configures 1m, 5m, 15m, 1h, and 1d.
	Resolutions map[string]MarketDataResolution `yaml:"resolutions"`
}

// The collection tiers of the pairs (#279).
const (
	TierHot    = "hot"
	TierActive = "active"
	TierOther  = "other"
)

// Tiers lists the collection tiers, the most watched first.
var Tiers = []string{TierHot, TierActive, TierOther}

// MarketDataTiers configures how the pairs are put in tiers. A pair is hot
// when its base is one of HotAssets, or is listed on this exchange with
// HotListed, and its quote is one of the candle quotes; active when it is
// among the ActiveTop pairs by 24-hour volume in USD; other otherwise.
type MarketDataTiers struct {
	// IntervalHours is the time between two computations of the tiers.
	IntervalHours int `yaml:"interval_hours"`
	// HotAssets are the slugs of the catalog's assets whose pairs are hot,
	// such as bitcoin.
	HotAssets []string `yaml:"hot_assets"`
	// HotListed makes hot the pairs whose base is linked to a currency
	// listed on this exchange.
	HotListed *bool `yaml:"hot_listed"`
	// ActiveTop is the number of pairs, over every venue, that are active.
	ActiveTop int `yaml:"active_top"`
	// HysteresisPercent keeps an active pair active until its rank passes
	// ActiveTop by this share, so that a pair near the boundary does not
	// move every day.
	HysteresisPercent int `yaml:"hysteresis_percent"`
}

// The defaults and bounds of market_data.tiers.
const (
	DefaultTiersIntervalHours     = 24
	DefaultTiersActiveTop         = 200
	MaxTiersActiveTop             = 10000
	DefaultTiersHysteresisPercent = 20
)

// MarketDataStreams configures the WebSocket streams of the venues, which
// keep the tickers of the hot pairs seconds old (#281).
type MarketDataStreams struct {
	// Enabled runs the streams of the venues that have one and that this
	// node reaches.
	Enabled *bool `yaml:"enabled"`
	// Tiers are the tiers of the pairs that the streams follow.
	Tiers []string `yaml:"tiers"`
	// MaxPairs bounds the pairs of a venue's stream.
	MaxPairs int `yaml:"max_pairs"`
	// MaxPerWorker bounds the streams that one worker holds, apart from
	// max_concurrent_jobs, so that the workers share them.
	MaxPerWorker int `yaml:"max_per_worker"`
	// RunMinutes is how long a worker holds a stream before it lets the
	// workers take it again.
	RunMinutes int `yaml:"run_minutes"`
	// FlushMillis is how often the tickers received are written: a pair is
	// written at most once per this time.
	FlushMillis int `yaml:"flush_millis"`
	// QuietSeconds is how long a stream may stay quiet before it counts as
	// dead and reconnects.
	QuietSeconds int `yaml:"quiet_seconds"`
}

// The defaults and bounds of market_data.streams.
const (
	DefaultStreamsMaxPairs     = 200
	MaxStreamsMaxPairs         = 1000
	DefaultStreamsMaxPerWorker = 2
	DefaultStreamsRunMinutes   = 60
	DefaultStreamsFlushMillis  = 1000
	DefaultStreamsQuietSeconds = 60
)

// DefaultHotAssets are the hot assets when the configuration sets none.
func DefaultHotAssets() []string { return []string{"bitcoin", "ethereum", "tether"} }

// MarketDataResolution configures the candles of one resolution.
type MarketDataResolution struct {
	Enabled bool `yaml:"enabled"`
	// IntervalMinutes is the time between two runs of a venue.
	IntervalMinutes int `yaml:"interval_minutes"`
	// BackfillDays is how far back the history goes.
	BackfillDays int `yaml:"backfill_days"`
	// RetentionDays is how long candles are kept; 0 keeps them.
	RetentionDays int `yaml:"retention_days"`
	// Tiers are the tiers whose pairs are collected at this resolution.
	Tiers []string `yaml:"tiers"`
}

// The defaults and bounds of market_data.candles.
const (
	DefaultCandlesMaxPairsPerVenue = 200
	MaxCandlesMaxPairsPerVenue     = 10000
	DefaultCandlesRequestsPerRun   = 300
	MaxCandlesRequestsPerRun       = 10000
	MaxCandlesDays                 = 36500
)

// DefaultCandleResolutions are the resolutions when the configuration sets
// none: every pair has the full daily history and 180 days of hours; the
// hot and active pairs have 30 days of 5 minutes and 90 of 15 minutes; the
// hot pairs have 7 days of minutes (#279).
func DefaultCandleResolutions() map[string]MarketDataResolution {
	all := []string{TierHot, TierActive, TierOther}
	return map[string]MarketDataResolution{
		"1d":  {Enabled: true, IntervalMinutes: 360, BackfillDays: 3650, Tiers: all},
		"1h":  {Enabled: true, IntervalMinutes: 60, BackfillDays: 180, RetentionDays: 180, Tiers: all},
		"15m": {Enabled: true, IntervalMinutes: 15, BackfillDays: 90, RetentionDays: 90, Tiers: []string{TierHot, TierActive}},
		"5m":  {Enabled: true, IntervalMinutes: 5, BackfillDays: 30, RetentionDays: 30, Tiers: []string{TierHot, TierActive}},
		"1m":  {Enabled: true, IntervalMinutes: 1, BackfillDays: 1, RetentionDays: 7, Tiers: []string{TierHot}},
	}
}

// MarketDataTickers configures the ticker jobs of the venues, and the rules
// of their quality where they are read.
type MarketDataTickers struct {
	// IntervalSeconds is the time between two runs of a venue.
	IntervalSeconds int `yaml:"interval_seconds"`
	// StaleAfterSeconds: a ticker observed longer ago is stale.
	StaleAfterSeconds int `yaml:"stale_after_seconds"`
	// OutlierPercent: a price that differs from the median of the venues
	// by more than this share is an outlier.
	OutlierPercent int `yaml:"outlier_percent"`
	// MinVenues is the fewest venues with a fresh price of the same quote
	// that make a median.
	MinVenues int `yaml:"min_venues"`
	// MapRankLimit: a pair whose symbol several assets share maps to the
	// only one ranked within this limit, or stays unmapped.
	MapRankLimit int `yaml:"map_rank_limit"`
	// DeactivatePairsAfterHours marks inactive the pairs that a venue has
	// not listed for this long.
	DeactivatePairsAfterHours int `yaml:"deactivate_pairs_after_hours"`
}

// The defaults and bounds of market_data.tickers.
const (
	DefaultTickersIntervalSeconds    = 120
	MinTickersIntervalSeconds        = 30
	MaxTickersIntervalSeconds        = 3600
	DefaultTickersStaleAfterSeconds  = 600
	MaxTickersStaleAfterSeconds      = 86400
	DefaultTickersOutlierPercent     = 5
	DefaultTickersMinVenues          = 3
	MaxTickersMinVenues              = 20
	DefaultTickersMapRankLimit       = 1000
	DefaultTickersDeactivateAfterHrs = 24
)

// MarketDataCatalog configures the catalog sync of cmd/market-worker: which
// assets it keeps, and how often it reads them.
type MarketDataCatalog struct {
	// Priority lists the catalog sources, primary first. The primary
	// source writes the fields of the assets it knows; the others link to
	// its assets, and create only the assets it does not list.
	Priority []string `yaml:"priority"`
	// Coverage is "list" (a built-in set of important assets, for
	// development and tests), "top" (the first Top assets by market cap),
	// or "all" (every asset of the sources, for production).
	Coverage string `yaml:"coverage"`
	Top      int    `yaml:"top"`
	// IntervalHours is the time between two runs of a source.
	IntervalHours int `yaml:"interval_hours"`
	// DetailsPerRun bounds the full records (description, links,
	// contracts) read in one run, so that a large catalog fills over
	// several runs within the limits of the plan.
	DetailsPerRun int `yaml:"details_per_run"`
	// DetailsMaxAgeDays is when a full record is read again.
	DetailsMaxAgeDays int `yaml:"details_max_age_days"`
	// DeactivateAfterDays marks inactive the assets that no source listed
	// for this long; only the "all" coverage does it.
	DeactivateAfterDays int `yaml:"deactivate_after_days"`
}

// The coverages of market_data.catalog.coverage.
const (
	CatalogCoverageList = "list"
	CatalogCoverageTop  = "top"
	CatalogCoverageAll  = "all"
)

// CatalogSources are the sources that the catalog sync can read.
var CatalogSources = []string{"coingecko", "coinmarketcap"}

// The defaults and bounds of market_data.catalog.
const (
	DefaultCatalogTop                 = 500
	MaxCatalogTop                     = 50000
	DefaultCatalogIntervalHours       = 24
	MaxCatalogIntervalHours           = 24 * 7
	DefaultCatalogDetailsPerRun       = 200
	MaxCatalogDetailsPerRun           = 10000
	DefaultCatalogDetailsMaxAgeDays   = 30
	DefaultCatalogDeactivateAfterDays = 14
	MaxCatalogDays                    = 365
)

// MarketDataEgress is how a node of cmd/market-worker reaches the sources.
type MarketDataEgress struct {
	// Default is the route of the sources that set none: "direct" (the
	// node reaches the internet itself), "proxy" (through ProxyURL), or
	// "none" (this node calls no source unless the source says otherwise).
	Default string `yaml:"default"`
	// ProxyURL is the proxy of the "proxy" route: http://, https://, or
	// socks5://, with credentials if it needs them. Set it with
	// MARKET_DATA_PROXY_URL; it is never logged.
	ProxyURL string `yaml:"proxy_url"`
}

// MarketDataSource configures one source of cmd/market-worker.
type MarketDataSource struct {
	Enabled bool `yaml:"enabled"`
	// Egress overrides egress.default for this source: "direct",
	// "proxy", or "none". An Iranian exchange may be "direct" on a node in
	// Iran whose default is "proxy".
	Egress string `yaml:"egress"`
	// BaseURL overrides the API address of the source, for example the
	// address of a paid plan.
	BaseURL string `yaml:"base_url"`
	// APIKey is the key of the source's plan, if it has one. Set it in the
	// environment, such as MARKET_DATA_COINGECKO_API_KEY.
	APIKey string `yaml:"api_key"`
	// RequestsPerMinute and MonthlyRequests are the limits of the plan,
	// counted across every node; 0 is no limit of that kind.
	RequestsPerMinute int `yaml:"requests_per_minute"`
	MonthlyRequests   int `yaml:"monthly_requests"`
	// TimeoutSeconds bounds one request.
	TimeoutSeconds int `yaml:"timeout_seconds"`
}

// The routes of market_data.egress.
const (
	EgressDirect = "direct"
	EgressProxy  = "proxy"
	EgressNone   = "none"
)

// Route returns the route of the named source on this node, and "none" for
// a source that is not enabled.
func (m MarketDataConfig) Route(source string) string {
	s, ok := m.Sources[source]
	if !ok || !s.Enabled {
		return EgressNone
	}
	if s.Egress != "" {
		return s.Egress
	}
	if m.Egress.Default == "" {
		return EgressDirect
	}
	return m.Egress.Default
}

// Reachable reports whether this node calls the named source.
func (m MarketDataConfig) Reachable(source string) bool {
	return m.Route(source) != EgressNone
}

// The modes of market_data.mode.
const (
	MarketDataModeDryRun = "dry_run"
	MarketDataModeLive   = "live"
)

// DefaultMarketDataHealthAddr is the default address of the probes of
// cmd/market-worker.
const DefaultMarketDataHealthAddr = ":8081"

// The defaults and bounds of the job runner of cmd/market-worker (#264).
const (
	DefaultMarketDataPollSeconds       = 5
	MaxMarketDataPollSeconds           = 300
	DefaultMarketDataLeaseSeconds      = 60
	MinMarketDataLeaseSeconds          = 10
	MaxMarketDataLeaseSeconds          = 3600
	DefaultMarketDataMaxConcurrentJobs = 4
	MaxMarketDataMaxConcurrentJobs     = 64
	DefaultMarketDataRetryBaseSeconds  = 30
	MaxMarketDataRetryBaseSeconds      = 3600
	// DefaultMarketDataSourceTimeoutSeconds bounds one request to a source.
	DefaultMarketDataSourceTimeoutSeconds = 20
	MaxMarketDataSourceTimeoutSeconds     = 120
)

// DefaultJibitEndpoint is the base URL of the Jibit identity API.
const DefaultJibitEndpoint = "https://napi.jibit.ir/ide"

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

	usingExample := false
	if _, statErr := os.Stat(configPath); statErr == nil {
		data, err = os.ReadFile(configPath)
		if err != nil {
			return fmt.Errorf("reading %s: %w", configPath, err)
		}
	} else {
		if requireConfigFile {
			return fmt.Errorf("%w: %s", ErrConfigFileMissing, configPath)
		}
		usingExample = true
		data = configfiles.Example
	}

	Config = Configuration{}
	if err := yaml.Unmarshal(data, &Config); err != nil {
		return fmt.Errorf("parsing %s: %w", configPath, err)
	}
	if err := overlayEnvVars(); err != nil {
		return fmt.Errorf("invalid environment variables: %w", err)
	}
	// Whether the region was named, before the default fills it in: a
	// release must name it (#306), so that an international release that
	// forgets DEPLOYMENT_REGION does not run the features of Iran.
	regionNamed := strings.TrimSpace(Config.Deployment.Region) != ""
	setDefaults()
	if err := Config.validateMode(); err != nil {
		return err
	}
	if usingExample {
		// The example is for development. A release that cannot find its
		// file stops instead of running with it (#306).
		if Config.Server.Mode == "release" {
			return fmt.Errorf("%w: %s; release mode never falls back to the embedded example (set -config, or SERVER_MODE=debug for development)", ErrConfigFileMissing, configPath)
		}
		log.Printf("%s does not exist: using the embedded config/config.example.yaml, which is for development only", configPath)
	}
	if Config.Server.Mode == "release" && !regionNamed {
		return fmt.Errorf("deployment.region (%s) is not set; release mode needs the region named: %s or %s",
			envVarFor("deployment.region"), region.Iran, region.International)
	}
	if _, err := region.Parse(Config.Deployment.Region); err != nil {
		return fmt.Errorf("deployment.region (%s): %w", envVarFor("deployment.region"), err)
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
	reload := c.Keys.ReloadIntervalSeconds
	if reload < MinKeysReloadIntervalSeconds || reload > MaxKeysReloadIntervalSeconds {
		return fmt.Errorf("keys.reload_interval_seconds (%s) is %d; it must be from %d to %d",
			envVarFor("keys.reload_interval_seconds"), reload, MinKeysReloadIntervalSeconds, MaxKeysReloadIntervalSeconds)
	}
	if c.Keys.ActivationDelaySeconds < 2*reload {
		return fmt.Errorf("keys.activation_delay_seconds (%s) is %d; it must be at least twice keys.reload_interval_seconds (%d), so that every instance loads a new key before any writes with it",
			envVarFor("keys.activation_delay_seconds"), c.Keys.ActivationDelaySeconds, 2*reload)
	}
	switch c.Keys.RowSeals.Mode {
	case "", RowSealsReport:
	case RowSealsEnforce:
		if c.Keys.Provider == "" {
			return fmt.Errorf("keys.row_seals.mode (%s) is %q, but keys.provider is not set, so no row is sealed",
				envVarFor("keys.row_seals.mode"), c.Keys.RowSeals.Mode)
		}
	default:
		return fmt.Errorf("keys.row_seals.mode (%s) is %q; it must be %q or %q",
			envVarFor("keys.row_seals.mode"), c.Keys.RowSeals.Mode, RowSealsReport, RowSealsEnforce)
	}
	if v := c.Keys.RowSeals.VerifyIntervalMinutes; v < 1 || v > MaxRowSealsVerifyIntervalMinutes {
		return fmt.Errorf("keys.row_seals.verify_interval_minutes (%s) is %d; it must be from 1 to %d",
			envVarFor("keys.row_seals.verify_interval_minutes"), v, MaxRowSealsVerifyIntervalMinutes)
	}
	return nil
}

// Server modes. They are the modes of Gin.
var serverModes = []string{"debug", "release", "test"}

// DefaultServerMode is the mode of an instance that names none (#306).
const DefaultServerMode = "release"

// ErrConfigFileMissing is returned by Load when the configuration file
// does not exist and the embedded example may not stand in for it.
var ErrConfigFileMissing = errors.New("the configuration file does not exist")

// requireConfigFile is set by RequireConfigFile.
var requireConfigFile bool

// RequireConfigFile makes Load fail when the configuration file does not
// exist, instead of loading the embedded example: the path was named on
// the command line, so a missing file is a mistake (#306).
func RequireConfigFile(required bool) { requireConfigFile = required }

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

// Region returns the market that the release serves (#272). Load refuses
// a value that is not a region, so this is always one.
func (c *Configuration) Region() region.Region {
	r, err := region.Parse(c.Deployment.Region)
	if err != nil {
		return region.Default
	}
	return r
}

// DefaultCandleQuotes are the quotes of market_data.candles.quotes when it
// is not set: the dollar quotes, and tomans (IRT) for a release in Iran.
func DefaultCandleQuotes(r region.Region) []string {
	if r.Iran() {
		return []string{"USDT", "USD", "IRT"}
	}
	return []string{"USDT", "USD"}
}

func setDefaults() {
	Config.Deployment.Region = strings.ToLower(strings.TrimSpace(Config.Deployment.Region))
	if Config.Deployment.Region == "" {
		Config.Deployment.Region = string(region.Default)
	}
	if Config.Server.ReadTimeout == 0 {
		Config.Server.ReadTimeout = 10
	}
	if Config.Server.WriteTimeout == 0 {
		Config.Server.WriteTimeout = 10
	}
	if Config.Server.IdleTimeout == 0 {
		Config.Server.IdleTimeout = 120
	}
	// An instance that names no mode is a production instance (#306): the
	// release checks run unless development opts out with
	// SERVER_MODE=debug, which .env.example, docker-compose.yml, and
	// config.example.yaml do.
	if Config.Server.Mode == "" {
		Config.Server.Mode = DefaultServerMode
	}
	if Config.Server.MaxBodyBytes == 0 {
		Config.Server.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if Config.Server.HSTS.MaxAgeSeconds == 0 {
		Config.Server.HSTS.MaxAgeSeconds = DefaultHSTSMaxAgeSeconds
	}
	if Config.Server.ShutdownTimeout == 0 {
		Config.Server.ShutdownTimeout = DefaultShutdownTimeoutSeconds
	}

	// Row seals refuse a changed row by default in release mode (#306);
	// report, which records it and serves it anyway, is a setting of a
	// rollout, which the server logs at every start.
	if Config.Keys.RowSeals.Mode == "" {
		Config.Keys.RowSeals.Mode = RowSealsReport
		if Config.Server.Mode == "release" && Config.Keys.Provider != "" {
			Config.Keys.RowSeals.Mode = RowSealsEnforce
		}
	}

	if Config.GeoIP.ReloadIntervalSeconds == 0 {
		Config.GeoIP.ReloadIntervalSeconds = DefaultGeoIPReloadIntervalSeconds
	}
	if Config.GeoIP.MaxAgeDays == 0 {
		Config.GeoIP.MaxAgeDays = DefaultGeoIPMaxAgeDays
	}
	if Config.Sites.Scheme == "" {
		Config.Sites.Scheme = DefaultSiteScheme
	}
	if Config.I18n.DefaultLocale == "" {
		Config.I18n.DefaultLocale = DefaultLocale
	}
	if len(Config.I18n.Locales) == 0 {
		Config.I18n.Locales = append([]string(nil), DefaultLocales...)
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
	if Config.Postgres.TargetSessionAttrs == "" {
		Config.Postgres.TargetSessionAttrs = DefaultPostgresTargetSessionAttrs
	}
	if Config.Postgres.Migrations.Mode == "" {
		Config.Postgres.Migrations.Mode = MigrationsModeStartup
	}
	for _, d := range []struct {
		field *int
		def   int
	}{
		{&Config.Postgres.StatementTimeoutMs, DefaultPostgresStatementTimeoutMs},
		{&Config.Postgres.LockTimeoutMs, DefaultPostgresLockTimeoutMs},
		{&Config.Postgres.IdleInTransactionTimeoutMs, DefaultPostgresIdleInTransactionTimeoutMs},
		{&Config.Postgres.MigrationLockTimeoutSeconds, DefaultPostgresMigrationLockTimeoutSeconds},
	} {
		if *d.field == 0 {
			*d.field = d.def
		}
	}
	if Config.Postgres.ConnectRetries == 0 {
		Config.Postgres.ConnectRetries = 3
	}
	if Config.Postgres.ConnectRetryDelayMs == 0 {
		Config.Postgres.ConnectRetryDelayMs = 500
	}

	if Config.Redis.Mode == "" {
		Config.Redis.Mode = RedisModeStandalone
	}
	if Config.Redis.DurableWrites.TimeoutMs == 0 {
		Config.Redis.DurableWrites.TimeoutMs = DefaultRedisDurableWritesTimeoutMs
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
		{&Config.RateLimits.IPv6PrefixLength, clientip.DefaultIPv6PrefixLength},
	} {
		if *ms.value == 0 {
			*ms.value = ms.fallback
		}
	}

	setNotificationDefaults(&Config.Notifications)

	if Config.Identity.MobileOwnership.Provider == "" {
		Config.Identity.MobileOwnership.Provider = MobileOwnershipNone
	}
	if Config.Identity.MobileOwnership.Provider == MobileOwnershipJibit && Config.Identity.MobileOwnership.Endpoint == "" {
		Config.Identity.MobileOwnership.Endpoint = DefaultJibitEndpoint
	}

	if Config.Keys.Transit.Mount == "" {
		Config.Keys.Transit.Mount = "transit"
	}
	if Config.Keys.RowSeals.VerifyIntervalMinutes == 0 {
		Config.Keys.RowSeals.VerifyIntervalMinutes = DefaultRowSealsVerifyIntervalMinutes
	}
	if Config.Keys.ReloadIntervalSeconds == 0 {
		Config.Keys.ReloadIntervalSeconds = DefaultKeysReloadIntervalSeconds
	}
	if Config.Keys.ActivationDelaySeconds == 0 {
		Config.Keys.ActivationDelaySeconds = max(DefaultKeysActivationDelaySeconds, 2*Config.Keys.ReloadIntervalSeconds)
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
		{&Config.WebSocket.MaxConnectionsPerUser, DefaultWSMaxConnectionsPerUser},
		{&Config.WebSocket.SessionCheckSeconds, DefaultWSSessionCheckSeconds},
		{&Config.Admin.SignInMinResponseMS, DefaultAdminSignInMinResponseMS},
		{&Config.Admin.Sessions.MaxPerAdmin, DefaultMaxSessionsPerAdmin},
		{&Config.JWT.RefreshToken.ReuseGraceSeconds, DefaultRefreshReuseGraceSeconds},
		{&Config.JWT.Sessions.MaxPerUser, DefaultMaxSessionsPerUser},
		{&Config.JWT.Sessions.MaxLifetimeMinutes, DefaultSessionMaxLifetimeMinutes},
		{&Config.Favorites.MaxPerUser, DefaultMaxFavoritesPerUser},
		{&Config.Contacts.NewContactCooldownHours, DefaultNewContactCooldownHours},
		{&Config.Security.ChangeHoldHours, DefaultSecurityChangeHoldHours},
		{&Config.Identity.MobileOwnership.TimeoutSeconds, 10},
		{&Config.Identity.MobileOwnership.RecheckDays, 30},
		{&Config.Delisting.IntervalSeconds, DefaultDelistingIntervalSeconds},
		{&Config.MarketData.PollSeconds, DefaultMarketDataPollSeconds},
		{&Config.MarketData.LeaseSeconds, DefaultMarketDataLeaseSeconds},
		{&Config.MarketData.MaxConcurrentJobs, DefaultMarketDataMaxConcurrentJobs},
		{&Config.MarketData.RetryBaseSeconds, DefaultMarketDataRetryBaseSeconds},
		{&Config.Platform.PollIntervalMs, DefaultPlatformPollIntervalMs},
		{&Config.Platform.StaleAfterSeconds, DefaultPlatformStaleAfterSeconds},
	} {
		if *setting.value == 0 {
			*setting.value = setting.fallback
		}
	}

	if Config.MarketData.Mode == "" {
		Config.MarketData.Mode = MarketDataModeDryRun
	}
	if Config.MarketData.Egress.Default == "" {
		Config.MarketData.Egress.Default = EgressDirect
	}
	for name, s := range Config.MarketData.Sources {
		if s.TimeoutSeconds == 0 {
			s.TimeoutSeconds = DefaultMarketDataSourceTimeoutSeconds
			Config.MarketData.Sources[name] = s
		}
	}
	if Config.MarketData.HealthAddr == "" {
		Config.MarketData.HealthAddr = DefaultMarketDataHealthAddr
	}
	candles := &Config.MarketData.Candles
	if len(candles.Quotes) == 0 {
		candles.Quotes = DefaultCandleQuotes(Config.Region())
	}
	// An international release shows the global venues only (#272).
	if len(Config.MarketData.API.Regions) == 0 && Config.Region().International() {
		Config.MarketData.API.Regions = []string{"global"}
	}
	// A resolution that the file does not name takes its default; one it
	// names keeps its own settings, with defaults for those left at zero.
	resolutions := DefaultCandleResolutions()
	for name, r := range candles.Resolutions {
		if d, ok := resolutions[name]; ok {
			if r.IntervalMinutes == 0 {
				r.IntervalMinutes = d.IntervalMinutes
			}
			if r.BackfillDays == 0 {
				r.BackfillDays = d.BackfillDays
			}
			if len(r.Tiers) == 0 {
				r.Tiers = d.Tiers
			}
		}
		resolutions[name] = r
	}
	candles.Resolutions = resolutions
	tiers := &Config.MarketData.Tiers
	if tiers.HotAssets == nil {
		tiers.HotAssets = DefaultHotAssets()
	}
	if tiers.HotListed == nil {
		hotListed := true
		tiers.HotListed = &hotListed
	}
	streams := &Config.MarketData.Streams
	if streams.Enabled == nil {
		enabled := true
		streams.Enabled = &enabled
	}
	if streams.Tiers == nil {
		streams.Tiers = []string{TierHot}
	}
	catalog := &Config.MarketData.Catalog
	if len(catalog.Priority) == 0 {
		catalog.Priority = slices.Clone(CatalogSources)
	}
	if catalog.Coverage == "" {
		catalog.Coverage = CatalogCoverageList
	}
	for _, setting := range []struct {
		value    *int
		fallback int
	}{
		{&catalog.Top, DefaultCatalogTop},
		{&catalog.IntervalHours, DefaultCatalogIntervalHours},
		{&catalog.DetailsPerRun, DefaultCatalogDetailsPerRun},
		{&catalog.DetailsMaxAgeDays, DefaultCatalogDetailsMaxAgeDays},
		{&catalog.DeactivateAfterDays, DefaultCatalogDeactivateAfterDays},
		{&Config.MarketData.Tickers.IntervalSeconds, DefaultTickersIntervalSeconds},
		{&Config.MarketData.Tickers.StaleAfterSeconds, DefaultTickersStaleAfterSeconds},
		{&Config.MarketData.Tickers.OutlierPercent, DefaultTickersOutlierPercent},
		{&Config.MarketData.Tickers.MinVenues, DefaultTickersMinVenues},
		{&Config.MarketData.Tickers.MapRankLimit, DefaultTickersMapRankLimit},
		{&Config.MarketData.Tickers.DeactivatePairsAfterHours, DefaultTickersDeactivateAfterHrs},
		{&Config.MarketData.Candles.MaxPairsPerVenue, DefaultCandlesMaxPairsPerVenue},
		{&Config.MarketData.Candles.RequestsPerRun, DefaultCandlesRequestsPerRun},
		{&Config.MarketData.API.CacheSeconds, DefaultMarketDataAPICacheSeconds},
		{&Config.MarketData.Tiers.IntervalHours, DefaultTiersIntervalHours},
		{&Config.MarketData.Streams.MaxPairs, DefaultStreamsMaxPairs},
		{&Config.MarketData.Streams.MaxPerWorker, DefaultStreamsMaxPerWorker},
		{&Config.MarketData.Streams.RunMinutes, DefaultStreamsRunMinutes},
		{&Config.MarketData.Streams.FlushMillis, DefaultStreamsFlushMillis},
		{&Config.MarketData.Streams.QuietSeconds, DefaultStreamsQuietSeconds},
		{&Config.MarketData.Tiers.ActiveTop, DefaultTiersActiveTop},
		{&Config.MarketData.Tiers.HysteresisPercent, DefaultTiersHysteresisPercent},
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

func GetRabbitMQURL() string {
	return RabbitMQURL(Config.RabbitMQ.Username, Config.RabbitMQ.Password,
		Config.RabbitMQ.Host, Config.RabbitMQ.Port, Config.RabbitMQ.VHost, Config.RabbitMQ.TLS.Enabled)
}

// RabbitMQNode is one node that the RabbitMQ service may dial: its host,
// for the TLS server name, its address (host:port), for the logs, and its
// URI, which holds the password and must never be logged.
type RabbitMQNode struct {
	Host    string
	Address string
	URL     string
}

// RabbitMQNodes returns the nodes of rabbitmq.hosts, or the one node of
// rabbitmq.host and rabbitmq.port when hosts is empty (#232).
func RabbitMQNodes() []RabbitMQNode {
	r := Config.RabbitMQ
	if len(r.Hosts) == 0 {
		return []RabbitMQNode{{Host: r.Host, Address: net.JoinHostPort(r.Host, r.Port), URL: GetRabbitMQURL()}}
	}
	nodes := make([]RabbitMQNode, 0, len(r.Hosts))
	for _, hostPort := range r.Hosts {
		host, port, err := net.SplitHostPort(strings.TrimSpace(hostPort))
		if err != nil {
			continue // validateServer reports it
		}
		nodes = append(nodes, RabbitMQNode{Host: host, Address: net.JoinHostPort(host, port), URL: RabbitMQURL(r.Username, r.Password, host, port, r.VHost, r.TLS.Enabled)})
	}
	return nodes
}

// RabbitMQURL builds the AMQP URI of a broker (#185). The user, the
// password, and the virtual host are escaped, so that generated passwords
// with characters such as "@", "/", ":", "#", "?", or "%" reach the broker
// unchanged instead of changing the host or the virtual host of the URI.
// An empty vhost, or "/", is the broker's default virtual host, which an
// empty path selects. The URI holds the password, so it must never be
// logged.
//
// With useTLS, the scheme is amqps, so that the client dials with TLS.
func RabbitMQURL(username, password, host, port, vhost string, useTLS bool) string {
	scheme := "amqp"
	if useTLS {
		scheme = "amqps"
	}
	u := url.URL{
		Scheme: scheme,
		User:   url.UserPassword(username, password),
		Host:   net.JoinHostPort(host, port),
		Path:   "/",
	}
	if vhost != "" && vhost != "/" {
		// The AMQP URI spec puts the virtual host, percent-encoded, in the
		// single path segment, so a "/" in it becomes "%2F".
		u.Path, u.RawPath = "/"+vhost, "/"+url.PathEscape(vhost)
	}
	return u.String()
}

// DefaultRefreshReuseGraceSeconds covers refreshes that start together, and
// MaxRefreshReuseGraceSeconds keeps a stolen previous token useful only
// briefly.
const (
	DefaultRefreshReuseGraceSeconds = 10
	MaxRefreshReuseGraceSeconds     = 60
	// DefaultMaxSessionsPerUser allows a phone, a laptop, and a few
	// browsers; MaxSessionsPerUser keeps the list of sessions short enough
	// to show in a refusal.
	DefaultMaxSessionsPerUser = 5
	MaxSessionsPerUser        = 50
	// DefaultSessionMaxLifetimeMinutes is the absolute lifetime of a
	// session, 30 days: a user signs in again at least that often.
	DefaultSessionMaxLifetimeMinutes = 30 * 24 * 60
	// DefaultNewContactCooldownHours and MaxNewContactCooldownHours bound
	// contacts.new_contact_cooldown_hours (#250): three days by default,
	// and at most thirty.
	DefaultNewContactCooldownHours = 72
	MaxNewContactCooldownHours     = 720

	// DefaultSecurityChangeHoldHours and MaxSecurityChangeHoldHours bound
	// security.change_hold_hours (#251): a day by default, and at most
	// thirty.
	DefaultSecurityChangeHoldHours = 24
	MaxSecurityChangeHoldHours     = 720

	// DefaultMaxFavoritesPerUser and MaxFavoritesPerUser bound the
	// favorites of a user, which are listed in one answer.
	DefaultMaxFavoritesPerUser = 100
	MaxFavoritesPerUser        = 1000

	// DefaultDelistingIntervalSeconds and MaxDelistingIntervalSeconds bound
	// how late after its time a step of a delisting is applied.
	DefaultDelistingIntervalSeconds = 30
	MaxDelistingIntervalSeconds     = 3600

	// The bounds of the platform settings (#235). A change of a control
	// applies on every instance within platform.poll_interval_ms.
	DefaultPlatformPollIntervalMs    = 1000
	MinPlatformPollIntervalMs        = 100
	MaxPlatformPollIntervalMs        = 60000
	DefaultPlatformStaleAfterSeconds = 30
	MaxPlatformStaleAfterSeconds     = 3600
)

// DefaultAdminSignInMinResponseMS is longer than the work of a sign-in
// request for an administrator, which includes a broker confirm, and
// MaxAdminSignInMinResponseMS keeps it below the request timeout.
const (
	DefaultAdminSignInMinResponseMS = 400
	MaxAdminSignInMinResponseMS     = 5000
	// DefaultMaxSessionsPerAdmin allows a second device, and
	// MaxSessionsPerAdmin bounds what one administrator can hold.
	DefaultMaxSessionsPerAdmin = 2
	MaxSessionsPerAdmin        = 10
)

// The defaults of the websocket settings. Clients send small control
// messages, such as pings and subscriptions, so a message is at most 4 KiB.
const (
	DefaultWSMaxMessageSize          = 4 << 10
	MaxWSMaxMessageSize              = 1 << 20
	DefaultWSWriteTimeoutSeconds     = 10
	DefaultWSClientMessagesPerSecond = 10
	DefaultWSClientMessageBurst      = 20
	// DefaultWSSessionCheckSeconds bounds how long a connection outlives
	// the end of its session.
	DefaultWSSessionCheckSeconds = 60
	DefaultWSMaxConnections      = 10000
	DefaultWSMaxConnectionsPerIP = 20
	// DefaultWSMaxConnectionsPerUser is enough for a user with a few
	// devices and browser tabs.
	DefaultWSMaxConnectionsPerUser = 10
)

// DefaultCurrencyCacheTTLSeconds is redis.currency_cache_ttl_seconds when
// it is not set.
const DefaultCurrencyCacheTTLSeconds = 300

// DefaultShutdownTimeoutSeconds is server.shutdown_timeout_seconds when it
// is not set.
const DefaultShutdownTimeoutSeconds = 30

// DefaultHSTSMaxAgeSeconds is server.hsts.max_age_seconds when it is not
// set: one year.
const DefaultHSTSMaxAgeSeconds = 365 * 24 * 60 * 60

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
	// DefaultRedisDurableWritesTimeoutMs is how long a durable write waits
	// for its replicas. Replication within a data center takes about a
	// millisecond; the wait is on the path of a sign-in.
	DefaultRedisDurableWritesTimeoutMs = 250
	DefaultRateLimitTimeoutMs          = 250
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
// HSTSEnabled reports whether responses carry Strict-Transport-Security:
// as set, or else when the server or a proxy in front of it terminates TLS.
// Browsers ignore the header on plain HTTP, so a proxy that also serves
// HTTP is not a problem.
func (c *Configuration) HSTSEnabled() bool {
	if c.Server.HSTS.Enabled != nil {
		return *c.Server.HSTS.Enabled
	}
	return c.Server.TLS.Enabled || c.Server.TLS.BehindProxy
}

func (c *Configuration) SwaggerEnabled() bool {
	if c.Server.Swagger.Enabled != nil {
		return *c.Server.Swagger.Enabled
	}
	return c.Server.Mode != "release"
}

// The modes of redis.mode.
const (
	RedisModeStandalone = "standalone"
	RedisModeSentinel   = "sentinel"
	RedisModeCluster    = "cluster"
)

// RedisModes lists the values of redis.mode.
var RedisModes = []string{RedisModeStandalone, RedisModeSentinel, RedisModeCluster}

// The locales of a configuration that lists none (#283): English, the
// fallback of every message, and Persian. A release outside Iran may list
// only the languages of its markets.
const DefaultLocale = "en"

var DefaultLocales = []string{"en", "fa"}

// Locales builds the registry of the configured locales (#283). A
// configuration that lists none, such as one built by a test, gets the
// defaults.
func (c *Configuration) Locales() (*i18n.Registry, error) {
	def, codes := c.I18n.DefaultLocale, c.I18n.Locales
	if def == "" {
		def = DefaultLocale
	}
	if len(codes) == 0 {
		codes = DefaultLocales
	}
	return i18n.New(def, codes)
}

// The GeoIP defaults and limits (#284).
const (
	DefaultGeoIPReloadIntervalSeconds = 300
	MinGeoIPReloadIntervalSeconds     = 10
	MaxGeoIPReloadIntervalSeconds     = 86400
	DefaultGeoIPMaxAgeDays            = 45
	MaxGeoIPMaxAgeDays                = 365
)

// SiteConfig is one site of the product (#285).
type SiteConfig struct {
	// ID names the site, such as global or ir.
	ID string `yaml:"id"`
	// Domain is its canonical host; Aliases are other hosts of the same
	// site, such as www.example.com.
	Domain  string   `yaml:"domain"`
	Aliases []string `yaml:"aliases"`
	// Countries are the ISO 3166-1 alpha-2 codes of the countries whose
	// visitors belong to it. A country belongs to one site at most.
	Countries []string `yaml:"countries"`
	// Locales are codes of i18n.locales; the first is its default.
	Locales []string `yaml:"locales"`
	// Enforce keeps the visitors of its countries on it, even when they
	// chose another site, for a legal requirement.
	Enforce bool `yaml:"enforce"`
}

// DefaultSiteScheme is the scheme of the sites' URLs.
const DefaultSiteScheme = "https"

// SiteRegistry builds the table of sites (#285) on locales.
func (c *Configuration) SiteRegistry(locales *i18n.Registry) (*sites.Registry, error) {
	specs := make([]sites.Spec, 0, len(c.Sites.List))
	for _, s := range c.Sites.List {
		specs = append(specs, sites.Spec{ID: s.ID, Domain: s.Domain, Aliases: s.Aliases, Countries: s.Countries, Locales: s.Locales, Enforce: s.Enforce})
	}
	scheme := c.Sites.Scheme
	if scheme == "" {
		scheme = DefaultSiteScheme
	}
	return sites.New(c.Sites.Default, scheme, specs, locales)
}

// SiteOrigins returns the browser origins of every domain of the sites
// (#285), which CORS and the WebSocket allow besides their own lists.
func (c *Configuration) SiteOrigins() []string {
	scheme := c.Sites.Scheme
	if scheme == "" {
		scheme = DefaultSiteScheme
	}
	var origins []string
	for _, s := range c.Sites.List {
		for _, host := range append([]string{s.Domain}, s.Aliases...) {
			if host, err := sites.NormalizeHost(host); err == nil {
				origins = append(origins, scheme+"://"+host)
			}
		}
	}
	return origins
}
