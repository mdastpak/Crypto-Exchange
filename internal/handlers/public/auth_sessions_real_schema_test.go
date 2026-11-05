package public

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sessionTestEnv wires the sign-in, refresh and logout routes, plus one
// protected route, to a database built by the real migrations.
type sessionTestEnv struct {
	router      *gin.Engine
	conn        *sql.DB
	redisClient *redis.Client
	sender      *recordingOTPSender
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
	router.GET("/user/ping", auth.JWTUserVerification(blacklist), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	return &sessionTestEnv{router: router, conn: conn, redisClient: redisClient, sender: sender}
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
