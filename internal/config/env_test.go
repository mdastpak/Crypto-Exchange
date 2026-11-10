package config

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var envAssignment = regexp.MustCompile(`^(#\s*)?([A-Z][A-Z0-9_]*)=(.*)$`)

// envExample returns the assignments in .env.example, and the names of the
// ones that are commented out.
func envExample(t *testing.T) (map[string]string, []string) {
	t.Helper()
	file, err := os.Open(filepath.Join("..", "..", ".env.example"))
	require.NoError(t, err)
	defer file.Close()

	values := map[string]string{}
	var commented []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		m := envAssignment.FindStringSubmatch(strings.TrimRight(scanner.Text(), "\r"))
		if m == nil {
			continue
		}
		if m[1] != "" {
			commented = append(commented, m[2])
			continue
		}
		values[m[2]] = m[3]
	}
	require.NoError(t, scanner.Err())
	require.NotEmpty(t, values)
	return values, commented
}

func lookupIn(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func envVarNames() map[string]bool {
	names := map[string]bool{}
	for _, v := range envVars {
		names[v.Name] = true
	}
	return names
}

// TestEnvExample_EveryVariableIsHonored checks that every variable in
// .env.example, also the ones commented out, is read, and that each one
// changes the effective configuration on its own.
func TestEnvExample_EveryVariableIsHonored(t *testing.T) {
	values, commented := envExample(t)
	known := envVarNames()
	for _, name := range append(commented, keys(values)...) {
		assert.True(t, known[name], "%s is in .env.example but is not read", name)
	}

	for name := range values {
		if !known[name] {
			continue
		}
		changed := false
		// One of these values suits every kind of setting, and none is a
		// default.
		for _, candidate := range []string{"17", "true"} {
			var base, c Configuration
			if err := overlayEnv(&c, lookupIn(map[string]string{name: candidate})); err != nil {
				continue
			}
			changed = !reflect.DeepEqual(base, c)
			break
		}
		assert.True(t, changed, "%s does not change the configuration", name)
	}

	var c Configuration
	assert.NoError(t, overlayEnv(&c, lookupIn(values)), "the values in .env.example must be valid")
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestEnvVars_AreDocumented(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "configuration.md"))
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, v := range envVars {
		assert.False(t, seen[v.Name], "%s is listed twice", v.Name)
		seen[v.Name] = true
		assert.Contains(t, string(doc), "`"+v.Name+"`", "%s is not documented in docs/configuration.md", v.Name)
	}
}

func TestOverlayEnv_SetsTheNamedSetting(t *testing.T) {
	var c Configuration
	require.NoError(t, overlayEnv(&c, lookupIn(map[string]string{
		"POSTGRES_DB":              "ledger",
		"REDIS_DB":                 "3",
		"SERVER_TLS_ENABLED":       "true",
		"WS_ALLOWED_ORIGINS":       " https://a.example , ,https://b.example ",
		"JWT_SSL_USER_PRIVATE_KEY": "/keys/user.pem",
		"SERVER_MODE":              "",
	})))
	assert.Equal(t, "ledger", c.Postgres.DBName)
	assert.Equal(t, 3, c.Redis.DB)
	assert.True(t, c.Server.TLS.Enabled)
	assert.Equal(t, []string{"https://a.example", "https://b.example"}, c.WebSocket.AllowedOrigins)
	assert.Equal(t, KeyPath("/keys/user.pem"), c.JWT.SSL.User.PrivateKey)
	assert.Empty(t, c.Server.Mode, "an empty variable leaves the setting unchanged")
}

