package db

import (
	"context"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/testutil/redistest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/tlsproxy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// redisThroughTLS points the configuration at a TLS proxy in front of the
// test Redis server (#186).
func redisThroughTLS(t *testing.T) *tlsproxy.Proxy {
	t.Helper()
	saved := config.Config
	t.Cleanup(func() { config.Config = saved })
	proxy := tlsproxy.New(t, redistest.Addr)
	config.Config.Redis.Host, config.Config.Redis.Port = proxy.Host(), proxy.Port()
	config.Config.Redis.DB = redistest.DB
	config.Config.Redis.ConnectRetries, config.Config.Redis.ConnectRetryDelayMs = 1, 10
	config.Config.Redis.TLS = config.ClientTLS{Enabled: true, CAFile: proxy.CAFile, ServerName: proxy.ServerName}
	return proxy
}

func TestNewRedisService_TLS(t *testing.T) {
	redisThroughTLS(t)
	svc, err := NewRedisService()
	require.NoError(t, err)
	defer svc.Close()
	assert.NoError(t, svc.Ping(context.Background()))
	assert.NotNil(t, svc.Client.Options().TLSConfig)
}

func TestNewRedisService_TLS_RefusesAnUntrustedCertificate(t *testing.T) {
	redisThroughTLS(t)
	config.Config.Redis.TLS.CAFile = "" // the system authorities do not know the test CA
	_, err := NewRedisService()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "certificate")
}

func TestNewRedisService_TLS_RefusesTheWrongName(t *testing.T) {
	redisThroughTLS(t)
	config.Config.Redis.TLS.ServerName = "" // the certificate does not hold 127.0.0.1
	_, err := NewRedisService()
	require.Error(t, err)
}

func TestNewRedisService_TLS_ReportsAnUnusableCAFile(t *testing.T) {
	redisThroughTLS(t)
	config.Config.Redis.TLS.CAFile = "/does/not/exist.pem"
	_, err := NewRedisService()
	require.ErrorContains(t, err, "redis.tls")
}
