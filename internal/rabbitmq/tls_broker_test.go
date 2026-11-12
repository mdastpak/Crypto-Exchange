package rabbitmq

import (
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/testutil/tlsproxy"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rabbitMQThroughTLS points the configuration at a TLS proxy in front of
// the test broker, so that the service dials amqps (#186).
func rabbitMQThroughTLS(t *testing.T) *tlsproxy.Proxy {
	t.Helper()
	probe, err := amqp.Dial(brokerTestURL)
	if err != nil {
		t.Skipf("RabbitMQ is not reachable at localhost:5672: %v", err)
	}
	probe.Close()
	proxy := tlsproxy.New(t, "localhost:5672")
	t.Cleanup(withRabbitMQConfig(proxy.Host(), proxy.Port(), "guest", "guest"))
	config.Config.RabbitMQ.TLS = config.ClientTLS{Enabled: true, CAFile: proxy.CAFile, ServerName: proxy.ServerName}
	return proxy
}

func TestNewRabbitMQService_TLS(t *testing.T) {
	rabbitMQThroughTLS(t)
	exchange := "test_tls_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	svc, err := NewRabbitMQService(&config.Config, exchange)
	require.NoError(t, err)
	defer func() {
		svc.Close()
		withBrokerChannel(t, func(ch *amqp.Channel) { _ = ch.ExchangeDelete(exchange, false, false) })
	}()
	assert.True(t, strings.HasPrefix(svc.nodes[0].url, "amqps://"))
	require.NoError(t, publishWithTimeout(svc, exchange, `{"over":"tls"}`), "a publish is confirmed over TLS")
}

// TestNewRabbitMQService_TLS_RefusesAnUntrustedCertificate checks that a
// certificate that is not trusted never connects: the service starts
// without the broker (#232) and reports the certificate as the reason.
func TestNewRabbitMQService_TLS_RefusesAnUntrustedCertificate(t *testing.T) {
	rabbitMQThroughTLS(t)
	config.Config.RabbitMQ.TLS.CAFile = ""
	svc, err := NewRabbitMQService(&config.Config)
	require.NoError(t, err)
	defer svc.Close()
	assert.False(t, svc.IsHealthy())
	require.Error(t, svc.LastError())
	assert.Contains(t, svc.LastError().Error(), "certificate")
}
