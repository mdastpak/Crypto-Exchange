package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/dberr"
	"github.com/mdastpak/crypto-exchange/internal/handlers/system"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"
	"github.com/mdastpak/crypto-exchange/internal/testutil/tcpproxy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServer_StartsWithoutRabbitMQ starts the server while RabbitMQ is
// down, as during a broker outage in a rolling deploy (#232). It starts,
// stays ready with RabbitMQ reported as down, serves the routes that do not
// need the broker, and answers a sign-in code request with 503
// delivery_unavailable at once. Once the broker is back, it connects on its
// own, and the same request queues the code.
func TestServer_StartsWithoutRabbitMQ(t *testing.T) {
	skipUnlessReachable(t)
	proxy, err := tcpproxy.New("localhost:5672")
	require.NoError(t, err)
	t.Cleanup(proxy.Close)
	proxy.SetDown(true)
	env := newE2EEnv(t, func(c *config.Configuration) {
		c.RabbitMQ.Host, c.RabbitMQ.Port = proxy.Host(), proxy.Port()
	})
	broker := env.handlers.Socket.RabbitMQService.(*rabbitmq.RabbitMQService)
	require.False(t, broker.IsHealthy())

	// Sign-in codes go through the broker, as in main.
	sender, _ := newOTPDelivery(notify.Channels{Email: deliverNothing{}}, broker, nil)
	env.handlers.Public.OTPSender = sender

	ready := func() (int, system.HealthStatus) {
		status, body := env.call(t, http.MethodGet, "/system/ready", nil, "")
		var health system.HealthStatus
		require.NoError(t, json.Unmarshal(body, &health), string(body))
		return status, health
	}
	status, health := ready()
	assert.Equal(t, http.StatusOK, status, "the instance serves without the broker")
	assert.Equal(t, "degraded", health.Status)
	assert.Equal(t, "error", health.Details["rabbitmq"].Status)
	assert.Equal(t, "ok", health.Details["postgres"].Status)

	status, _ = env.call(t, http.MethodGet, "/api/v1/public/currencies", nil, "")
	assert.Equal(t, http.StatusOK, status)

	started := time.Now()
	status, body := env.call(t, http.MethodPost, "/api/v1/public/auth", map[string]string{"username": "outage@example.com"}, "")
	assert.Equal(t, http.StatusServiceUnavailable, status, string(body))
	assert.Less(t, time.Since(started), 2*time.Second, "a known outage is answered at once")
	var resp e2eResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.Equal(t, dberr.CodeDeliveryUnavailable, resp.Code)

	proxy.SetDown(false)
	require.Eventually(t, func() bool {
		status, health := ready()
		return status == http.StatusOK && health.Status == "ok"
	}, 30*time.Second, 100*time.Millisecond, "the server connects once the broker is back")
	status, body = env.call(t, http.MethodPost, "/api/v1/public/auth", map[string]string{"username": "outage@example.com"}, "")
	assert.Equal(t, http.StatusOK, status, string(body))
}

// deliverNothing is a channel that the test never reaches: no worker runs.
type deliverNothing struct{}

func (deliverNothing) Deliver(context.Context, notify.OTPMessage) error { return nil }
