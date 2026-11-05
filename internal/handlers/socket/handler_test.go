package socket

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func newTestHandler() *WebSocketHandler {
	return NewWebSocketHandler(&models.InfraServices{RabbitMQ: &rabbitmq.RabbitMQService{}})
}

func newTestClient() *Client {
	return &Client{Send: make(chan []byte, 256), Timezone: "UTC"}
}

func TestNewWebSocketHandler_InitializesFields(t *testing.T) {
	h := newTestHandler()

	assert.NotNil(t, h.Clients)
	assert.Empty(t, h.Clients)
	assert.NotNil(t, h.ShutdownCh)
	assert.NotNil(t, h.RabbitMQService)
}

func TestRegisterUnregisterClient_UpdatesClientCount(t *testing.T) {
	h := newTestHandler()
	client := newTestClient()
	// registerClient/unregisterClient close/read the connection field only
	// indirectly via Conn.Close(); use a real (unconnected) websocket.Conn
	// substitute by skipping Conn access: unregisterClient calls
	// client.Conn.Close(), so we need a functioning Conn. We instead test the
	// map bookkeeping directly through the exported Clients map, matching
	// what registerClient/unregisterClient do internally.
	h.Mu.Lock()
	h.Clients[client] = true
	h.Mu.Unlock()

	assert.Equal(t, 1, h.clientCount())

	h.Mu.Lock()
	delete(h.Clients, client)
	h.Mu.Unlock()

	assert.Equal(t, 0, h.clientCount())
}

func TestBroadcast_SendsToAllRegisteredClients(t *testing.T) {
	h := newTestHandler()
	c1 := newTestClient()
	c2 := newTestClient()

	h.Mu.Lock()
	h.Clients[c1] = true
	h.Clients[c2] = true
	h.Mu.Unlock()

	message := []byte(`{"hello":"world"}`)
	h.Broadcast(message)

	select {
	case got := <-c1.Send:
		assert.Equal(t, message, got)
	default:
		t.Fatal("expected client 1 to receive the broadcast message")
	}

	select {
	case got := <-c2.Send:
		assert.Equal(t, message, got)
	default:
		t.Fatal("expected client 2 to receive the broadcast message")
	}
}

func TestBroadcast_SkipsClientsWithFullBuffer(t *testing.T) {
	h := newTestHandler()
	client := &Client{Send: make(chan []byte, 1), Timezone: "UTC"}
	client.Send <- []byte("already-full")

	h.Mu.Lock()
	h.Clients[client] = true
	h.Mu.Unlock()

	// Broadcasting must not block or panic even though the client's buffer
	// is already full; the message is simply dropped for that client.
	assert.NotPanics(t, func() {
		h.Broadcast([]byte("new-message"))
	})

	assert.Equal(t, 1, len(client.Send))
}

func TestHandleClientMessage_InvalidJSONIsIgnored(t *testing.T) {
	h := newTestHandler()
	client := newTestClient()

	assert.NotPanics(t, func() {
		h.handleClientMessage(client, []byte("not-json"))
	})
	assert.Empty(t, client.Send)
}

func TestHandleClientMessage_MissingEventFieldIsIgnored(t *testing.T) {
	h := newTestHandler()
	client := newTestClient()

	h.handleClientMessage(client, []byte(`{"channels":["a"]}`))
	assert.Empty(t, client.Send)
}

func TestHandleClientMessage_PingUpdatesTimezoneAndRepliesPong(t *testing.T) {
	h := newTestHandler()
	client := newTestClient()

	h.handleClientMessage(client, []byte(`{"event":"ping","timezone":"Asia/Tehran"}`))

	assert.Equal(t, "Asia/Tehran", client.Timezone)

	select {
	case data := <-client.Send:
		var msg map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &msg))
		assert.Equal(t, "pong", msg["event"])
		assert.NotEmpty(t, msg["timestamp"])
	default:
		t.Fatal("expected a pong message to be queued")
	}
}

func TestHandleClientMessage_SubscribeEventDoesNotReply(t *testing.T) {
	h := newTestHandler()
	client := newTestClient()

	h.handleClientMessage(client, []byte(`{"event":"subscribe","channels":["prices"]}`))
	assert.Empty(t, client.Send)
}

