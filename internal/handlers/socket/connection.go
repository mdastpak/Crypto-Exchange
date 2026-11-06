package socket

import (
	"encoding/json"
	"errors"
	"log"
	"math"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// limits bounds what each connection may do.
type limits struct {
	maxMessageBytes   int64
	pingInterval      time.Duration
	pongWait          time.Duration
	writeTimeout      time.Duration
	messagesPerSecond float64
	messageBurst      int
	maxConnections    int
	maxPerIP          int
}

// limitsFromConfig reads the websocket settings, with the defaults of the
// configuration for settings that are not set, as in a configuration that
// Load did not fill in.
func limitsFromConfig() limits {
	ws := config.Config.WebSocket
	or := func(value, fallback int) int {
		if value > 0 {
			return value
		}
		return fallback
	}
	ping := time.Duration(or(ws.PingInterval, 30)) * time.Second
	return limits{
		maxMessageBytes: int64(or(ws.MaxMessageSize, config.DefaultWSMaxMessageSize)),
		pingInterval:    ping,
		// A client that misses two pings in a row is gone.
		pongWait:          2 * ping,
		writeTimeout:      time.Duration(or(ws.WriteTimeout, config.DefaultWSWriteTimeoutSeconds)) * time.Second,
		messagesPerSecond: float64(or(ws.ClientMessagesPerSecond, config.DefaultWSClientMessagesPerSecond)),
		messageBurst:      or(ws.ClientMessageBurst, config.DefaultWSClientMessageBurst),
		maxConnections:    or(ws.MaxConnections, config.DefaultWSMaxConnections),
		maxPerIP:          or(ws.MaxConnectionsPerIP, config.DefaultWSMaxConnectionsPerIP),
	}
}

// accessTokenProtocol is the subprotocol that carries an access token from
// a browser, which cannot set headers on a WebSocket:
// new WebSocket(url, ["access_token", token]).
const accessTokenProtocol = "access_token"

// ServeWSGin serves a WebSocket upgrade. The client address, which the
// connection caps count, is Gin's client IP, which honors
// server.trusted_proxies.
func (h *WebSocketHandler) ServeWSGin(c *gin.Context) {
	h.serve(c.Writer, c.Request, c.ClientIP())
}

// ServeWS serves a WebSocket upgrade, with the TCP peer as the client
// address.
func (h *WebSocketHandler) ServeWS(w http.ResponseWriter, r *http.Request) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	h.serve(w, r, ip)
}

func (h *WebSocketHandler) serve(w http.ResponseWriter, r *http.Request, ip string) {
	select {
	case <-h.ShutdownCh:
		reject(w, http.StatusServiceUnavailable, "The server is shutting down")
		return
	default:
	}

	// The token is checked before the upgrade, so that a bad token gets a
	// plain HTTP answer.
	claims, status, message := h.authenticate(r)
	if status != 0 {
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", "Bearer")
		}
		reject(w, status, message)
		return
	}
	if status, message := h.reserve(ip); status != 0 {
		reject(w, status, message)
		return
	}

	conn, err := h.Upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.release(ip)
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	client := &Client{
		Conn:     conn,
		Send:     make(chan []byte, 256),
		Timezone: "UTC",
		ip:       ip,
		limiter:  newTokenBucket(h.limits.messagesPerSecond, h.limits.messageBurst),
	}
	if claims != nil {
		client.UserID, client.SessionID = claims.UUID, claims.SessionID
	}

	if !h.registerClient(client) {
		// Shutdown started during the upgrade.
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "server is shutting down"),
			time.Now().Add(h.limits.writeTimeout))
		conn.Close()
		h.release(ip)
		return
	}
	// Note: readPump's own defer unregisters the client on exit; unregistering
	// again here would double-close client.Send and panic.

	go h.writePump(client)

	h.readPump(client)
}

