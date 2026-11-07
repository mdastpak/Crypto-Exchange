package socket

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/auth"

	"github.com/gorilla/websocket"
)

// Close codes of an authenticated connection that loses its authorization
// (#189). They mirror the HTTP status of the same failure on the REST
// routes, in the range that RFC 6455 leaves to applications.
const (
	// CloseUnauthorized ends a connection whose token expired, whose
	// session ended, or that re-authenticated with a bad token.
	CloseUnauthorized = 4401
	// CloseForbidden ends a connection whose user can no longer sign in.
	CloseForbidden = 4403
)

// Close reasons, which are the error codes of the REST routes.
const (
	reasonTokenExpired       = "token_expired"
	reasonSessionInactive    = "session_inactive"
	reasonAccountUnavailable = "account_unavailable"
	reasonTokenInvalid       = "token_invalid"
)

// sessionCheckTimeout bounds one check of a session.
const sessionCheckTimeout = 5 * time.Second

// authorize records the principal of an accepted token on client, and
// closes the connection when the token expires, unless the client sends a
// newer one first. A connection lives as long as its access token, like a
// REST request, instead of for as long as it stays open.
func (h *WebSocketHandler) authorize(client *Client, claims *auth.Claims) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.UserID, client.SessionID = claims.UUID, claims.SessionID
	if client.expiry != nil {
		client.expiry.Stop()
		client.expiry = nil
	}
	if claims.ExpiresAt == nil {
		return
	}
	client.expiresAt = claims.ExpiresAt.Time
	client.expiry = time.AfterFunc(time.Until(client.expiresAt), func() {
		h.closeClient(client, CloseUnauthorized, reasonTokenExpired)
	})
}

// forget stops the expiry of c, once it has disconnected.
func (c *Client) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.expiry != nil {
		c.expiry.Stop()
		c.expiry = nil
	}
}

// principal returns the session and user of c, empty for an anonymous
// connection.
func (c *Client) principal() (sessionID, userID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.SessionID, c.UserID
}

// closeClient ends a connection with a close frame. The read pump then
// fails, and unregisters the client. WriteControl may run concurrently with
// the write pump.
func (h *WebSocketHandler) closeClient(client *Client, code int, reason string) {
	if client.Conn == nil {
		return
	}
	_ = client.Conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), time.Now().Add(h.limits.writeTimeout))
	_ = client.Conn.Close()
}

// closeFor ends a connection whose authorization failed with err, with the
// close code and reason of that failure. It reports whether it closed it:
// an error that says nothing about the session, such as an unreachable
// database, leaves the connection open, and is retried at the next check.
func (h *WebSocketHandler) closeFor(client *Client, err error) bool {
	switch {
	case errors.Is(err, auth.ErrAccountUnavailable):
		h.closeClient(client, CloseForbidden, reasonAccountUnavailable)
	case errors.Is(err, auth.ErrSessionInvalid):
		h.closeClient(client, CloseUnauthorized, reasonSessionInactive)
	case errors.Is(err, auth.ErrInvalidToken), errors.Is(err, auth.ErrWrongTokenType), errors.Is(err, auth.ErrWrongRole):
		h.closeClient(client, CloseUnauthorized, reasonTokenInvalid)
	default:
		return false
	}
	return true
}

// StartSessionWatcher checks, every websocket.session_check_seconds, the
// sessions of the authenticated connections of this instance, and closes
// the connections of sessions that ended, by logout, revocation, or expiry,
// and of users who can no longer sign in (#189). Each instance watches only
// its own connections against the shared session store, so several
// instances need no coordination. It stops at Shutdown. Without
// VerifySession, it does nothing.
func (h *WebSocketHandler) StartSessionWatcher(ctx context.Context) {
	if h.VerifySession == nil {
		return
	}
	h.Wg.Add(1)
	go func() {
		defer h.Wg.Done()
		ticker := time.NewTicker(h.limits.sessionCheck)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-h.ShutdownCh:
				return
			case <-ticker.C:
				h.checkSessions(ctx)
			}
		}
	}()
}

// checkSessions checks each session of the authenticated connections once,
// and closes the connections of those that may no longer be used.
func (h *WebSocketHandler) checkSessions(ctx context.Context) {
	type principal struct{ session, user string }
	bySession := make(map[principal][]*Client)
	h.Mu.RLock()
	for client := range h.Clients {
		if session, user := client.principal(); user != "" {
			key := principal{session, user}
			bySession[key] = append(bySession[key], client)
		}
	}
	h.Mu.RUnlock()

	for p, clients := range bySession {
		checkCtx, cancel := context.WithTimeout(ctx, sessionCheckTimeout)
		err := h.VerifySession(checkCtx, p.session, p.user)
		cancel()
		if err == nil {
			continue
		}
		closed := false
		for _, client := range clients {
			closed = h.closeFor(client, err)
		}
		if !closed {
			log.Printf("WebSocket: could not check session %s, kept its connections: %v", p.session, err)
		}
	}
}

// handleReauth replaces the token of a connection with a newer one, which a
// client sends before its token expires: {"event": "reauth", "token": "..."}.
// The token must be valid for the user of the connection; an anonymous
// connection becomes authenticated. A token that is refused closes the
// connection, as it would refuse the upgrade.
func (h *WebSocketHandler) handleReauth(client *Client, event map[string]interface{}) {
	token, _ := event["token"].(string)
	if token == "" || h.Authenticate == nil {
		h.closeClient(client, CloseUnauthorized, reasonTokenInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionCheckTimeout)
	claims, err := h.Authenticate(ctx, token)
	cancel()
	if err != nil {
		if !h.closeFor(client, err) {
			log.Printf("WebSocket: could not check a reauth token: %v", err)
			h.send(client, map[string]interface{}{"event": "error", "code": "unavailable", "message": "Could not check the token; try again"})
		}
		return
	}
	if _, user := client.principal(); user != "" && user != claims.UUID {
		// A connection keeps its user; another user opens a connection of
		// its own.
		h.closeClient(client, CloseUnauthorized, reasonTokenInvalid)
		return
	}
	h.authorize(client, claims)
	reply := map[string]interface{}{"event": "reauth"}
	if claims.ExpiresAt != nil {
		reply["expires_at"] = claims.ExpiresAt.UTC().Format(time.RFC3339)
	}
	h.send(client, reply)
}

// send queues message for client, or drops it when its buffer is full.
func (h *WebSocketHandler) send(client *Client, message map[string]interface{}) {
	data, err := json.Marshal(message)
	if err != nil {
		log.Printf("WebSocket: could not encode a message: %v", err)
		return
	}
	select {
	case client.Send <- data:
	default:
		log.Printf("Client send buffer full, dropping %v", message["event"])
	}
}
