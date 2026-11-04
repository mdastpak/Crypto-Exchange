package rabbitmq

import (
	"context"
	"testing"
	"time"

	"encrypted-db/config"

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

func TestNewRabbitMQService_FailsWithBadHost(t *testing.T) {
	restore := withRabbitMQConfig("no-such-rabbitmq-host-xyz", "5672", "guest", "guest")
	defer restore()

	svc, err := NewRabbitMQService()
	assert.Error(t, err)
	assert.Nil(t, svc)
}

func TestNewRabbitMQService_FailsWithUnreachablePort(t *testing.T) {
	// Port 1 should have nothing listening and refuse the connection quickly.
	restore := withRabbitMQConfig("127.0.0.1", "1", "guest", "guest")
	defer restore()

	svc, err := NewRabbitMQService()
	assert.Error(t, err)
	assert.Nil(t, svc)
	assert.Contains(t, err.Error(), "failed to connect to RabbitMQ")
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

	err := r.Publish(ctx, "some-exchange", "payload")
	assert.Error(t, err, "publishing without any connection or channel should fail, not panic")
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
