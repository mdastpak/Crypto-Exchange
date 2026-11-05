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
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
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
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
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

// TestWebSocket_AuthenticatesWithAUserToken connects through the real
// router with a token from a real sign-in, and checks that the token gets
// the checks of the user routes.
func TestWebSocket_AuthenticatesWithAUserToken(t *testing.T) {
	env := newE2EEnv(t)
	server := httptest.NewServer(env.router)
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/"
	session := env.startSession(t, "ws-user@example.com")

	connect := func(header http.Header) (*websocket.Conn, *http.Response, error) {
		conn, resp, err := websocket.DefaultDialer.Dial(url, header)
		if conn != nil {
			t.Cleanup(func() { conn.Close() })
		}
		return conn, resp, err
	}

	_, _, err := connect(http.Header{"Authorization": {"Bearer " + session.access}})
	require.NoError(t, err, "a valid user token connects")
	conn, _, err := connect(http.Header{"Sec-Websocket-Protocol": {"access_token, " + session.access}})
	require.NoError(t, err, "a browser sends the token as a subprotocol")
	assert.Equal(t, "access_token", conn.Subprotocol())
	require.Eventually(t, func() bool {
		env.handlers.Socket.Mu.RLock()
		defer env.handlers.Socket.Mu.RUnlock()
		authenticated := 0
		for client := range env.handlers.Socket.Clients {
			if client.UserID == session.userID && client.SessionID == session.sessionID {
				authenticated++
			}
		}
		return authenticated == 2
	}, 5*time.Second, 10*time.Millisecond, "the connections know their user and session")

	_, resp, err := connect(http.Header{"Authorization": {"Bearer forged"}})
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	w := env.send(t, http.MethodPost, "/user/logout", nil, session.access, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	_, resp, err = connect(http.Header{"Authorization": {"Bearer " + session.access}})
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a token of an ended session is refused")

	_, _, err = connect(nil)
	assert.NoError(t, err, "anonymous connections still work")
}
