package public

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sessionTestEnv wires the sign-in, refresh and logout routes, plus one
// protected route behind both user middlewares, to a database built by the
// real migrations.
type sessionTestEnv struct {
	router      *gin.Engine
	conn        *sql.DB
	redisClient *redis.Client
	sender      *recordingOTPSender
	sessions    auth.SessionStore
}

func newSessionTestEnv(t *testing.T) *sessionTestEnv {
	t.Helper()
	handler, conn, redisClient := setupRealSchemaHandler(t)
	sender := &recordingOTPSender{}
	handler.OTPSender = sender
	blacklist := auth.NewTokenBlacklist(redisClient)

	router := gin.New()
	router.POST("/public/auth", handler.RequestOTP)
	router.POST("/public/auth/:uuid", handler.VerifyOTP)
	router.POST("/public/auth/refresh", handler.RefreshToken)
	router.POST("/public/auth/logout", auth.JWTUserVerification(blacklist), func(c *gin.Context) {
		auth.LogoutHandler(c, blacklist, handler.Sessions)
	})
	router.GET("/user/ping", auth.JWTUserVerification(blacklist), auth.RequireActiveSession(handler.Sessions), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	return &sessionTestEnv{router: router, conn: conn, redisClient: redisClient, sender: sender, sessions: handler.Sessions}
}

type requestOption func(*http.Request)

// withAuthorization sends token with the Bearer scheme, which every protected
// route requires.
func withAuthorization(token string) requestOption {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func withRefreshCookie(token string) requestOption {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: auth.RefreshTokenCookie, Value: token}) }
}

