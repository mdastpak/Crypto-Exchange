package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dialWebSocket connects to the gateway and waits until the server has
// registered the client, by completing a ping.
func dialWebSocket(t *testing.T, serverURL string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(serverURL, "http")+"/ws/", nil)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	require.NoError(t, conn.WriteJSON(map[string]string{"event": "ping"}))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var pong map[string]interface{}
	require.NoError(t, conn.ReadJSON(&pong))
	require.Equal(t, "pong", pong["event"])
	return conn
}

// TestWebSocket_CurrencyEventsReachEveryClient creates a currency through the
// admin API and expects every connected WebSocket client to receive the event.
func TestWebSocket_CurrencyEventsReachEveryClient(t *testing.T) {
	env := newE2EEnv(t)
	server := httptest.NewServer(env.router)
	defer server.Close()
	clients := []*websocket.Conn{dialWebSocket(t, server.URL), dialWebSocket(t, server.URL)}

	admin, err := env.accounts.Create(context.Background(), "events@example.com")
	require.NoError(t, err)
	token := env.signInAdmin(t, admin)
	status, resp := env.post(t, "/admin/currencies", testCurrency("WSE"), token)
	require.Equal(t, http.StatusCreated, status, resp.Message)

	for i, conn := range clients {
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		var event map[string]interface{}
		require.NoError(t, conn.ReadJSON(&event), "client %d received no event", i)
		assert.Equal(t, "currencies", event["entity"], "client %d", i)
		assert.Equal(t, "create", event["action"], "client %d", i)
		assert.NotEmpty(t, event["hk"], "client %d", i)
		assert.NotEmpty(t, event["server_time"], "client %d", i)
		info, ok := event["info"].(map[string]interface{})
		require.True(t, ok, "client %d: the event carries the currency info", i)
		assert.Equal(t, "WSE", info["symbol"], "client %d", i)
	}
}
