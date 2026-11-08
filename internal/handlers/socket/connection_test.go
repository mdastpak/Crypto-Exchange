package socket

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gateway serves h on /ws and returns the WebSocket URL.
func gateway(t *testing.T, h *WebSocketHandler) string {
	t.Helper()
	router := gin.New()
	router.GET("/ws", h.ServeWSGin)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
}

// dial connects with the given headers and closes the connection when the
// test ends.
func dial(t *testing.T, url string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(url, header)
	if conn != nil {
		t.Cleanup(func() { conn.Close() })
	}
	return conn, resp, err
}

// fakeAuth accepts "good" and maps other tokens to errors.
func fakeAuth(ctx context.Context, token string) (*auth.Claims, error) {
	switch token {
	case "good":
		return &auth.Claims{UUID: "user-1", SessionID: "session-1"}, nil
	case "ended":
		return nil, auth.ErrSessionInvalid
	case "disabled":
		return nil, auth.ErrAccountUnavailable
	case "broken":
		return nil, errors.New("database down")
	}
	return nil, auth.ErrInvalidToken
}

// onlyClient waits for one registered client and returns it.
func onlyClient(t *testing.T, h *WebSocketHandler) *Client {
	t.Helper()
	require.Eventually(t, func() bool { return h.clientCount() == 1 }, 2*time.Second, 5*time.Millisecond)
	h.Mu.RLock()
	defer h.Mu.RUnlock()
	for client := range h.Clients {
		return client
	}
	return nil
}

func TestLimitsFromConfig(t *testing.T) {
	saved := config.Config.WebSocket
	t.Cleanup(func() { config.Config.WebSocket = saved })

	config.Config.WebSocket = saved
	config.Config.WebSocket.PingInterval, config.Config.WebSocket.MaxMessageSize = 0, 0
	config.Config.WebSocket.WriteTimeout, config.Config.WebSocket.ClientMessagesPerSecond = 0, 0
	config.Config.WebSocket.ClientMessageBurst, config.Config.WebSocket.MaxConnections, config.Config.WebSocket.MaxConnectionsPerIP = 0, 0, 0
	config.Config.WebSocket.SessionCheckSeconds, config.Config.WebSocket.MaxConnectionsPerUser = 0, 0
	l := limitsFromConfig()
	assert.Equal(t, int64(4096), l.maxMessageBytes, "the defaults apply to settings that are not set")
	assert.Equal(t, 30*time.Second, l.pingInterval)
	assert.Equal(t, 60*time.Second, l.pongWait, "two ping intervals")
	assert.Equal(t, 10*time.Second, l.writeTimeout)
	assert.Equal(t, 10.0, l.messagesPerSecond)
	assert.Equal(t, 20, l.messageBurst)
	assert.Equal(t, 10000, l.maxConnections)
	assert.Equal(t, 20, l.maxPerIP)
	assert.Equal(t, 10, l.maxPerUser)
	assert.Equal(t, 60*time.Second, l.sessionCheck)

	config.Config.WebSocket.PingInterval, config.Config.WebSocket.MaxMessageSize = 5, 1024
	config.Config.WebSocket.WriteTimeout, config.Config.WebSocket.ClientMessagesPerSecond = 3, 2
	config.Config.WebSocket.ClientMessageBurst, config.Config.WebSocket.MaxConnections, config.Config.WebSocket.MaxConnectionsPerIP = 4, 100, 7
	config.Config.WebSocket.SessionCheckSeconds, config.Config.WebSocket.MaxConnectionsPerUser = 15, 3
	l = limitsFromConfig()
	assert.Equal(t, limits{maxMessageBytes: 1024, pingInterval: 5 * time.Second, pongWait: 10 * time.Second, writeTimeout: 3 * time.Second,
		messagesPerSecond: 2, messageBurst: 4, maxConnections: 100, maxPerIP: 7, maxPerUser: 3, sessionCheck: 15 * time.Second,
		ipv6Prefix: config.Config.RateLimits.IPv6PrefixLength}, l)
}

