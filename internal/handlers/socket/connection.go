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
	"github.com/mdastpak/crypto-exchange/internal/clientip"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/platform"

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
	// maxPerUser bounds the connections of one user, over all of its
	// addresses (#234).
	maxPerUser int
	// ipv6Prefix groups IPv6 clients for maxPerIP, as the rate limits do.
	ipv6Prefix int
	// sessionCheck is how often the sessions of the authenticated
	// connections are checked (#189).
	sessionCheck time.Duration
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
		maxPerUser:        or(ws.MaxConnectionsPerUser, config.DefaultWSMaxConnectionsPerUser),
		ipv6Prefix:        config.Config.RateLimits.IPv6PrefixLength,
		sessionCheck:      time.Duration(or(ws.SessionCheckSeconds, config.DefaultWSSessionCheckSeconds)) * time.Second,
	}
}

// accessTokenProtocol is the subprotocol that carries an access token from
// a browser, which cannot set headers on a WebSocket:
// new WebSocket(url, ["access_token", token]).
const accessTokenProtocol = "access_token"

// ServeWSGin serves a WebSocket upgrade. The client address, which the
// connection caps count, is Gin's client IP, which honors
// server.trusted_proxies.
//
// @Summary Connect to the WebSocket gateway
// @Description Upgrades to a WebSocket. An optional user access token comes as Authorization: Bearer, or from a browser as the subprotocols access_token and the token. docs/api.md describes the messages.
// @Tags websocket
// @Produce json
// @Param Authorization header string false "Bearer <access token>"
// @Success 101 "Switching Protocols"
// @Failure 400 {object} models.APIResponse "Not a WebSocket handshake"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 429 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse
// @Router /api/v1/ws/ [get]
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
	addr := ip
	ip = h.addressKey(ip)
	select {
	case <-h.ShutdownCh:
		reject(w, http.StatusServiceUnavailable, helpers.StatusCode(http.StatusServiceUnavailable), "The server is shutting down")
		return
	default:
	}

	// The token is checked before the upgrade, so that a bad token gets a
	// plain HTTP answer.
	claims, status, code, message := h.authenticate(r)
	if status != 0 {
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", "Bearer")
		}
		reject(w, status, code, message)
		return
	}
	subject := platform.Subject{IP: addr}
	if claims != nil {
		subject.UserHK = claims.UUID
	}
	if locked := h.platformLocked(subject); locked != nil {
		rejectLocked(w, locked)
		return
	}
	if status, code, message := h.reserve(ip, subject.UserHK); status != 0 {
		reject(w, status, code, message)
		return
	}
	// The connections of the other instances count too (#234).
	slots, status, code, message := h.reserveShared(r.Context(), ip, subject.UserHK)
	if status != 0 {
		h.release(ip, subject.UserHK)
		reject(w, status, code, message)
		return
	}

	conn, err := h.Upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.release(ip, subject.UserHK)
		h.releaseShared(slots)
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	client := &Client{
		Conn:     conn,
		Send:     make(chan []byte, 256),
		Timezone: "UTC",
		ip:       ip,
		addr:     addr,
		limiter:  newTokenBucket(h.limits.messagesPerSecond, h.limits.messageBurst),
		// The connection counts against its user from the start.
		countedUser: subject.UserHK,
		slots:       slots,
	}
	if claims != nil {
		h.authorize(client, claims)
	}

	if !h.registerClient(client) {
		// Shutdown started during the upgrade.
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "server is shutting down"),
			time.Now().Add(h.limits.writeTimeout))
		conn.Close()
		h.release(ip, subject.UserHK)
		h.releaseShared(slots)
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
// It returns a status, an error code, and a message to refuse the request
// with, or a zero status. A connection without a token is anonymous. The
// codes are those of the REST routes for the same failures.
func (h *WebSocketHandler) authenticate(r *http.Request) (*auth.Claims, int, string, string) {
	token, present, err := connectionToken(r)
	if !present {
		return nil, 0, "", ""
	}
	if err != nil || h.Authenticate == nil {
		return nil, http.StatusUnauthorized, helpers.CodeTokenInvalid, "Unauthorized - Invalid token"
	}
	claims, err := h.Authenticate(r.Context(), token)
	switch {
	case err == nil:
		return claims, 0, "", ""
	case errors.Is(err, auth.ErrAccountUnavailable):
		code, message := auth.AccountError(err)
		return nil, http.StatusForbidden, code, message.String()
	case errors.Is(err, auth.ErrSessionInvalid):
		return nil, http.StatusUnauthorized, "session_inactive", "Unauthorized - Session is not active"
	case errors.Is(err, auth.ErrInvalidToken), errors.Is(err, auth.ErrWrongTokenType), errors.Is(err, auth.ErrWrongRole):
		return nil, http.StatusUnauthorized, helpers.CodeTokenInvalid, "Unauthorized - Invalid token"
	default:
		log.Printf("Failed to authenticate a WebSocket connection: %v", err)
		return nil, http.StatusInternalServerError, helpers.StatusCode(http.StatusInternalServerError), "Failed to authenticate the connection"
	}
}