func TestOverlayEnv_ReportsEveryInvalidValue(t *testing.T) {
	var c Configuration
	err := overlayEnv(&c, lookupIn(map[string]string{
		"REDIS_DB":           "two",
		"SERVER_TLS_ENABLED": "sometimes",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "REDIS_DB (redis.db)")
	assert.Contains(t, err.Error(), "SERVER_TLS_ENABLED (server.tls.enabled)")
	assert.NotContains(t, err.Error(), string(rune(10)), "the message is one log line, without line breaks")
}

func TestOverlayEnv_ServerModeWinsOverGinMode(t *testing.T) {
	var c Configuration
	require.NoError(t, overlayEnv(&c, lookupIn(map[string]string{"GIN_MODE": "release"})))
	assert.Equal(t, "release", c.Server.Mode, "GIN_MODE still selects the mode")

	c = Configuration{}
	require.NoError(t, overlayEnv(&c, lookupIn(map[string]string{"GIN_MODE": "release", "SERVER_MODE": "test"})))
	assert.Equal(t, "test", c.Server.Mode)
}

// configFile returns the path of a file in config/, where the configuration
// files are, from the directory of this package.
func configFile(name string) string {
	return filepath.Join("..", "..", "config", name)
}

// loadFile loads a configuration file with only the given environment
// variables set among the ones the configuration reads.
func loadFile(t *testing.T, path string, env map[string]string) error {
	t.Helper()
	for _, v := range envVars {
		t.Setenv(v.Name, env[v.Name])
	}
	original := configPath
	t.Cleanup(func() { configPath = original; resetConfig() })
	SetConfigPath(path)
	return Load()
}

func TestLoad_RejectsAnUnknownServerMode(t *testing.T) {
	err := loadFile(t, configFile("config.compose.yaml"), map[string]string{"SERVER_MODE": "production"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `server.mode (SERVER_MODE) is "production"`)

	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), map[string]string{"GIN_MODE": "release"}))
	assert.Equal(t, "release", Config.Server.Mode)
}

func TestLoad_ReportsInvalidEnvironmentVariables(t *testing.T) {
	err := loadFile(t, configFile("config.compose.yaml"), map[string]string{"POSTGRES_MAX_OPEN_CONNS": "many"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "POSTGRES_MAX_OPEN_CONNS")
}

func TestKeyPath_AcceptsAStringOrPathSegments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
deployment:
  region: "ir"
jwt:
  ssl:
    user:
      private_key: "keys/user/private.pem"
      public_key: ["keys", "user", "public.pem"]
      retired_public_keys:
        - "keys/user/old.pem"
        - ["keys", "user", "older.pem"]
`), 0o600))
	require.NoError(t, loadFile(t, path, nil))

	user := Config.JWT.SSL.User
	assert.Equal(t, KeyPath("keys/user/private.pem"), user.PrivateKey)
	assert.Equal(t, KeyPath(filepath.Join("keys", "user", "public.pem")), user.PublicKey)
	assert.Equal(t, []KeyPath{"keys/user/old.pem", KeyPath(filepath.Join("keys", "user", "older.pem"))}, user.RetiredPublicKeys)

	require.NoError(t, os.WriteFile(path, []byte("jwt:\n  ssl:\n    user:\n      private_key: {file: x}\n"), 0o600))
	assert.ErrorContains(t, loadFile(t, path, nil), "a key path must be a string")
}

func TestValidateServer_NamesEveryMissingSetting(t *testing.T) {
	var c Configuration
	err := c.validateServer()
	require.Error(t, err)
	for _, want := range []string{
		"server.port (SERVER_PORT)",
		"postgres.dbname (POSTGRES_DB)",
		"rabbitmq.username (RABBITMQ_USER)",
		"rabbitmq.password (RABBITMQ_PASSWORD)",
		"jwt.ssl.user.private_key (JWT_SSL_USER_PRIVATE_KEY)",
		"jwt.access_token.expiration_minutes (JWT_ACCESS_TOKEN_EXPIRATION_MINUTES)",
	} {
		assert.Contains(t, err.Error(), want)
	}
}

// TestConfigExample_StartsWithCredentialsFromTheEnvironment loads the
// example file, which is also the embedded fallback, as a local run would.
func TestConfigExample_StartsWithCredentialsFromTheEnvironment(t *testing.T) {
	data, err := os.ReadFile(configFile("config.example.yaml"))
	require.NoError(t, err)
	assert.NotRegexp(t, `(?m)^[^#]*:\s*"?\$\{`, string(data), "placeholders are not expanded, so values must not use them")

	err = loadFile(t, configFile("config.example.yaml"), nil)
	require.NoError(t, err)
	err = ValidateServer()
	require.Error(t, err, "RabbitMQ credentials come from the environment")
	assert.Contains(t, err.Error(), "rabbitmq.username (RABBITMQ_USER)")

	require.NoError(t, loadFile(t, configFile("config.example.yaml"), map[string]string{
		"RABBITMQ_USER": "guest", "RABBITMQ_PASSWORD": "guest",
	}))
	assert.NoError(t, ValidateServer())
	assert.Equal(t, KeyPath("internal/ssl/user/private_key.pem"), Config.JWT.SSL.User.PrivateKey)
}

// TestComposeConfig_WithEnvExample loads the app service's configuration
// with the variables that it receives when .env is a copy of .env.example:
// those of the file, overridden by the service's environment.
func TestComposeConfig_WithEnvExample(t *testing.T) {
	values, _ := envExample(t)
	for name, value := range map[string]string{
		"SERVER_IP": "0.0.0.0", "SERVER_PORT": "8080",
		"POSTGRES_HOST": "postgres", "REDIS_HOST": "redis", "RABBITMQ_HOST": "rabbitmq",
		"JWT_SSL_USER_PRIVATE_KEY":  "/run/secrets/jwt_user_private_key",
		"JWT_SSL_USER_PUBLIC_KEY":   "/run/secrets/jwt_user_public_key",
		"JWT_SSL_ADMIN_PRIVATE_KEY": "", "JWT_SSL_ADMIN_PUBLIC_KEY": "",
	} {
		values[name] = value
	}
	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), values))
	require.NoError(t, ValidateServer())
	assert.Equal(t, "exchange", Config.Postgres.DBName)
	assert.Equal(t, "guest", Config.RabbitMQ.Username)
	assert.Equal(t, KeyPath("/run/secrets/jwt_user_private_key"), Config.JWT.SSL.User.PrivateKey)
	assert.Empty(t, Config.JWT.SSL.Admin.PrivateKey)
	assert.Equal(t, "debug", Config.Server.Mode)
}

