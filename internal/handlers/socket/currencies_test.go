package socket

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/config"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"
	"github.com/mdastpak/crypto-exchange/internal/testutil/tcpproxy"

	"github.com/google/uuid"
	"github.com/streadway/amqp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testHK = "0d4f2a7e-8c1b-4f0e-9a51-3c2f6f1b7d10"

// testEvent is a valid event with the given action, as the admin handler
// publishes it.
func testEvent(action string) string {
	info := `{"en_name":"Tether","fa_name":"تتر","if_fiat":false,"symbol":"USDT","is_token":true,"contract":"0xdac17f958d2ee523a2206206994597c13d831ec7"}`
	if action == "delete" {
		info = `{"status":"suspend"}`
	}
	return `{"entity":"currencies","action":"` + action + `","hk":"` + testHK + `","info":` + info + `,"server_time":"2026-11-05T10:00:00Z"}`
}

func registeredClient(h *WebSocketHandler) *Client {
	client := newTestClient()
	h.Mu.Lock()
	h.Clients[client] = true
	h.Mu.Unlock()
	return client
}

func TestHandleCurrencyUpdate_BroadcastsValidEventsWithServerTime(t *testing.T) {
	h := newTestHandler()
	client := registeredClient(h)
	now := time.Now().UTC()

	for _, action := range []string{"create", "update", "delete"} {
		h.handleCurrencyUpdate([]byte(testEvent(action)))

		select {
		case got := <-client.Send:
			var msg map[string]interface{}
			require.NoError(t, json.Unmarshal(got, &msg))
			assert.Equal(t, "currencies", msg["entity"], action)
			assert.Equal(t, action, msg["action"])
			assert.Equal(t, testHK, msg["hk"])
			serverTime, err := time.Parse(time.RFC3339, msg["server_time"].(string))
			require.NoError(t, err)
			assert.WithinDuration(t, now, serverTime, 5*time.Second, "the gateway sets its own time, not the publisher's")
			info := msg["info"].(map[string]interface{})
			if action == "delete" {
				assert.Equal(t, map[string]interface{}{"status": "suspend"}, info)
			} else {
				assert.Equal(t, "USDT", info["symbol"])
				assert.Equal(t, "تتر", info["fa_name"])
				assert.Equal(t, true, info["is_token"])
			}
		default:
			t.Fatalf("expected the %s event to be broadcast", action)
		}
	}
}

func TestSanitizeCurrencyEvent_DropsUnknownFields(t *testing.T) {
	extended := `{"entity":"currencies","action":"create","hk":"` + testHK + `",` +
		`"info":{"symbol":"USDT","en_name":"Tether","script":"<script>alert(1)</script>","price":{"usd":1}},` +
		`"redirect":"https://phishing.example","admin":true,"server_time":"1999-01-01T00:00:00Z"}`

	out, err := sanitizeCurrencyEvent([]byte(extended), time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	var msg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &msg))
	assert.ElementsMatch(t, []string{"entity", "action", "hk", "info", "server_time"}, keysOf(msg), "only the fields of the schema are sent")
	assert.JSONEq(t, `"2026-11-05T10:00:00Z"`, string(msg["server_time"]))
	var info map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(msg["info"], &info))
	assert.ElementsMatch(t, []string{"en_name", "fa_name", "if_fiat", "symbol", "is_token", "contract"}, keysOf(info))
	assert.NotContains(t, string(out), "script")
	assert.NotContains(t, string(out), "phishing")

	out, err = sanitizeCurrencyEvent([]byte(`{"entity":"currencies","action":"delete","hk":"`+testHK+`","info":{"status":"deleted","symbol":"X"}}`), time.Now())
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(out, &msg))
	assert.JSONEq(t, `{"status":"deleted"}`, string(msg["info"]))
}

