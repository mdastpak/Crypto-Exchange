package socket

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/auth"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// expectClose reads from conn until the server closes it, and returns the
// close code and reason.
func expectClose(t *testing.T, conn *websocket.Conn) (int, string) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		var closeErr *websocket.CloseError
		require.True(t, errors.As(err, &closeErr), "the connection must end with a close frame, got %v", err)
		return closeErr.Code, closeErr.Text
	}
}

// readEvent reads the next message whose event is name.
func readEvent(t *testing.T, conn *websocket.Conn, name string) map[string]interface{} {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	for {
		_, data, err := conn.ReadMessage()
		require.NoError(t, err)
		var event map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &event))
		if event["event"] == name {
			return event
		}
	}
}

// expiringAuth accepts tokens of the form "user|lifetime", such as
// "user-1|100ms", for the user and session of that name.
func expiringAuth(ctx context.Context, token string) (*auth.Claims, error) {
	for i := 0; i < len(token); i++ {
		if token[i] != '|' {
			continue
		}
		lifetime, err := time.ParseDuration(token[i+1:])
		if err != nil {
			return nil, auth.ErrInvalidToken
		}
		user := token[:i]
		return &auth.Claims{UUID: user, SessionID: "session-of-" + user, RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(lifetime)),
		}}, nil
	}
	return fakeAuth(ctx, token)
}

func TestConnection_ClosesWhenItsTokenExpires(t *testing.T) {
	h := newTestHandler()
	h.Authenticate = expiringAuth
	conn, _, err := dial(t, gateway(t, h), http.Header{"Authorization": {"Bearer user-1|1s"}})
	require.NoError(t, err)

	start := time.Now()
	code, reason := expectClose(t, conn)
	assert.Equal(t, CloseUnauthorized, code)
	assert.Equal(t, "token_expired", reason)
	// JWT expiry has a resolution of a second.
	assert.Less(t, time.Since(start), 2500*time.Millisecond)
	require.Eventually(t, func() bool { return h.clientCount() == 0 }, 2*time.Second, 5*time.Millisecond,
		"a closed connection must be unregistered")
}

func TestConnection_ReauthExtendsItsLife(t *testing.T) {
	h := newTestHandler()
	h.Authenticate = expiringAuth
	conn, _, err := dial(t, gateway(t, h), http.Header{"Authorization": {"Bearer user-1|2s"}})
	require.NoError(t, err)

	require.NoError(t, conn.WriteJSON(map[string]string{"event": "reauth", "token": "user-1|1h"}))
	reply := readEvent(t, conn, "reauth")
	expiresAt, err := time.Parse(time.RFC3339, reply["expires_at"].(string))
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(time.Hour), expiresAt, 5*time.Second)

	// The first token has expired, and the connection is still open.
	time.Sleep(2200 * time.Millisecond)
	require.NoError(t, conn.WriteJSON(map[string]string{"event": "ping"}))
	readEvent(t, conn, "pong")
}

func TestConnection_ReauthCanAuthenticateAnAnonymousConnection(t *testing.T) {
	h := newTestHandler()
	h.Authenticate = expiringAuth
	conn, _, err := dial(t, gateway(t, h), nil)
	require.NoError(t, err)
	client := onlyClient(t, h)

	require.NoError(t, conn.WriteJSON(map[string]string{"event": "reauth", "token": "user-1|1h"}))
	readEvent(t, conn, "reauth")
	session, user := client.principal()
	assert.Equal(t, "user-1", user)
	assert.Equal(t, "session-of-user-1", session)
}

func TestConnection_ReauthRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		message    map[string]string
		wantCode   int
		wantReason string
	}{
		"another user":            {map[string]string{"event": "reauth", "token": "user-2|1h"}, CloseUnauthorized, "token_invalid"},
		"no token":                {map[string]string{"event": "reauth"}, CloseUnauthorized, "token_invalid"},
		"invalid token":           {map[string]string{"event": "reauth", "token": "forged"}, CloseUnauthorized, "token_invalid"},
		"ended session":           {map[string]string{"event": "reauth", "token": "ended"}, CloseUnauthorized, "session_inactive"},
		"user who cannot sign in": {map[string]string{"event": "reauth", "token": "disabled"}, CloseForbidden, "account_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newTestHandler()
			h.Authenticate = expiringAuth
			conn, _, err := dial(t, gateway(t, h), http.Header{"Authorization": {"Bearer user-1|1h"}})
			require.NoError(t, err)

			require.NoError(t, conn.WriteJSON(tc.message))
			code, reason := expectClose(t, conn)
			assert.Equal(t, tc.wantCode, code)
			assert.Equal(t, tc.wantReason, reason)
		})
	}

	t.Run("an unreachable authenticator keeps the connection", func(t *testing.T) {
		h := newTestHandler()
		h.Authenticate = expiringAuth
		conn, _, err := dial(t, gateway(t, h), http.Header{"Authorization": {"Bearer user-1|1h"}})
		require.NoError(t, err)

		require.NoError(t, conn.WriteJSON(map[string]string{"event": "reauth", "token": "broken"}))
		reply := readEvent(t, conn, "error")
		assert.Equal(t, "unavailable", reply["code"])
		require.NoError(t, conn.WriteJSON(map[string]string{"event": "ping"}))
		readEvent(t, conn, "pong")
	})
}