func (e *sessionTestEnv) send(method, path string, body interface{}, opts ...requestOption) *httptest.ResponseRecorder {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	for _, opt := range opts {
		opt(req)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// signIn completes the OTP flow and returns the access token, the refresh
// token cookie, and the sign-in response.
func (e *sessionTestEnv) signIn(t *testing.T, contact string) (string, *http.Cookie, *httptest.ResponseRecorder) {
	t.Helper()
	w := e.send(http.MethodPost, "/public/auth", map[string]string{"username": contact})
	require.Equal(t, http.StatusOK, w.Code)
	var resp models.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	sessionID := resp.Info.(map[string]interface{})["uuid"].(string)

	w = e.send(http.MethodPost, "/public/auth/"+sessionID, map[string]string{"otp": lastSentCode(t, e.sender)})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	accessToken := resp.Info.(map[string]interface{})["access_token"].(string)

	cookie := findRefreshCookie(w)
	require.NotNil(t, cookie, "sign-in must set the refresh token cookie")
	return accessToken, cookie, w
}

func findRefreshCookie(w *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == auth.RefreshTokenCookie {
			return cookie
		}
	}
	return nil
}

type storedSession struct {
	status, authMethod, accessHash, refreshHash string
	revoked                                     bool
}

func (e *sessionTestEnv) sessionOf(t *testing.T, email string) storedSession {
	t.Helper()
	var s storedSession
	require.NoError(t, e.conn.QueryRow(`
		SELECT s.status, s.auth_method, s.access_token_hash, s.refresh_token_hash, s.revoked_at IS NOT NULL
		FROM sessions s JOIN users u ON u.hk = s.user_id
		JOIN user_contacts c ON c.user_hk = u.hk
		WHERE c.type = 'email' AND c.value = $1`, email,
	).Scan(&s.status, &s.authMethod, &s.accessHash, &s.refreshHash, &s.revoked))
	return s
}

func TestSession_RealSchema_SignInCreatesSessionAndCookie(t *testing.T) {
	env := newSessionTestEnv(t)
	const email = "session-signin@example.com"

	accessToken, cookie, w := env.signIn(t, email)

	assert.NotContains(t, w.Body.String(), "refresh_token", "the refresh token must not appear in the body")
	assert.True(t, cookie.HttpOnly)
	assert.True(t, cookie.Secure)
	assert.Equal(t, http.SameSiteStrictMode, cookie.SameSite)
	assert.Equal(t, "/public/auth", cookie.Path)
	assert.Equal(t, 60*60, cookie.MaxAge)

	session := env.sessionOf(t, email)
	assert.Equal(t, "ACTIVE", session.status)
	assert.Equal(t, "OTP", session.authMethod)
	assert.Equal(t, auth.HashToken(accessToken), session.accessHash, "only token hashes are stored")
	assert.Equal(t, auth.HashToken(cookie.Value), session.refreshHash)
}

func TestSession_RealSchema_RefreshTokenCannotCallTheAPI(t *testing.T) {
	env := newSessionTestEnv(t)
	accessToken, cookie, _ := env.signIn(t, "session-types@example.com")

	assert.Equal(t, http.StatusOK, env.send(http.MethodGet, "/user/ping", nil, withAuthorization(accessToken)).Code)
	assert.Equal(t, http.StatusUnauthorized, env.send(http.MethodGet, "/user/ping", nil, withAuthorization(cookie.Value)).Code,
		"a refresh token must not authorize API requests")
	assert.Equal(t, http.StatusUnauthorized, env.send(http.MethodPost, "/public/auth/refresh", nil, withRefreshCookie(accessToken)).Code,
		"an access token must not be accepted as a refresh token")
}

func TestSession_RealSchema_RefreshRotatesAndDetectsReuse(t *testing.T) {
	env := newSessionTestEnv(t)
	const email = "session-rotation@example.com"
	_, firstCookie, _ := env.signIn(t, email)

	w := env.send(http.MethodPost, "/public/auth/refresh", nil, withRefreshCookie(firstCookie.Value))
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "refresh_token")
	var resp models.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	newAccessToken := resp.Info.(map[string]interface{})["access_token"].(string)
	secondCookie := findRefreshCookie(w)
	require.NotNil(t, secondCookie)
	assert.NotEqual(t, firstCookie.Value, secondCookie.Value, "the refresh token must be rotated")
	assert.Equal(t, http.StatusOK, env.send(http.MethodGet, "/user/ping", nil, withAuthorization(newAccessToken)).Code)
	assert.Equal(t, auth.HashToken(secondCookie.Value), env.sessionOf(t, email).refreshHash)

	// Replaying the rotated token is treated as theft: the session is revoked.
	w = env.send(http.MethodPost, "/public/auth/refresh", nil, withRefreshCookie(firstCookie.Value))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	session := env.sessionOf(t, email)
	assert.Equal(t, "REVOKED", session.status)
	assert.True(t, session.revoked)

	assert.Equal(t, http.StatusUnauthorized, env.send(http.MethodPost, "/public/auth/refresh", nil, withRefreshCookie(secondCookie.Value)).Code,
		"after reuse is detected, the latest refresh token must stop working too")
}

func TestSession_RealSchema_ConcurrentRefreshesWithOneTokenSucceedOnce(t *testing.T) {
	env := newSessionTestEnv(t)
	const email = "session-concurrent@example.com"
	_, cookie, _ := env.signIn(t, email)
	const requests = 5

	codes := make([]int, requests)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = env.send(http.MethodPost, "/public/auth/refresh", nil, withRefreshCookie(cookie.Value)).Code
		}(i)
	}
	close(start)
	wg.Wait()

	succeeded := 0
	for _, code := range codes {
		if code == http.StatusOK {
			succeeded++
		} else {
			assert.Equal(t, http.StatusUnauthorized, code)
		}
	}
	assert.Equal(t, 1, succeeded, "one refresh token must be exchanged at most once")
	assert.Equal(t, "REVOKED", env.sessionOf(t, email).status, "the replayed requests count as reuse and revoke the session")
}

func TestSession_RealSchema_LogoutEndsTheSession(t *testing.T) {
	env := newSessionTestEnv(t)
	const email = "session-logout@example.com"
	accessToken, cookie, _ := env.signIn(t, email)

	w := env.send(http.MethodPost, "/public/auth/logout", nil, withAuthorization(accessToken))
	require.Equal(t, http.StatusOK, w.Code)
	cleared := findRefreshCookie(w)
	require.NotNil(t, cleared)
	assert.Negative(t, cleared.MaxAge, "logout must clear the refresh token cookie")

	session := env.sessionOf(t, email)
	assert.Equal(t, "REVOKED", session.status)
	assert.True(t, session.revoked)
	assert.Equal(t, http.StatusUnauthorized, env.send(http.MethodGet, "/user/ping", nil, withAuthorization(accessToken)).Code,
		"the access token must be rejected after logout")
	assert.Equal(t, http.StatusUnauthorized, env.send(http.MethodPost, "/public/auth/refresh", nil, withRefreshCookie(cookie.Value)).Code,
		"the refresh token must be rejected after logout")
}