func TestServe_AuthenticatesTheConnectionToken(t *testing.T) {
	for name, tc := range map[string]struct {
		header     http.Header
		wantStatus int
		wantUser   string
	}{
		"no token":                     {nil, http.StatusSwitchingProtocols, ""},
		"bearer header":                {http.Header{"Authorization": {"Bearer good"}}, http.StatusSwitchingProtocols, "user-1"},
		"subprotocol from a browser":   {http.Header{"Sec-Websocket-Protocol": {"access_token, good"}}, http.StatusSwitchingProtocols, "user-1"},
		"token in the URL is ignored":  {nil, http.StatusSwitchingProtocols, ""},
		"another scheme":               {http.Header{"Authorization": {"Basic Z29vZA=="}}, http.StatusUnauthorized, ""},
		"subprotocol without token":    {http.Header{"Sec-Websocket-Protocol": {"access_token"}}, http.StatusUnauthorized, ""},
		"invalid token":                {http.Header{"Authorization": {"Bearer forged"}}, http.StatusUnauthorized, ""},
		"ended session":                {http.Header{"Authorization": {"Bearer ended"}}, http.StatusUnauthorized, ""},
		"user who cannot sign in":      {http.Header{"Authorization": {"Bearer disabled"}}, http.StatusForbidden, ""},
		"authentication service error": {http.Header{"Authorization": {"Bearer broken"}}, http.StatusInternalServerError, ""},
	} {
		t.Run(name, func(t *testing.T) {
			h := newTestHandler()
			h.Authenticate = fakeAuth
			url := gateway(t, h)
			if name == "token in the URL is ignored" {
				url += "?access_token=good"
			}

			conn, resp, err := dial(t, url, tc.header)
			require.NotNil(t, resp)
			assert.Equal(t, tc.wantStatus, resp.StatusCode)
			if tc.wantStatus != http.StatusSwitchingProtocols {
				require.Error(t, err)
				if tc.wantStatus == http.StatusUnauthorized {
					assert.Equal(t, "Bearer", resp.Header.Get("WWW-Authenticate"))
				}
				assert.Equal(t, 0, h.clientCount())
				return
			}
			require.NoError(t, err)
			client := onlyClient(t, h)
			assert.Equal(t, tc.wantUser, client.UserID)
			if tc.wantUser != "" {
				assert.Equal(t, "session-1", client.SessionID)
			}
			if name == "subprotocol from a browser" {
				assert.Equal(t, accessTokenProtocol, conn.Subprotocol(), "the handshake must select the subprotocol")
			}
		})
	}
}