func TestHandleClientMessage_UnknownEventDoesNotReply(t *testing.T) {
	h := newTestHandler()
	client := newTestClient()

	h.handleClientMessage(client, []byte(`{"event":"mystery"}`))
	assert.Empty(t, client.Send)
}

func TestHandleClientPing_InvalidTimezoneFallsBackToUTC(t *testing.T) {
	h := newTestHandler()
	client := newTestClient()
	client.Timezone = "Not/ARealZone"

	h.handleClientPing(client)

	select {
	case data := <-client.Send:
		var msg map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &msg))
		assert.Equal(t, "pong", msg["event"])
	default:
		t.Fatal("expected a pong message even with an invalid timezone")
	}
}

func TestHandleClientPing_DropsWhenBufferFull(t *testing.T) {
	h := newTestHandler()
	client := &Client{Send: make(chan []byte, 1), Timezone: "UTC"}
	client.Send <- []byte("occupying-slot")

	assert.NotPanics(t, func() {
		h.handleClientPing(client)
	})
	assert.Equal(t, 1, len(client.Send))
}

func TestServeWSGin_UpgradesAndRespondsToPing(t *testing.T) {
	h := newTestHandler()

	router := gin.New()
	router.GET("/ws", h.ServeWSGin)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	require.Eventually(t, func() bool { return h.clientCount() == 1 }, time.Second, 10*time.Millisecond)

	require.NoError(t, conn.WriteJSON(map[string]string{"event": "ping"}))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	var resp map[string]interface{}
	require.NoError(t, conn.ReadJSON(&resp))
	assert.Equal(t, "pong", resp["event"])

	conn.Close()
	require.Eventually(t, func() bool { return h.clientCount() == 0 }, time.Second, 10*time.Millisecond)
}

func TestServeWSGin_RejectsDisallowedOrigin(t *testing.T) {
	original := config.Config.WebSocket.AllowedOrigins
	config.Config.WebSocket.AllowedOrigins = []string{"https://allowed.example.com"}
	defer func() { config.Config.WebSocket.AllowedOrigins = original }()

	h := newTestHandler()
	router := gin.New()
	router.GET("/ws", h.ServeWSGin)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	header := http.Header{"Origin": []string{"https://not-allowed.example.com"}}
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, header)

	require.Error(t, err)
	if resp != nil {
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	}
}

func TestShutdown_ClosesClientConnectionsAndReturns(t *testing.T) {
	h := newTestHandler()

	router := gin.New()
	router.GET("/ws", h.ServeWSGin)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	require.Eventually(t, func() bool { return h.clientCount() == 1 }, time.Second, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err = h.Shutdown(ctx)
	assert.NoError(t, err)
}

func TestShutdown_ReturnsContextErrorWhenDeadlineExceeded(t *testing.T) {
	h := newTestHandler()
	// Simulate an in-flight goroutine that never finishes so Wg.Wait blocks.
	h.Wg.Add(1)
	defer h.Wg.Done()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := h.Shutdown(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestWritePump_SendsEachMessageAsItsOwnFrame queues several messages before
// the pump runs and checks that every frame holds exactly one of them.
func TestWritePump_SendsEachMessageAsItsOwnFrame(t *testing.T) {
	h := newTestHandler()
	messages := []string{
		`{"event":"pong","timestamp":"2026-11-04T10:00:00Z"}`,
		`{"entity":"currencies","action":"create"}`,
		`{"entity":"currencies","action":"delete"}`,
	}

	pumped := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		client := &Client{Conn: conn, Send: make(chan []byte, len(messages))}
		for _, m := range messages {
			client.Send <- []byte(m)
		}
		close(client.Send)
		h.Wg.Add(1)
		h.writePump(client)
		close(pumped)
	}))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer conn.Close()

	for i, want := range messages {
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
		messageType, frame, err := conn.ReadMessage()
		require.NoError(t, err, "frame %d", i)
		assert.Equal(t, websocket.TextMessage, messageType)
		assert.JSONEq(t, want, string(frame), "frame %d must hold exactly one message", i)
	}
	<-pumped
}