func TestSwaggerEnabled_DefaultsToOffInReleaseMode(t *testing.T) {
	on, off := true, false
	cases := []struct {
		mode    string
		enabled *bool
		want    bool
	}{
		{"debug", nil, true},
		{"test", nil, true},
		{"release", nil, false},
		{"release", &on, true},
		{"debug", &off, false},
	}
	for _, tc := range cases {
		var c Configuration
		c.Server.Mode = tc.mode
		c.Server.Swagger.Enabled = tc.enabled
		assert.Equal(t, tc.want, c.SwaggerEnabled(), "mode %s, enabled %v", tc.mode, tc.enabled)
	}

	var c Configuration
	c.Server.Mode = "release"
	require.NoError(t, overlayEnv(&c, lookupIn(map[string]string{"SERVER_SWAGGER_ENABLED": "true", "SERVER_MAX_BODY_BYTES": "2048"})))
	assert.True(t, c.SwaggerEnabled(), "SERVER_SWAGGER_ENABLED turns it on in release mode")
	assert.Equal(t, int64(2048), c.Server.MaxBodyBytes)
}

func TestRateLimitOnError_DefaultsAndValidation(t *testing.T) {
	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), nil))
	limits := Config.RateLimits
	assert.Equal(t, RateLimitAllow, limits.REST.OnError, "the API stays available while Redis is unreachable")
	assert.Equal(t, RateLimitAllow, limits.WS.OnError)
	assert.Equal(t, RateLimitAllow, limits.Trading.OnError)
	assert.Equal(t, RateLimitDeny, limits.Auth.OnError, "sign-in limits fail closed")

	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), map[string]string{"RATE_LIMITS_REST_ON_ERROR": "deny", "RATE_LIMITS_AUTH_ON_ERROR": "allow"}))
	assert.Equal(t, RateLimitDeny, Config.RateLimits.REST.OnError)
	assert.Equal(t, RateLimitAllow, Config.RateLimits.Auth.OnError)

	err := loadFile(t, configFile("config.compose.yaml"), map[string]string{"RATE_LIMITS_WS_ON_ERROR": "ignore"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `rate_limits.ws.on_error (RATE_LIMITS_WS_ON_ERROR) is "ignore"`)
}