// sessionState is a session store for the watcher: each session maps to
// the error that Verify returns for it.
type sessionState struct {
	mu     sync.Mutex
	errs   map[string]error
	checks map[string]int
}

func (s *sessionState) set(session string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs[session] = err
}

func (s *sessionState) verify(ctx context.Context, sessionID, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks[sessionID]++
	return s.errs[sessionID]
}

func (s *sessionState) count(session string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checks[session]
}

func TestSessionWatcher_ClosesTheConnectionsOfEndedSessions(t *testing.T) {
	state := &sessionState{errs: map[string]error{}, checks: map[string]int{}}
	h := newTestHandler()
	h.Authenticate = expiringAuth
	h.VerifySession = state.verify
	h.limits.sessionCheck = 20 * time.Millisecond
	url := gateway(t, h)

	open := func(token string) *websocket.Conn {
		conn, _, err := dial(t, url, http.Header{"Authorization": {"Bearer " + token}})
		require.NoError(t, err)
		return conn
	}
	revoked1, revoked2 := open("user-1|1h"), open("user-1|1h")
	suspended := open("user-2|1h")
	flaky := open("user-3|1h")
	healthy := open("user-4|1h")
	anonymous, _, err := dial(t, url, nil)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return h.clientCount() == 6 }, 2*time.Second, 5*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.StartSessionWatcher(ctx)

	state.set("session-of-user-1", auth.ErrSessionInvalid)
	state.set("session-of-user-2", auth.ErrAccountUnavailable)
	state.set("session-of-user-3", errors.New("database down"))

	for _, conn := range []*websocket.Conn{revoked1, revoked2} {
		code, reason := expectClose(t, conn)
		assert.Equal(t, CloseUnauthorized, code)
		assert.Equal(t, "session_inactive", reason)
	}
	code, reason := expectClose(t, suspended)
	assert.Equal(t, CloseForbidden, code)
	assert.Equal(t, "account_unavailable", reason)

	// An error that says nothing about the session keeps the connection,
	// and the session is checked again.
	require.Eventually(t, func() bool { return state.count("session-of-user-3") >= 3 }, 2*time.Second, 5*time.Millisecond)
	for _, conn := range []*websocket.Conn{flaky, healthy, anonymous} {
		require.NoError(t, conn.WriteJSON(map[string]string{"event": "ping"}))
		readEvent(t, conn, "pong")
	}
	require.Eventually(t, func() bool { return h.clientCount() == 3 }, 2*time.Second, 5*time.Millisecond)
}

func TestSessionWatcher_ChecksEachSessionOncePerRound(t *testing.T) {
	var calls atomic.Int32
	h := newTestHandler()
	h.Authenticate = expiringAuth
	h.VerifySession = func(ctx context.Context, sessionID, userID string) error {
		calls.Add(1)
		return nil
	}
	url := gateway(t, h)
	for i := 0; i < 3; i++ {
		_, _, err := dial(t, url, http.Header{"Authorization": {"Bearer user-1|1h"}})
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool { return h.clientCount() == 3 }, 2*time.Second, 5*time.Millisecond)

	h.checkSessions(context.Background())
	assert.Equal(t, int32(1), calls.Load(), "three connections of one session need one lookup")
}

func TestSessionWatcher_StopsAtShutdown(t *testing.T) {
	h := newTestHandler()
	h.VerifySession = func(ctx context.Context, sessionID, userID string) error { return nil }
	h.limits.sessionCheck = time.Hour
	h.StartSessionWatcher(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, h.Shutdown(ctx), "Shutdown waits for the watcher, which must stop")
}

func TestSessionWatcher_DoesNothingWithoutAVerifier(t *testing.T) {
	h := newTestHandler()
	h.StartSessionWatcher(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, h.Shutdown(ctx))
}
