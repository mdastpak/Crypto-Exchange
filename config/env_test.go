package config

import (
	"bufio"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var envAssignment = regexp.MustCompile(`^(#\s*)?([A-Z][A-Z0-9_]*)=(.*)$`)

// envExample returns the assignments in .env.example, and the names of the
// ones that are commented out.
func envExample(t *testing.T) (map[string]string, []string) {
	t.Helper()
	file, err := os.Open(filepath.Join("..", ".env.example"))
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
	doc, err := os.ReadFile(filepath.Join("..", "docs", "configuration.md"))
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
	err := loadFile(t, "config.compose.yaml", map[string]string{"SERVER_MODE": "production"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `server.mode (SERVER_MODE) is "production"`)

	require.NoError(t, loadFile(t, "config.compose.yaml", map[string]string{"GIN_MODE": "release"}))
	assert.Equal(t, "release", Config.Server.Mode)
}

func TestLoad_ReportsInvalidEnvironmentVariables(t *testing.T) {
	err := loadFile(t, "config.compose.yaml", map[string]string{"POSTGRES_MAX_OPEN_CONNS": "many"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "POSTGRES_MAX_OPEN_CONNS")
}

func TestKeyPath_AcceptsAStringOrPathSegments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
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
	data, err := os.ReadFile("config.example.yaml")
	require.NoError(t, err)
	assert.NotRegexp(t, `(?m)^[^#]*:\s*"?\$\{`, string(data), "placeholders are not expanded, so values must not use them")

	err = loadFile(t, "config.example.yaml", nil)
	require.NoError(t, err)
	err = ValidateServer()
	require.Error(t, err, "RabbitMQ credentials come from the environment")
	assert.Contains(t, err.Error(), "rabbitmq.username (RABBITMQ_USER)")

	require.NoError(t, loadFile(t, "config.example.yaml", map[string]string{
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
	require.NoError(t, loadFile(t, "config.compose.yaml", values))
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
	require.NoError(t, loadFile(t, "config.compose.yaml", nil))
	limits := Config.RateLimits
	assert.Equal(t, RateLimitAllow, limits.REST.OnError, "the API stays available while Redis is unreachable")
	assert.Equal(t, RateLimitAllow, limits.WS.OnError)
	assert.Equal(t, RateLimitAllow, limits.Trading.OnError)
	assert.Equal(t, RateLimitDeny, limits.Auth.OnError, "sign-in limits fail closed")

	require.NoError(t, loadFile(t, "config.compose.yaml", map[string]string{"RATE_LIMITS_REST_ON_ERROR": "deny", "RATE_LIMITS_AUTH_ON_ERROR": "allow"}))
	assert.Equal(t, RateLimitDeny, Config.RateLimits.REST.OnError)
	assert.Equal(t, RateLimitAllow, Config.RateLimits.Auth.OnError)

	err := loadFile(t, "config.compose.yaml", map[string]string{"RATE_LIMITS_WS_ON_ERROR": "ignore"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `rate_limits.ws.on_error (RATE_LIMITS_WS_ON_ERROR) is "ignore"`)
}

func TestRedisTimeouts_DefaultsAndValidation(t *testing.T) {
	require.NoError(t, loadFile(t, "config.compose.yaml", nil))
	assert.Equal(t, DefaultRedisDialTimeoutMs, Config.Redis.DialTimeoutMs)
	assert.Equal(t, DefaultRedisReadTimeoutMs, Config.Redis.ReadTimeoutMs)
	assert.Equal(t, DefaultRedisWriteTimeoutMs, Config.Redis.WriteTimeoutMs)
	assert.Equal(t, DefaultRedisPoolTimeoutMs, Config.Redis.PoolTimeoutMs)
	assert.Equal(t, DefaultRedisMaxRetries, Config.Redis.MaxRetries)
	assert.Equal(t, DefaultRateLimitTimeoutMs, Config.RateLimits.TimeoutMs)

	require.NoError(t, loadFile(t, "config.compose.yaml", map[string]string{
		"REDIS_DIAL_TIMEOUT_MS": "300", "REDIS_READ_TIMEOUT_MS": "400", "REDIS_WRITE_TIMEOUT_MS": "500",
		"REDIS_POOL_TIMEOUT_MS": "600", "REDIS_MAX_RETRIES": "-1", "RATE_LIMITS_TIMEOUT_MS": "50",
	}))
	assert.Equal(t, 300, Config.Redis.DialTimeoutMs)
	assert.Equal(t, 400, Config.Redis.ReadTimeoutMs)
	assert.Equal(t, 500, Config.Redis.WriteTimeoutMs)
	assert.Equal(t, 600, Config.Redis.PoolTimeoutMs)
	assert.Equal(t, -1, Config.Redis.MaxRetries, "-1 turns retries off")
	assert.Equal(t, 50, Config.RateLimits.TimeoutMs)

	err := loadFile(t, "config.compose.yaml", map[string]string{"RATE_LIMITS_TIMEOUT_MS": "-5"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rate_limits.timeout_ms (RATE_LIMITS_TIMEOUT_MS) is -5")
	err = loadFile(t, "config.compose.yaml", map[string]string{"REDIS_READ_TIMEOUT_MS": "-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redis.read_timeout_ms (REDIS_READ_TIMEOUT_MS) is -1")
	err = loadFile(t, "config.compose.yaml", map[string]string{"REDIS_MAX_RETRIES": "-2"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redis.max_retries (REDIS_MAX_RETRIES) is -2")
}

func TestMilliseconds_FallsBackWhenNotSet(t *testing.T) {
	assert.Equal(t, 300*time.Millisecond, Milliseconds(300, 1000))
	assert.Equal(t, time.Second, Milliseconds(0, 1000), "a configuration that Load did not fill in")
}

func TestKeys_DefaultsAndValidation(t *testing.T) {
	require.NoError(t, loadFile(t, "config.compose.yaml", nil))
	assert.Equal(t, "", Config.Keys.Provider, "the data-protection keys are off unless configured")
	assert.Equal(t, "transit", Config.Keys.Transit.Mount)
	assert.Equal(t, 10, Config.Keys.Transit.Timeout)

	require.NoError(t, loadFile(t, "config.compose.yaml", map[string]string{
		"KEYS_PROVIDER": "transit", "VAULT_ADDR": "https://vault:8200", "VAULT_TOKEN": "s.token", "VAULT_CACERT": "/run/ca.pem",
		"KEYS_TRANSIT_TOKEN_FILE": "/run/token", "KEYS_TRANSIT_MOUNT": "kms", "KEYS_TRANSIT_KEY": "data-keys", "KEYS_TRANSIT_TIMEOUT_SECONDS": "3",
	}))
	transit := Config.Keys.Transit
	assert.Equal(t, KeysProviderTransit, Config.Keys.Provider)
	assert.Equal(t, []string{"https://vault:8200", "s.token", "/run/ca.pem", "/run/token", "kms", "data-keys"},
		[]string{transit.Address, transit.Token, transit.CACert, transit.TokenFile, transit.Mount, transit.Key})
	assert.Equal(t, 3, transit.Timeout)

	require.NoError(t, loadFile(t, "config.compose.yaml", map[string]string{"KEYS_PROVIDER": "local", "KEYS_LOCAL_KEK": "a2V5"}))
	assert.Equal(t, "a2V5", Config.Keys.Local.KEK)

	err := loadFile(t, "config.compose.yaml", map[string]string{"KEYS_PROVIDER": "aws"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `keys.provider (KEYS_PROVIDER) is "aws"`)
	err = loadFile(t, "config.compose.yaml", map[string]string{"KEYS_TRANSIT_TIMEOUT_SECONDS": "-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keys.transit.timeout_seconds (KEYS_TRANSIT_TIMEOUT_SECONDS) is -1")
}