func TestRedisTimeouts_DefaultsAndValidation(t *testing.T) {
	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), nil))
	assert.Equal(t, DefaultRedisDialTimeoutMs, Config.Redis.DialTimeoutMs)
	assert.Equal(t, DefaultRedisReadTimeoutMs, Config.Redis.ReadTimeoutMs)
	assert.Equal(t, DefaultRedisWriteTimeoutMs, Config.Redis.WriteTimeoutMs)
	assert.Equal(t, DefaultRedisPoolTimeoutMs, Config.Redis.PoolTimeoutMs)
	assert.Equal(t, DefaultRedisMaxRetries, Config.Redis.MaxRetries)
	assert.Equal(t, DefaultRateLimitTimeoutMs, Config.RateLimits.TimeoutMs)

	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), map[string]string{
		"REDIS_DIAL_TIMEOUT_MS": "300", "REDIS_READ_TIMEOUT_MS": "400", "REDIS_WRITE_TIMEOUT_MS": "500",
		"REDIS_POOL_TIMEOUT_MS": "600", "REDIS_MAX_RETRIES": "-1", "RATE_LIMITS_TIMEOUT_MS": "50",
	}))
	assert.Equal(t, 300, Config.Redis.DialTimeoutMs)
	assert.Equal(t, 400, Config.Redis.ReadTimeoutMs)
	assert.Equal(t, 500, Config.Redis.WriteTimeoutMs)
	assert.Equal(t, 600, Config.Redis.PoolTimeoutMs)
	assert.Equal(t, -1, Config.Redis.MaxRetries, "-1 turns retries off")
	assert.Equal(t, 50, Config.RateLimits.TimeoutMs)

	err := loadFile(t, configFile("config.compose.yaml"), map[string]string{"RATE_LIMITS_TIMEOUT_MS": "-5"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rate_limits.timeout_ms (RATE_LIMITS_TIMEOUT_MS) is -5")
	err = loadFile(t, configFile("config.compose.yaml"), map[string]string{"REDIS_READ_TIMEOUT_MS": "-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redis.read_timeout_ms (REDIS_READ_TIMEOUT_MS) is -1")
	err = loadFile(t, configFile("config.compose.yaml"), map[string]string{"REDIS_MAX_RETRIES": "-2"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redis.max_retries (REDIS_MAX_RETRIES) is -2")
}

func TestMilliseconds_FallsBackWhenNotSet(t *testing.T) {
	assert.Equal(t, 300*time.Millisecond, Milliseconds(300, 1000))
	assert.Equal(t, time.Second, Milliseconds(0, 1000), "a configuration that Load did not fill in")
}

func TestKeys_DefaultsAndValidation(t *testing.T) {
	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), nil))
	assert.Equal(t, "", Config.Keys.Provider, "the data-protection keys are off unless configured")
	assert.Equal(t, "transit", Config.Keys.Transit.Mount)
	assert.Equal(t, 10, Config.Keys.Transit.Timeout)

	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), map[string]string{
		"KEYS_PROVIDER": "transit", "VAULT_ADDR": "https://vault:8200", "VAULT_TOKEN": "s.token", "VAULT_CACERT": "/run/ca.pem",
		"KEYS_TRANSIT_TOKEN_FILE": "/run/token", "KEYS_TRANSIT_MOUNT": "kms", "KEYS_TRANSIT_KEY": "data-keys", "KEYS_TRANSIT_TIMEOUT_SECONDS": "3",
	}))
	transit := Config.Keys.Transit
	assert.Equal(t, KeysProviderTransit, Config.Keys.Provider)
	assert.Equal(t, []string{"https://vault:8200", "s.token", "/run/ca.pem", "/run/token", "kms", "data-keys"},
		[]string{transit.Address, transit.Token, transit.CACert, transit.TokenFile, transit.Mount, transit.Key})
	assert.Equal(t, 3, transit.Timeout)

	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), map[string]string{"KEYS_PROVIDER": "local", "KEYS_LOCAL_KEK": "a2V5"}))
	assert.Equal(t, "a2V5", Config.Keys.Local.KEK)

	err := loadFile(t, configFile("config.compose.yaml"), map[string]string{"KEYS_PROVIDER": "aws"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `keys.provider (KEYS_PROVIDER) is "aws"`)
	err = loadFile(t, configFile("config.compose.yaml"), map[string]string{"KEYS_TRANSIT_TIMEOUT_SECONDS": "-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keys.transit.timeout_seconds (KEYS_TRANSIT_TIMEOUT_SECONDS) is -1")
}

// TestKeys_ReloadAndActivation checks the settings of the two-phase
// rotation of the data keys (#233).
func TestKeys_ReloadAndActivation(t *testing.T) {
	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), nil))
	assert.Equal(t, DefaultKeysReloadIntervalSeconds, Config.Keys.ReloadIntervalSeconds)
	assert.Equal(t, DefaultKeysActivationDelaySeconds, Config.Keys.ActivationDelaySeconds)

	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), map[string]string{"KEYS_RELOAD_INTERVAL_SECONDS": "600"}))
	assert.Equal(t, 1200, Config.Keys.ActivationDelaySeconds, "the default delay follows a long interval")

	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), map[string]string{
		"KEYS_RELOAD_INTERVAL_SECONDS": "10", "KEYS_ACTIVATION_DELAY_SECONDS": "20",
	}))
	assert.Equal(t, 10, Config.Keys.ReloadIntervalSeconds)
	assert.Equal(t, 20, Config.Keys.ActivationDelaySeconds)

	for env, want := range map[string]string{
		"KEYS_RELOAD_INTERVAL_SECONDS=4":    "keys.reload_interval_seconds (KEYS_RELOAD_INTERVAL_SECONDS) is 4",
		"KEYS_RELOAD_INTERVAL_SECONDS=3601": "keys.reload_interval_seconds (KEYS_RELOAD_INTERVAL_SECONDS) is 3601",
		"KEYS_ACTIVATION_DELAY_SECONDS=119": "keys.activation_delay_seconds (KEYS_ACTIVATION_DELAY_SECONDS) is 119; it must be at least twice",
	} {
		name, value, _ := strings.Cut(env, "=")
		err := loadFile(t, configFile("config.compose.yaml"), map[string]string{name: value})
		require.Error(t, err, env)
		assert.Contains(t, err.Error(), want)
	}
}

