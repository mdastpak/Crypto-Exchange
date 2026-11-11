package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/dberr"
	"github.com/mdastpak/crypto-exchange/internal/testutil/tcpproxy"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRedisUnavailable_RequestsAnswer503 runs the server with Redis behind a
// proxy, signs a user and an administrator in, and then takes Redis away
// (#130). The routes that need Redis still refuse the request, and a token
// whose revocation cannot be checked is still not accepted, but they answer
// 503 service_unavailable with Retry-After, not 500. The currency reads,
// which fall back to PostgreSQL, keep answering.
func TestRedisUnavailable_RequestsAnswer503(t *testing.T) {
	proxy, err := tcpproxy.New("localhost:6379")
	require.NoError(t, err)
	t.Cleanup(proxy.Close)
	env := newE2EEnv(t, func(c *config.Configuration) {
		c.Redis.Host, c.Redis.Port = proxy.Host(), proxy.Port()
		c.Redis.MaxRetries = -1 // no retries, for a quick test
	})
	ctx := context.Background()
	// Redis comes back before the cleanup of the environment flushes it.
	t.Cleanup(func() { proxy.SetDown(false) })

	userToken, _ := env.signedInUser(t, "redis-down@example.com")
	enrollment, err := env.accounts.Create(ctx, "redis-down-admin@example.com")
	require.NoError(t, err)
	adminToken := env.signInAdmin(t, enrollment)
	status, _ := env.call(t, http.MethodGet, "/api/v1/user/profile", nil, userToken)
	require.Equal(t, http.StatusOK, status, "the tokens work while Redis is up")

	proxy.SetDown(true)
	proxy.Cut()

	// The currency reads do not need Redis: they read PostgreSQL when the
	// cache cannot be reached.
	status, body := env.call(t, http.MethodGet, "/api/v1/public/currencies", nil, "")
	assert.Equal(t, http.StatusOK, status, string(body))
	status, body = env.call(t, http.MethodGet, "/api/v1/public/currencies/"+uuid.NewString(), nil, "")
	assert.Equal(t, http.StatusNotFound, status, string(body))

	for _, route := range []struct{ name, path, token string }{
		{"user route, revocation unknown", "/api/v1/user/profile", userToken},
		{"admin route, revocation unknown", "/api/v1/admin/currencies", adminToken},
	} {
		t.Run(route.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, route.path, nil)
			if route.token != "" {
				req.Header.Set("Authorization", "Bearer "+route.token)
			}
			w := httptest.NewRecorder()
			env.router.ServeHTTP(w, req)
			assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
			assert.Equal(t, "1", w.Header().Get("Retry-After"))
			var resp e2eResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			assert.Equal(t, dberr.CodeServiceUnavailable, resp.Code, "the answer of an unreachable Redis, not of a rate limiter")
			assert.Nil(t, resp.Info, "nothing of the route is answered")
		})
	}

	proxy.SetDown(false)
	assert.Eventually(t, func() bool {
		status, _ := env.call(t, http.MethodGet, "/api/v1/user/profile", nil, userToken)
		return status == http.StatusOK
	}, 10*time.Second, 50*time.Millisecond, "the same token works again once Redis is back")
}