// connectionToken returns the access token that an upgrade request brings:
// an Authorization header with the Bearer scheme, or, from a browser, the
// subprotocols "access_token" and the token. A token in the URL is not
// read, because URLs end up in access logs.
func connectionToken(r *http.Request) (token string, present bool, err error) {
	if header := r.Header.Get("Authorization"); header != "" {
		scheme, value, found := strings.Cut(header, " ")
		value = strings.TrimSpace(value)
		if !found || !strings.EqualFold(scheme, "Bearer") || value == "" {
			return "", true, errors.New("bearer token required")
		}
		return value, true, nil
	}
	protocols := websocket.Subprotocols(r)
	if len(protocols) > 0 && protocols[0] == accessTokenProtocol {
		if len(protocols) < 2 || protocols[1] == "" {
			return "", true, errors.New("the access_token subprotocol needs the token as the next subprotocol")
		}
		return protocols[1], true, nil
	}
	return "", false, nil
}

// authenticate checks the token of an upgrade request, when it brings one.
// It returns a status and message to refuse the request with, or a zero
// status. A connection without a token is anonymous.
func (h *WebSocketHandler) authenticate(r *http.Request) (*auth.Claims, int, string) {
	token, present, err := connectionToken(r)
	if !present {
		return nil, 0, ""
	}
	if err != nil || h.Authenticate == nil {
		return nil, http.StatusUnauthorized, "Unauthorized - Invalid token"
	}
	claims, err := h.Authenticate(r.Context(), token)
	switch {
	case err == nil:
		return claims, 0, ""
	case errors.Is(err, auth.ErrAccountUnavailable):
		return nil, http.StatusForbidden, "This account is not available. Please contact support."
	case errors.Is(err, auth.ErrSessionInvalid):
		return nil, http.StatusUnauthorized, "Unauthorized - Session is not active"
	case errors.Is(err, auth.ErrInvalidToken), errors.Is(err, auth.ErrWrongTokenType), errors.Is(err, auth.ErrWrongRole):
		return nil, http.StatusUnauthorized, "Unauthorized - Invalid token"
	default:
		log.Printf("Failed to authenticate a WebSocket connection: %v", err)
		return nil, http.StatusInternalServerError, "Failed to authenticate the connection"
	}
}

// reserve counts a new connection from ip, unless this instance already has
// websocket.max_connections, or ip has websocket.max_connections_per_ip.
func (h *WebSocketHandler) reserve(ip string) (int, string) {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	if h.openPerIP == nil {
		h.openPerIP = make(map[string]int)
	}
	if h.open >= h.limits.maxConnections {
		return http.StatusServiceUnavailable, "Too many connections, try again later"
	}
	if h.openPerIP[ip] >= h.limits.maxPerIP {
		return http.StatusTooManyRequests, "Too many connections from this address"
	}
	h.open++
	h.openPerIP[ip]++
	return 0, ""
}

// release forgets a connection that reserve counted.
func (h *WebSocketHandler) release(ip string) {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	if h.open > 0 {
		h.open--
	}
	if h.openPerIP[ip] <= 1 {
		delete(h.openPerIP, ip)
	} else {
		h.openPerIP[ip]--
	}
}

// reject answers an upgrade request that is refused, in the envelope of the
// REST API.
func reject(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": status, "message": message})
}

// maxTimezoneLength bounds a time zone name from a client; IANA names are
// shorter.
const maxTimezoneLength = 64

// setTimezone sets the time zone of the pong timestamps when name is a
// known IANA zone, and reports whether it did. The zone is loaded once
// here, not on every ping.
func (c *Client) setTimezone(name string) bool {
	if name == "" || len(name) > maxTimezoneLength {
		return false
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return false
	}
	c.Timezone, c.location = name, location
	return true
}

// tokenBucket limits the messages of one connection. Only the connection's
// read loop uses it, so it needs no lock.
type tokenBucket struct {
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newTokenBucket(rate float64, burst int) *tokenBucket {
	return &tokenBucket{rate: rate, burst: float64(burst), tokens: float64(burst)}
}

// allow takes a token if one is left. Tokens refill at rate per second, up
// to burst.
func (b *tokenBucket) allow(now time.Time) bool {
	if !b.last.IsZero() {
		b.tokens = math.Min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