func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestSanitizeCurrencyEvent_RejectsInvalidEvents(t *testing.T) {
	valid := testEvent("update")
	for name, body := range map[string]string{
		"not JSON":                "not-json",
		"a JSON array":            `[` + valid + `]`,
		"null":                    `null`,
		"trailing data":           valid + `{}`,
		"unknown entity":          strings.Replace(valid, `"currencies"`, `"users"`, 1),
		"no entity":               strings.Replace(valid, `"entity":"currencies",`, ``, 1),
		"unknown action":          strings.Replace(valid, `"update"`, `"drop"`, 1),
		"no hk":                   strings.Replace(valid, `"hk":"`+testHK+`",`, ``, 1),
		"hk not a UUID":           strings.Replace(valid, testHK, `abc`, 1),
		"hk in braces":            strings.Replace(valid, testHK, `{`+testHK+`}`, 1),
		"hk in upper case":        strings.Replace(valid, testHK, strings.ToUpper(testHK), 1),
		"no info":                 `{"entity":"currencies","action":"create","hk":"` + testHK + `"}`,
		"info is a string":        `{"entity":"currencies","action":"create","hk":"` + testHK + `","info":"USDT"}`,
		"info of the wrong types": `{"entity":"currencies","action":"create","hk":"` + testHK + `","info":{"symbol":42}}`,
		"delete without a status": `{"entity":"currencies","action":"delete","hk":"` + testHK + `","info":{}}`,
		"delete as approved":      `{"entity":"currencies","action":"delete","hk":"` + testHK + `","info":{"status":"approved"}}`,
		"too large":               strings.Replace(valid, `"Tether"`, `"`+strings.Repeat("x", maxEventBytes)+`"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			out, err := sanitizeCurrencyEvent([]byte(body), time.Now())
			assert.Error(t, err)
			assert.Nil(t, out)
		})
	}

	h := newTestHandler()
	client := registeredClient(h)
	assert.NotPanics(t, func() { h.handleCurrencyUpdate([]byte(strings.Replace(valid, `"update"`, `"drop"`, 1))) })
	assert.Empty(t, client.Send, "a rejected event is not broadcast")
}

func TestQuoted_ShortensAndEscapesValues(t *testing.T) {
	assert.Equal(t, `"drop"`, quoted("drop"))
	assert.Equal(t, `"line\nforged"`, quoted("line\nforged"), "a value cannot start a new log line")
	long := quoted(strings.Repeat("a", 500))
	assert.Less(t, len(long), 80)
	assert.True(t, strings.HasSuffix(long, `"...`))
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
		// The test client has no connection for Shutdown to close.
		h.Mu.Lock()
		delete(h.Clients, client)
		h.Mu.Unlock()
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
		require.NoError(t, publisher.Publish(context.Background(), exchange, testEvent(action)))
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

func TestConsumer_Broker_ResumesAfterABrokerRestart(t *testing.T) {
	exchange := withTestBroker(t)
	proxy, err := tcpproxy.New("localhost:5672")
	require.NoError(t, err)
	t.Cleanup(proxy.Close)
	config.Config.RabbitMQ.Host, config.Config.RabbitMQ.Port = proxy.Host(), proxy.Port()

	h, client := newBrokerHandler(t)
	require.NoError(t, h.StartConsumer(context.Background()))

	proxy.Cut()

	// Events published while the consumer subscribes again are lost with its
	// old queue, so publish until one arrives.
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = h.RabbitMQService.Publish(ctx, exchange, testEvent("update"))
		select {
		case <-client.Send:
			return true
		case <-time.After(200 * time.Millisecond):
			return false
		}
	}, 30*time.Second, 10*time.Millisecond, "the consumer must receive events again after a broker restart")
}

func TestConsumer_Broker_DropsInvalidEventsAndKeepsConsuming(t *testing.T) {
	exchange := withTestBroker(t)
	h, client := newBrokerHandler(t)
	require.NoError(t, h.StartConsumer(context.Background()))

	publisher := h.RabbitMQService
	for _, body := range []string{
		`{"entity":"users","action":"create","hk":"` + testHK + `","info":{"email":"victim@example.com"}}`,
		`not-json`,
		strings.Replace(testEvent("create"), `"symbol":"USDT"`, `"symbol":"USDT","html":"<img src=x onerror=alert(1)>"`, 1),
		testEvent("delete"),
	} {
		require.NoError(t, publisher.Publish(context.Background(), exchange, body))
	}

	created := receive(t, client)
	assert.Equal(t, "create", created["action"], "the invalid events before it were dropped and acknowledged")
	assert.NotContains(t, created["info"], "html", "the unknown field was dropped")
	deleted := receive(t, client)
	assert.Equal(t, "delete", deleted["action"])
	select {
	case extra := <-client.Send:
		t.Errorf("an invalid event reached the client: %s", extra)
	case <-time.After(200 * time.Millisecond):
	}
}