func TestSession_RealSchema_VerifyChecksTheSessionAndTheUser(t *testing.T) {
	env := newSessionTestEnv(t)
	accessToken, _, _ := env.signIn(t, "session-verify@example.com")
	claims, err := auth.ParseToken(accessToken, auth.RoleUser, auth.TokenTypeAccess)
	require.NoError(t, err)
	ctx := context.Background()
	verify := func() error { return env.sessions.Verify(ctx, claims.SessionID, claims.UUID) }
	exec := func(query string, arg string) {
		t.Helper()
		_, err := env.conn.Exec(query, arg)
		require.NoError(t, err)
	}
	restore := func() {
		exec(`UPDATE sessions SET status = 'ACTIVE', revoked_at = NULL, expires_at = NOW() + INTERVAL '1 hour' WHERE id = $1`, claims.SessionID)
		exec(`UPDATE users SET status = 'ACTIVE', role = 'user', deleted_at = NULL WHERE hk = $1`, claims.UUID)
	}

	require.NoError(t, verify(), "a new session of an active user is valid")
	assert.ErrorIs(t, env.sessions.Verify(ctx, uuid.NewString(), claims.UUID), auth.ErrSessionInvalid, "an unknown session")
	assert.ErrorIs(t, env.sessions.Verify(ctx, claims.SessionID, uuid.NewString()), auth.ErrSessionInvalid, "the session of another user")

	for name, change := range map[string]string{
		"revoked":         `UPDATE sessions SET status = 'REVOKED', revoked_at = NOW() WHERE id = $1`,
		"marked expired":  `UPDATE sessions SET status = 'EXPIRED' WHERE id = $1`,
		"past its expiry": `UPDATE sessions SET expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`,
	} {
		t.Run("session "+name, func(t *testing.T) {
			t.Cleanup(restore)
			exec(change, claims.SessionID)
			assert.ErrorIs(t, verify(), auth.ErrSessionInvalid)
		})
	}

	for name, change := range map[string]string{
		"inactive":      `UPDATE users SET status = 'INACTIVE' WHERE hk = $1`,
		"suspended":     `UPDATE users SET status = 'SUSPENDED' WHERE hk = $1`,
		"banned":        `UPDATE users SET status = 'BANNED' WHERE hk = $1`,
		"soft-deleted":  `UPDATE users SET deleted_at = NOW() WHERE hk = $1`,
		"administrator": `UPDATE users SET role = 'admin' WHERE hk = $1`,
	} {
		t.Run("user "+name, func(t *testing.T) {
			t.Cleanup(restore)
			exec(change, claims.UUID)
			assert.ErrorIs(t, verify(), auth.ErrAccountUnavailable)
			assert.NotErrorIs(t, verify(), auth.ErrSessionInvalid, "the client must not mistake it for an expired session")
		})
	}

	t.Run("user pending verification", func(t *testing.T) {
		t.Cleanup(restore)
		exec(`UPDATE users SET status = 'PENDING_VERIFICATION' WHERE hk = $1`, claims.UUID)
		assert.NoError(t, verify(), "a pending user may sign in, so its session stays valid")
	})

	t.Run("revoked session of a suspended user", func(t *testing.T) {
		t.Cleanup(restore)
		exec(`UPDATE sessions SET status = 'REVOKED', revoked_at = NOW() WHERE id = $1`, claims.SessionID)
		exec(`UPDATE users SET status = 'SUSPENDED' WHERE hk = $1`, claims.UUID)
		assert.ErrorIs(t, verify(), auth.ErrSessionInvalid, "a dead session is reported first")
	})
}