// addressKey is the address that websocket.max_connections_per_ip counts
// by: an IPv6 client counts by its prefix, so that the addresses of one /64
// share the limit (#188), as they share the rate limits.
func (h *WebSocketHandler) addressKey(ip string) string {
	return clientip.Key(ip, h.limits.ipv6Prefix)
}

// reserve counts a new connection from ip, of userID when it is not
// empty, unless this instance already has websocket.max_connections, ip
// has websocket.max_connections_per_ip, or the user has
// websocket.max_connections_per_user here. It returns the status, error
// code, and message to refuse the connection with, or a zero status.
func (h *WebSocketHandler) reserve(ip, userID string) (int, string, string) {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	if h.openPerIP == nil {
		h.openPerIP = make(map[string]int)
	}
	if h.openPerUser == nil {
		h.openPerUser = make(map[string]int)
	}
	if h.open >= h.limits.maxConnections {
		return http.StatusServiceUnavailable, helpers.StatusCode(http.StatusServiceUnavailable), "Too many connections, try again later"
	}
	if h.openPerIP[ip] >= h.limits.maxPerIP {
		return http.StatusTooManyRequests, helpers.StatusCode(http.StatusTooManyRequests), "Too many connections from this address"
	}
	if userID != "" && h.openPerUser[userID] >= h.limits.maxPerUser {
		return http.StatusTooManyRequests, CodeTooManyConnections, "Too many connections for this user"
	}
	h.open++
	h.openPerIP[ip]++
	if userID != "" {
		h.openPerUser[userID]++
	}
	return 0, "", ""
}

// release forgets a connection that reserve counted.
func (h *WebSocketHandler) release(ip, userID string) {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	h.releaseLocked(ip, userID)
}

// releaseLocked is release for a caller that holds Mu.
func (h *WebSocketHandler) releaseLocked(ip, userID string) {
	if h.open > 0 {
		h.open--
	}
	decrement(h.openPerIP, ip)
	if userID != "" {
		decrement(h.openPerUser, userID)
	}
}

// decrement lowers the count of key, and forgets it at zero.
func decrement(counts map[string]int, key string) {
	if counts[key] <= 1 {
		delete(counts, key)
	} else {
		counts[key]--
	}
}

// counted returns the user that c counts against, or "".
func (c *Client) counted() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.countedUser
}

// reject answers an upgrade request that is refused, in the error envelope
// of the REST API: with a code and the request ID, which the request ID
// middleware has set as a response header.
func reject(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(models.APIResponse{
		Status: status, Message: message, Code: code, RequestID: w.Header().Get(requestIDHeader),
	})
}

// requestIDHeader is the header of middleware.RequestIDHeader, which this
// package cannot import without a cycle.
const requestIDHeader = "X-Request-ID"

// handshakeError answers an upgrade request that is not a valid WebSocket
// handshake, as the upgrader finds it, in the same envelope instead of the
// upgrader's plain text. Like the upgrader, it names the supported version.
func handshakeError(w http.ResponseWriter, _ *http.Request, status int, reason error) {
	w.Header().Set("Sec-Websocket-Version", "13")
	reject(w, status, helpers.StatusCode(status), reason.Error())
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