// TestPostgres_AvailabilitySettings checks the defaults, environment
// variables, and validation of the PostgreSQL availability settings (#230).
func TestPostgres_AvailabilitySettings(t *testing.T) {
	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), nil))
	pg := Config.Postgres
	assert.Equal(t, "read-write", pg.TargetSessionAttrs)
	assert.Equal(t, []int{30000, 5000, 60000, 300},
		[]int{pg.StatementTimeoutMs, pg.LockTimeoutMs, pg.IdleInTransactionTimeoutMs, pg.MigrationLockTimeoutSeconds})

	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), map[string]string{
		"POSTGRES_HOSTS": "pg-1:5432, pg-2:5432,[2001:db8::1]:5433", "POSTGRES_TARGET_SESSION_ATTRS": "primary",
		"POSTGRES_STATEMENT_TIMEOUT_MS": "-1", "POSTGRES_LOCK_TIMEOUT_MS": "1000", "POSTGRES_IDLE_IN_TRANSACTION_TIMEOUT_MS": "2000",
		"POSTGRES_MIGRATION_LOCK_TIMEOUT_SECONDS": "60",
	}))
	pg = Config.Postgres
	assert.Equal(t, []string{"pg-1:5432", "pg-2:5432", "[2001:db8::1]:5433"}, pg.Hosts)
	assert.Equal(t, "primary", pg.TargetSessionAttrs)
	assert.Equal(t, []int{-1, 1000, 2000, 60},
		[]int{pg.StatementTimeoutMs, pg.LockTimeoutMs, pg.IdleInTransactionTimeoutMs, pg.MigrationLockTimeoutSeconds})

	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"host and hosts":      {map[string]string{"POSTGRES_HOSTS": "pg-1:5432"}, "postgres.host (POSTGRES_HOST) is set together with postgres.hosts"},
		"a host without port": {map[string]string{"POSTGRES_HOSTS": "pg-1"}, `entry 1 is "pg-1"`},
		"a bad port":          {map[string]string{"POSTGRES_HOSTS": "pg-1:5432,pg-2:99999"}, `entry 2 is "pg-2:99999"`},
		"unknown attrs":       {map[string]string{"POSTGRES_TARGET_SESSION_ATTRS": "writable"}, `postgres.target_session_attrs (POSTGRES_TARGET_SESSION_ATTRS) is "writable"`},
		"negative timeout":    {map[string]string{"POSTGRES_LOCK_TIMEOUT_MS": "-2"}, "postgres.lock_timeout_ms (POSTGRES_LOCK_TIMEOUT_MS) is -2"},
		"lock wait below 1":   {map[string]string{"POSTGRES_MIGRATION_LOCK_TIMEOUT_SECONDS": "-1"}, "postgres.migration_lock_timeout_seconds"},
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, loadFile(t, configFile("config.compose.yaml"), tc.env))
			err := ValidateServer()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), map[string]string{"POSTGRES_HOSTS": "pg-1:5432"}))
	Config.Postgres.Host, Config.Postgres.Port = "", "" // as in a file that sets only hosts
	err := ValidateServer()
	if err != nil {
		assert.NotContains(t, err.Error(), "postgres.", "hosts replaces host and port")
	}
}