func TestServe_RefusesTokensWithoutAnAuthenticator(t *testing.T) {
	h := newTestHandler()
	_, resp, err := dial(t, gateway(t, h), http.Header{"Authorization": {"Bearer good"}})
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestServe_CapsConnectionsPerAddressAndInTotal(t *testing.T) {
	h := newTestHandler()
	h.limits.maxPerIP, h.limits.maxConnections = 2, 3
	url := gateway(t, h)

	first, _, err := dial(t, url, nil)
	require.NoError(t, err)
	_, _, err = dial(t, url, nil)
	require.NoError(t, err)
	// A dial returns once the handshake is answered, which can be before
	// the server registers the client: wait for both, so that the count
	// below is of these two connections and not of a stale state.
	require.Eventually(t, func() bool { return h.clientCount() == 2 }, 2*time.Second, 5*time.Millisecond)
	_, resp, err := dial(t, url, nil)
	require.Error(t, err, "a third connection from the same address")
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)

	// Closing a connection frees its place.
	first.Close()
	require.Eventually(t, func() bool { return h.clientCount() == 1 }, 2*time.Second, 5*time.Millisecond)
	_, _, err = dial(t, url, nil)
	require.NoError(t, err)

	// The total cap counts every address.
	h.Mu.Lock()
	h.limits.maxPerIP = 10
	h.Mu.Unlock()
	_, _, err = dial(t, url, nil)
	require.NoError(t, err)
	_, resp, err = dial(t, url, nil)
	require.Error(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestReadPump_EndsConnectionsThatSendTooMuch(t *testing.T) {
	t.Run("a message over the size limit", func(t *testing.T) {
		h := newTestHandler()
		h.limits.maxMessageBytes = 64
		conn, _, err := dial(t, gateway(t, h), nil)
		require.NoError(t, err)

		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"event":"ping","timezone":"`+strings.Repeat("x", 100)+`"}`)))
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
		_, _, err = conn.ReadMessage()
		assert.True(t, websocket.IsCloseError(err, websocket.CloseMessageTooBig), "got %v", err)
	})

	t.Run("messages faster than the rate", func(t *testing.T) {
		h := newTestHandler()
		h.limits.messagesPerSecond, h.limits.messageBurst = 1, 3
		conn, _, err := dial(t, gateway(t, h), nil)
		require.NoError(t, err)

		for i := 0; i < 5; i++ {
			require.NoError(t, conn.WriteJSON(map[string]string{"event": "subscribe"}))
		}
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
		_, _, err = conn.ReadMessage()
		assert.True(t, websocket.IsCloseError(err, websocket.ClosePolicyViolation), "got %v", err)
		require.Eventually(t, func() bool { return h.clientCount() == 0 }, 2*time.Second, 5*time.Millisecond)
	})
}

func TestTokenBucket(t *testing.T) {
	b := newTokenBucket(2, 3)
	now := time.Now()
	for i := 0; i < 3; i++ {
		assert.True(t, b.allow(now), "the burst")
	}
	assert.False(t, b.allow(now))
	assert.True(t, b.allow(now.Add(500*time.Millisecond)), "one token after half a second at 2 per second")
	assert.False(t, b.allow(now.Add(500*time.Millisecond)))
	for i := 0; i < 3; i++ {
		assert.True(t, b.allow(now.Add(time.Hour)), "the bucket refills to the burst, not beyond")
	}
	assert.False(t, b.allow(now.Add(time.Hour)))
}

func TestSetTimezone(t *testing.T) {
	client := newTestClient()
	require.True(t, client.setTimezone("Asia/Tehran"))
	assert.Equal(t, "Asia/Tehran", client.Timezone)
	tehran := client.location

	for _, name := range []string{"Not/AZone", "", "../../etc/passwd", "/etc/localtime", strings.Repeat("A", 65)} {
		assert.False(t, client.setTimezone(name), "%q", name)
		assert.Equal(t, "Asia/Tehran", client.Timezone, "an invalid zone keeps the previous one")
		assert.Same(t, tehran, client.location)
	}
}

func TestShutdown_IsIdempotentAndRefusesNewConnections(t *testing.T) {
	h := newTestHandler()
	url := gateway(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	require.NoError(t, h.Shutdown(ctx))
	assert.NotPanics(t, func() { require.NoError(t, h.Shutdown(ctx)) })

	_, resp, err := dial(t, url, nil)
	require.Error(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestShutdown_SendsClientsACloseFrame(t *testing.T) {
	h := newTestHandler()
	conn, _, err := dial(t, gateway(t, h), nil)
	require.NoError(t, err)
	onlyClient(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, h.Shutdown(ctx))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, _, err = conn.ReadMessage()
	var closeErr *websocket.CloseError
	require.ErrorAs(t, err, &closeErr)
	assert.Equal(t, websocket.CloseGoingAway, closeErr.Code)
	assert.Equal(t, "server is shutting down", closeErr.Text)
	require.Eventually(t, func() bool { return h.clientCount() == 0 }, 2*time.Second, 5*time.Millisecond)
}

// TestShutdown_DuringAnUpgradeClosesTheNewConnection starts Shutdown while
// an upgrade is being authenticated, after the gateway checked for the
// shutdown. The connection must get a close frame instead of being
// registered once Shutdown is done, when nothing would close it.
func TestShutdown_DuringAnUpgradeClosesTheNewConnection(t *testing.T) {
	h := newTestHandler()
	authenticating := make(chan struct{})
	proceed := make(chan struct{})
	h.Authenticate = func(ctx context.Context, token string) (*auth.Claims, error) {
		close(authenticating)
		<-proceed
		return fakeAuth(ctx, token)
	}
	url := gateway(t, h)
	type dialed struct {
		conn *websocket.Conn
		err  error
	}
	result := make(chan dialed, 1)
	go func() {
		conn, _, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": {"Bearer good"}})
		result <- dialed{conn, err}
	}()
	select {
	case <-authenticating:
	case <-time.After(2 * time.Second):
		t.Fatal("the upgrade did not reach the authenticator")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, h.Shutdown(ctx))
	close(proceed)

	res := <-result
	require.NoError(t, res.err, "the upgrade completes")
	defer res.conn.Close()
	require.NoError(t, res.conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, _, err := res.conn.ReadMessage()
	var closeErr *websocket.CloseError
	require.ErrorAs(t, err, &closeErr)
	assert.Equal(t, websocket.CloseGoingAway, closeErr.Code)
	assert.False(t, h.registerClient(&Client{ip: "192.0.2.1"}), "no client registers after the shutdown")
	assert.Zero(t, h.clientCount())
}

// TestServe_RejectsInTheErrorEnvelope checks that a refused upgrade, and a
// request that is not a WebSocket handshake, answer in the error envelope
// of the REST API, with a code (#53).
func TestServe_RejectsInTheErrorEnvelope(t *testing.T) {
	h := newTestHandler()
	h.Authenticate = fakeAuth
	router := gin.New()
	router.GET("/ws", func(c *gin.Context) {
		c.Header("X-Request-ID", "req-7")
		h.ServeWSGin(c)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	decode := func(resp *http.Response) models.APIResponse {
		t.Helper()
		var body models.APIResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		return body
	}

	resp, err := http.Get(server.URL + "/ws")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "a plain GET is not a handshake")
	assert.Equal(t, "13", resp.Header.Get("Sec-Websocket-Version"))
	body := decode(resp)
	assert.Equal(t, "invalid_request", body.Code)
	assert.Equal(t, "req-7", body.RequestID)

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	for token, code := range map[string]string{"forged": "token_invalid", "ended": "session_inactive", "disabled": "account_unavailable"} {
		_, resp, err := dial(t, url, http.Header{"Authorization": {"Bearer " + token}})
		require.Error(t, err)
		assert.Equal(t, code, decode(resp).Code, token)
		resp.Body.Close()
	}
}

// TestReserve_GroupsIPv6ClientsByPrefix checks that the addresses of one
// IPv6 /64 share websocket.max_connections_per_ip (#188).
func TestReserve_GroupsIPv6ClientsByPrefix(t *testing.T) {
	h := newTestHandler()
	h.limits.maxPerIP, h.limits.maxConnections, h.limits.ipv6Prefix = 2, 100, 64

	status, _, _ := h.reserve(h.addressKey("2001:db8:1:2::1"), "")
	require.Zero(t, status)
	status, _, _ = h.reserve(h.addressKey("2001:db8:1:2::2"), "")
	require.Zero(t, status)
	status, _, _ = h.reserve(h.addressKey("2001:db8:1:2:ffff::3"), "")
	assert.Equal(t, http.StatusTooManyRequests, status, "a third address of the same /64")

	status, _, _ = h.reserve(h.addressKey("2001:db8:1:3::1"), "")
	assert.Zero(t, status, "another /64 has a budget of its own")

	h.release(h.addressKey("2001:db8:1:2::1"), "")
	status, _, _ = h.reserve(h.addressKey("2001:db8:1:2::9"), "")
	assert.Zero(t, status, "releasing any address of the /64 frees its place")
}
