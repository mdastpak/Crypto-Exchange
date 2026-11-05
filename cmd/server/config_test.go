package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadExample loads config/config.example.yaml with only the given
// environment variables set among the ones the configuration reads.
func loadExample(t *testing.T, env map[string]string) {
	t.Helper()
	saved, savedPath := config.Config, config.ConfigPath()
	t.Cleanup(func() {
		config.Config = saved
		config.SetConfigPath(savedPath)
	})
	for _, v := range config.EnvVars() {
		t.Setenv(v.Name, env[v.Name])
	}
	config.SetConfigPath(filepath.Join("..", "..", "config", "config.example.yaml"))
	require.NoError(t, config.Load())
}

// TestValidateConfiguration_ExampleInReleaseMode starts the example
// configuration in release mode, which must fail with one line per problem,
// each naming the setting and its environment variable.
func TestValidateConfiguration_ExampleInReleaseMode(t *testing.T) {
	loadExample(t, map[string]string{"SERVER_MODE": "release"})

	_, err := validateConfiguration()
	require.Error(t, err)
	list := problemList(err)
	want := []string{
		"  - required settings are not set: rabbitmq.username (RABBITMQ_USER), rabbitmq.password (RABBITMQ_PASSWORD)",
		"  - server.tls.enabled (SERVER_TLS_ENABLED) is false; release mode needs TLS: enable it, or set server.tls.behind_proxy (SERVER_TLS_BEHIND_PROXY)",
		`  - postgres.sslmode (POSTGRES_SSLMODE) is "disable"; release mode needs one of require, verify-ca, verify-full`,
		"  - postgres.password (POSTGRES_PASSWORD) is empty or the default",
		"  - redis.password (REDIS_PASSWORD) is empty",
		"  - otp.auth.secret (or OTP_SECRET) is required in release mode",
	}
	for _, line := range want {
		assert.Contains(t, list, line)
	}
	assert.Len(t, strings.Split(list, "\n"), len(want), "nothing else is reported:\n%s", list)
}

func TestValidateConfiguration_ExampleInDebugMode(t *testing.T) {
	loadExample(t, map[string]string{"RABBITMQ_USER": "guest", "RABBITMQ_PASSWORD": "guest"})

	provider, err := validateConfiguration()
	require.NoError(t, err, "the example starts a local server")
	assert.Nil(t, provider, "the example configures no key provider")
}

func TestValidateConfiguration_ReportsTheChecksOfEveryPackage(t *testing.T) {
	loadExample(t, map[string]string{
		"SERVER_MODE": "release", "OTP_SECRET": "short", "KEYS_PROVIDER": "local",
		"KEYS_LOCAL_KEK": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	})

	_, err := validateConfiguration()
	require.Error(t, err)
	list := problemList(err)
	assert.Contains(t, list, "  - otp.auth.secret must be at least 32 bytes")
	assert.Contains(t, list, `  - keys.provider "local" keeps the key-encryption key in the configuration and is refused in release mode`)
	assert.Contains(t, list, "  - server.tls.enabled (SERVER_TLS_ENABLED) is false", "the configuration problems come in the same list")
}