// TestRedis_ModeSettings checks the defaults and the environment variables
// of the Redis modes and durable writes (#231).
func TestRedis_ModeSettings(t *testing.T) {
	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), nil))
	assert.Equal(t, RedisModeStandalone, Config.Redis.Mode)
	assert.Equal(t, 0, Config.Redis.DurableWrites.Replicas, "durable writes are off by default")
	assert.Equal(t, DefaultRedisDurableWritesTimeoutMs, Config.Redis.DurableWrites.TimeoutMs)

	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), map[string]string{
		"REDIS_MODE": "sentinel", "REDIS_SENTINEL_MASTER_NAME": "primary-1",
		"REDIS_SENTINEL_ADDRESSES": "s-1:26379, s-2:26379,,s-3:26379", "REDIS_SENTINEL_USERNAME": "watcher",
		"REDIS_SENTINEL_PASSWORD": "sentinel-secret", "REDIS_DURABLE_WRITES_REPLICAS": "1", "REDIS_DURABLE_WRITES_TIMEOUT_MS": "100",
	}))
	r := Config.Redis
	assert.Equal(t, RedisModeSentinel, r.Mode)
	assert.Equal(t, "primary-1", r.Sentinel.MasterName)
	assert.Equal(t, []string{"s-1:26379", "s-2:26379", "s-3:26379"}, r.Sentinel.Addresses)
	assert.Equal(t, []string{"watcher", "sentinel-secret"}, []string{r.Sentinel.Username, r.Sentinel.Password})
	assert.Equal(t, []int{1, 100}, []int{r.DurableWrites.Replicas, r.DurableWrites.TimeoutMs})

	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), map[string]string{
		"REDIS_MODE": "cluster", "REDIS_CLUSTER_ADDRESSES": "r-1:6379,r-2:6379",
	}))
	assert.Equal(t, []string{"r-1:6379", "r-2:6379"}, Config.Redis.Cluster.Addresses)
	Config.Redis.Host, Config.Redis.Port = "", "" // as in a file that lists only the cluster
	if err := ValidateServer(); err != nil {
		assert.NotContains(t, err.Error(), "redis.", "the cluster mode needs no host or port")
	}
}

// TestRabbitMQ_Hosts checks rabbitmq.hosts, its environment variable, its
// validation, and the nodes that the RabbitMQ service dials (#232).
func TestRabbitMQ_Hosts(t *testing.T) {
	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), nil))
	assert.Empty(t, Config.RabbitMQ.Hosts)
	nodes := RabbitMQNodes()
	require.Len(t, nodes, 1, "without hosts, the one node of host and port")
	assert.Equal(t, net.JoinHostPort(Config.RabbitMQ.Host, Config.RabbitMQ.Port), nodes[0].Address)
	assert.Equal(t, GetRabbitMQURL(), nodes[0].URL)

	require.NoError(t, loadFile(t, configFile("config.compose.yaml"), map[string]string{
		"RABBITMQ_HOSTS": "rmq-1:5672, rmq-2:5673,[2001:db8::1]:5672",
	}))
	assert.Equal(t, []string{"rmq-1:5672", "rmq-2:5673", "[2001:db8::1]:5672"}, Config.RabbitMQ.Hosts)
	Config.RabbitMQ.Host, Config.RabbitMQ.Port = "", "" // as in a file that sets only hosts
	if err := ValidateServer(); err != nil {
		assert.NotContains(t, err.Error(), "rabbitmq.host (", "hosts replaces host")
		assert.NotContains(t, err.Error(), "rabbitmq.port (", "hosts replaces port")
	}
	nodes = RabbitMQNodes()
	require.Len(t, nodes, 3)
	for i, want := range []struct{ host, address string }{
		{"rmq-1", "rmq-1:5672"}, {"rmq-2", "rmq-2:5673"}, {"2001:db8::1", "[2001:db8::1]:5672"},
	} {
		assert.Equal(t, want.host, nodes[i].Host, "the host names the TLS server")
		assert.Equal(t, want.address, nodes[i].Address)
		uri, err := amqp.ParseURI(nodes[i].URL)
		require.NoError(t, err)
		assert.Equal(t, want.host, uri.Host)
		assert.Equal(t, Config.RabbitMQ.Password, uri.Password, "every node uses the same account")
	}

	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"host and hosts":      {map[string]string{"RABBITMQ_HOSTS": "rmq-1:5672"}, "rabbitmq.host (RABBITMQ_HOST) is set together with rabbitmq.hosts"},
		"a host without port": {map[string]string{"RABBITMQ_HOSTS": "rmq-1"}, `entry 1 is "rmq-1"`},
		"a bad port":          {map[string]string{"RABBITMQ_HOSTS": "rmq-1:5672,rmq-2:0"}, `entry 2 is "rmq-2:0"`},
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, loadFile(t, configFile("config.compose.yaml"), tc.env))
			err := ValidateServer()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
