package socket

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"encrypted-db/config"
	"encrypted-db/internal/models"
	"encrypted-db/internal/rabbitmq"

	"github.com/google/uuid"
	"github.com/streadway/amqp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleCurrencyUpdate_BroadcastsWithServerTime(t *testing.T) {
	h := newTestHandler()
	client := newTestClient()
	h.Mu.Lock()
	h.Clients[client] = true
	h.Mu.Unlock()

	h.handleCurrencyUpdate([]byte(`{"hk":"abc","action":"update"}`))

	select {
	case got := <-client.Send:
		var msg map[string]interface{}
		require.NoError(t, json.Unmarshal(got, &msg))
		assert.Equal(t, "abc", msg["hk"])
		assert.Equal(t, "update", msg["action"])
		assert.NotEmpty(t, msg["server_time"])
	default:
		t.Fatal("expected the currency update to be broadcast to the client")
	}
}

func TestHandleCurrencyUpdate_InvalidJSONDoesNotBroadcast(t *testing.T) {
	h := newTestHandler()
	client := newTestClient()
	h.Mu.Lock()
	h.Clients[client] = true
	h.Mu.Unlock()

	assert.NotPanics(t, func() {
		h.handleCurrencyUpdate([]byte("not-json"))
	})
	assert.Empty(t, client.Send)
}

func TestStartConsumer_FailsWithoutBroker(t *testing.T) {
	// A zero RabbitMQService has no connection, so subscribing fails at once.
	h := newTestHandler()
	assert.Error(t, h.StartConsumer(context.Background()), "startup must fail rather than run without events")

	assert.Error(t, (&WebSocketHandler{}).StartConsumer(context.Background()))
}

// The tests below need a real broker and are skipped when none is reachable.
const brokerTestURL = "amqp://guest:guest@localhost:5672/"

// newBrokerHandler returns a handler with its own connection to the test
// broker, like one server instance, and a registered client.
func newBrokerHandler(t *testing.T) (*WebSocketHandler, *Client) {
	t.Helper()
	svc, err := rabbitmq.NewRabbitMQService(config.Config.RabbitMQ.Exchanges.PriceUpdates)
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })

	h := NewWebSocketHandler(&models.InfraServices{RabbitMQ: svc})
	client := newTestClient()
	h.Mu.Lock()
	h.Clients[client] = true
	h.Mu.Unlock()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(t, h.Shutdown(ctx))
	})
	return h, client
}

// withTestBroker points the configuration at the test broker with a fresh
// price updates exchange, which is removed when the test ends.
func withTestBroker(t *testing.T) string {
	t.Helper()
	probe, err := amqp.Dial(brokerTestURL)
	if err != nil {
		t.Skipf("RabbitMQ is not reachable at localhost:5672: %v", err)
	}
	probe.Close()

	saved := config.Config.RabbitMQ
	t.Cleanup(func() { config.Config.RabbitMQ = saved })
	config.Config.RabbitMQ.Host, config.Config.RabbitMQ.Port = "localhost", "5672"
	config.Config.RabbitMQ.Username, config.Config.RabbitMQ.Password = "guest", "guest"
	exchange := "test_ws_events_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	config.Config.RabbitMQ.Exchanges.PriceUpdates = exchange
	t.Cleanup(func() {
		if conn, err := amqp.Dial(brokerTestURL); err == nil {
			if ch, err := conn.Channel(); err == nil {
				_ = ch.ExchangeDelete(exchange, false, false)
			}
			conn.Close()
		}
	})
	return exchange
}

func receive(t *testing.T, client *Client) map[string]interface{} {
	t.Helper()
	select {
	case data := <-client.Send:
		var msg map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &msg))
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("no event reached the client")
		return nil
	}
}

func TestConsumer_Broker_EveryInstanceReceivesEveryEvent(t *testing.T) {
	exchange := withTestBroker(t)
	first, firstClient := newBrokerHandler(t)
	second, secondClient := newBrokerHandler(t)
	require.NoError(t, first.StartConsumer(context.Background()))
	require.NoError(t, second.StartConsumer(context.Background()))

	// StartConsumer subscribes before it returns, so events published right
	// away are not lost.
	publisher := first.RabbitMQService
	for _, action := range []string{"create", "update", "delete"} {
		require.NoError(t, publisher.Publish(context.Background(), exchange,
			`{"entity":"currencies","action":"`+action+`","hk":"0d4f2a7e-8c1b-4f0e-9a51-3c2f6f1b7d10"}`))
	}

	for name, client := range map[string]*Client{"first instance": firstClient, "second instance": secondClient} {
		for _, action := range []string{"create", "update", "delete"} {
			msg := receive(t, client)
			assert.Equal(t, action, msg["action"], name)
			assert.Equal(t, "currencies", msg["entity"], name)
			assert.NotEmpty(t, msg["server_time"], name)
		}
		select {
		case extra := <-client.Send:
			t.Errorf("%s received an unexpected extra message: %s", name, extra)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func TestConsumer_Broker_ShutdownStopsTheConsumer(t *testing.T) {
	withTestBroker(t)
	svc, err := rabbitmq.NewRabbitMQService(config.Config.RabbitMQ.Exchanges.PriceUpdates)
	require.NoError(t, err)
	defer svc.Close()
	h := NewWebSocketHandler(&models.InfraServices{RabbitMQ: svc})
	require.NoError(t, h.StartConsumer(context.Background()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	assert.NoError(t, h.Shutdown(ctx), "Shutdown must stop the consumer promptly")
}
