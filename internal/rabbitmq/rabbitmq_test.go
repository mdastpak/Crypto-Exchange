package rabbitmq

import (
	"context"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// No RabbitMQ broker is available in this environment, so these tests focus
// on failure paths and logic that does not require a live connection:
// connection-failure handling in NewRabbitMQService, IsHealthy's state
// checks, and Publish/PublishWithRetry/Close behavior on a service that
// never established (or has torn down) its connection.

func withRabbitMQConfig(host, port, username, password string) func() {
	original := config.Config.RabbitMQ
	config.Config.RabbitMQ.Host = host
	config.Config.RabbitMQ.Port = port
	config.Config.RabbitMQ.Username = username
	config.Config.RabbitMQ.Password = password
	return func() { config.Config.RabbitMQ = original }
}

// TestNewRabbitMQService_StartsWithoutTheBroker starts the service while no
// node can be reached (#232): it starts, unhealthy, keeps the error of the
// attempt, fails a publish within its context, and closes at once.
func TestNewRabbitMQService_StartsWithoutTheBroker(t *testing.T) {
	for name, hostPort := range map[string][2]string{
		"unknown host":    {"no-such-rabbitmq-host-xyz", "5672"},
		"nothing listens": {"127.0.0.1", "1"},
	} {
		t.Run(name, func(t *testing.T) {
			restore := withRabbitMQConfig(hostPort[0], hostPort[1], "guest", "guest")
			defer restore()

			svc, err := NewRabbitMQService()
			require.NoError(t, err)
			require.NotNil(t, svc)
			assert.False(t, svc.IsHealthy())
			assert.Empty(t, svc.ConnectedTo())
			require.Error(t, svc.LastError())
			assert.Contains(t, svc.LastError().Error(), hostPort[0], "the error names the node")

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			assert.Error(t, svc.Publish(ctx, "x", "{}"))
			_, err = svc.OpenChannel()
			assert.Error(t, err)

			// Close waits for a dial in progress, which dialTimeout bounds: a
			// slow resolver can take that long to give up on the unknown
			// host (#342).
			started := time.Now()
			require.NoError(t, svc.Close())
			assert.Less(t, time.Since(started), dialTimeout+time.Second, "Close stops the background connection")
		})
	}
}

// TestNewRabbitMQService_RefusesSettingsThatCannotWork checks that a TLS
// setting that cannot load still stops the startup.
func TestNewRabbitMQService_RefusesSettingsThatCannotWork(t *testing.T) {
	restore := withRabbitMQConfig("127.0.0.1", "1", "guest", "guest")
	defer restore()
	config.Config.RabbitMQ.TLS = config.ClientTLS{Enabled: true, CAFile: "/does/not/exist.pem"}
	_, err := NewRabbitMQService()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rabbitmq.tls")
}

func TestRabbitMQService_IsHealthy_ZeroValue(t *testing.T) {
	r := &RabbitMQService{}
	assert.False(t, r.IsHealthy(), "a service with no connection should report unhealthy")
}

func TestRabbitMQService_IsHealthy_ClosedService(t *testing.T) {
	r := &RabbitMQService{}
	r.closed.Store(true)
	assert.False(t, r.IsHealthy())
}

func TestRabbitMQService_Close_NeverConnected(t *testing.T) {
	r := &RabbitMQService{}
	err := r.Close()
	assert.NoError(t, err)
	assert.True(t, r.closed.Load())
	assert.False(t, r.IsHealthy())
}

func TestRabbitMQService_Close_Idempotent(t *testing.T) {
	r := &RabbitMQService{}
	require.NoError(t, r.Close())
	// Calling Close a second time must not panic or block, even though the
	// reconnect loop was never started for this zero-value service.
	assert.NoError(t, r.Close())
}

func TestRabbitMQService_Publish_ClosedService(t *testing.T) {
	r := &RabbitMQService{}
	r.closed.Store(true)

	err := r.Publish(context.Background(), "some-exchange", "payload")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "rabbitmq service closed")
}

func TestRabbitMQService_Publish_WithoutChannelFails(t *testing.T) {
	r := &RabbitMQService{}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	err := r.Publish(ctx, "some-exchange", "payload")
	assert.Error(t, err, "publishing without any connection or channel should fail, not panic")
	assert.Less(t, time.Since(start), 500*time.Millisecond,
		"a service that never connected has no supervisor to wait for, so it must fail at once")
}

func TestRabbitMQService_PublishWithRetry_ClosedServiceExhaustsRetries(t *testing.T) {
	r := &RabbitMQService{}
	r.closed.Store(true)

	err := r.PublishWithRetry(context.Background(), "some-exchange", "payload", 2)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to publish message after 2 attempts")
}

func TestRabbitMQService_PublishWithRetry_ContextCancelled(t *testing.T) {
	r := &RabbitMQService{}
	r.closed.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := r.PublishWithRetry(ctx, "some-exchange", "payload", 3)
	assert.Error(t, err)
}
