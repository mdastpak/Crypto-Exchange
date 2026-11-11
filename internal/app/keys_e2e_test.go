package app

import (
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func localKeysConfig(t *testing.T) config.KeysConfig {
	t.Helper()
	kek := make([]byte, keys.KeySize)
	_, err := rand.Read(kek)
	require.NoError(t, err)
	var cfg config.KeysConfig
	cfg.Provider = config.KeysProviderLocal
	cfg.Local.KEK = base64.StdEncoding.EncodeToString(kek)
	return cfg
}

func TestDataKeyProvider(t *testing.T) {
	saved := config.Config.Keys
	t.Cleanup(func() { config.Config.Keys = saved })

	config.Config.Keys = config.KeysConfig{}
	provider, err := dataKeyProvider(false)
	require.NoError(t, err)
	assert.Nil(t, provider, "without keys.provider, no keys are loaded")
	_, err = dataKeyProvider(true)
	assert.ErrorIs(t, err, errKeysProviderRequired, "release mode needs the data keys")

	config.Config.Keys = localKeysConfig(t)
	provider, err = dataKeyProvider(false)
	require.NoError(t, err)
	assert.True(t, provider.Development())
	_, err = dataKeyProvider(true)
	assert.ErrorIs(t, err, keys.ErrDevelopmentProvider, "release mode refuses the local provider")

	config.Config.Keys = config.KeysConfig{Provider: config.KeysProviderTransit}
	config.Config.Keys.Transit.Address, config.Config.Keys.Transit.Token, config.Config.Keys.Transit.Key = "http://vault:8200", "t", "k"
	_, err = dataKeyProvider(true)
	assert.ErrorIs(t, err, keys.ErrPlainHTTP, "release mode requires TLS to the key service")
	config.Config.Keys.Transit.Address = "https://vault:8200"
	provider, err = dataKeyProvider(true)
	require.NoError(t, err)
	assert.Equal(t, "transit:transit/k", provider.ID())

	config.Config.Keys.Transit.Key = ""
	_, err = dataKeyProvider(false)
	assert.ErrorContains(t, err, "KEYS_TRANSIT_KEY")
}

// TestLoadDataKeys_EndToEnd loads the keys as the server does at startup,
// against the database that the migrations build.
func TestLoadDataKeys_EndToEnd(t *testing.T) {
	env := newE2EEnv(t, withoutDataKeys)
	services := &models.InfraServices{Postgres: &db.PostgresService{DB: env.db}}

	ring, err := loadDataKeys(services, nil)
	require.NoError(t, err)
	assert.Nil(t, ring)

	cfg := localKeysConfig(t)
	provider, err := keys.FromConfig(cfg)
	require.NoError(t, err)
	ring, err = loadDataKeys(services, provider)
	require.NoError(t, err)
	for _, purpose := range keys.Purposes {
		key, err := ring.Current(purpose)
		require.NoError(t, err)
		assert.Equal(t, 1, key.Version(), purpose)
	}

	other := localKeysConfig(t)
	wrongKEK, err := keys.FromConfig(other)
	require.NoError(t, err)
	_, err = loadDataKeys(services, wrongKEK)
	assert.ErrorContains(t, err, "was wrapped by the key-encryption key", "a server with another KEK refuses to start")
}
