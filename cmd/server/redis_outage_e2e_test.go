package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/config"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/testutil/tcpproxy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRedisOutage_RequestsAnswerWithinTheirDeadlines serves requests with the
// real services, but the rate limiters and the readiness probe reach Redis
// through a TCP proxy that stalls, as a hung Redis or a network that drops
// packets does: connections are accepted, but no command is answered. The
// probes must not wait for Redis, readiness must report it within its check
// timeout, and the limiters must decide within their deadline, as their
// on_error says.
func TestRedisOutage_RequestsAnswerWithinTheirDeadlines(t *testing.T) {
	env := newE2EEnv(t)
	proxy, err := tcpproxy.New("localhost:6379")
	require.NoError(t, err)
	t.Cleanup(proxy.Close)

	const limiterDeadline = 300 * time.Millisecond
	const checkTimeout = 2 * time.Second
	c := &config.Config
	c.Redis.Host, c.Redis.Port = proxy.Host(), proxy.Port()
	c.RateLimits.TimeoutMs = int(limiterDeadline / time.Millisecond)
	c.RateLimits.REST.RequestsPerSecond, c.RateLimits.REST.Burst = 100, 200
	c.RateLimits.REST.OnError = config.RateLimitAllow
	c.RateLimits.Auth.OTPRequestsPerHour = 100
	c.RateLimits.Auth.OnError = config.RateLimitDeny
	proxied, err := db.NewRedisService()
	require.NoError(t, err)
	t.Cleanup(func() { proxied.Close() })
	savedRedis, savedSystemRedis := env.handlers.Redis, env.handlers.System.Redis
	t.Cleanup(func() { env.handlers.Redis, env.handlers.System.Redis = savedRedis, savedSystemRedis })
	env.handlers.Redis, env.handlers.System.Redis = proxied, proxied
	router := newTestRouter(t, env)

	send := func(method, path string) (*httptest.ResponseRecorder, time.Duration) {
		req := httptest.NewRequest(method, path, strings.NewReader(`{"username": "outage@example.com"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		start := time.Now()
		router.ServeHTTP(w, req)
		return w, time.Since(start)
	}

	w, _ := send(http.MethodGet, "/")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "200", w.Header().Get("X-RateLimit-Limit"), "counted in Redis while it is up")

	proxy.SetStalled(true)
	proxy.Cut()

	probes := map[string]int{}
	for _, route := range router.Routes() {
		if strings.HasPrefix(route.Path, "/system/") {
			assert.Contains(t, probeRoutes, route.Path, "every probe is counted in memory")
			probes[route.Path] = 0
		}
	}
	require.NotEmpty(t, probes)
	for _, path := range []string{"/system/live", "/system/ping"} {
		w, took := send(http.MethodGet, path)
		assert.Equal(t, http.StatusOK, w.Code, path)
		assert.Less(t, took, 100*time.Millisecond, "%s does not wait for Redis", path)
		assert.NotEmpty(t, w.Header().Get("X-RateLimit-Limit"), "%s is still rate limited", path)
	}
	for _, path := range []string{"/system/ready", "/system/healthcheck"} {
		w, took := send(http.MethodGet, path)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code, path)
		assert.Contains(t, w.Body.String(), `"redis":{"status":"error"`, path)
		assert.Less(t, took, checkTimeout+500*time.Millisecond, "%s answers within its check timeout", path)
	}

	w, took := send(http.MethodGet, "/")
	assert.Equal(t, http.StatusOK, w.Code, "the REST limit lets requests through, as on_error says")
	assert.Empty(t, w.Header().Get("X-RateLimit-Limit"), "the request was not counted")
	assert.GreaterOrEqual(t, took, limiterDeadline, "the limiter waited for Redis")
	assert.Less(t, took, limiterDeadline+500*time.Millisecond, "but not past its deadline")

	w, took = send(http.MethodPost, "/public/auth")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "the sign-in limit rejects requests, as on_error says")
	assert.Equal(t, "1", w.Header().Get("Retry-After"))
	assert.GreaterOrEqual(t, took, limiterDeadline)
	assert.Less(t, took, 2*limiterDeadline, "the REST and sign-in limits wait for Redis once together")

	proxy.SetStalled(false)
	proxy.Cut()
	assert.Eventually(t, func() bool {
		w, _ := send(http.MethodGet, "/")
		return w.Header().Get("X-RateLimit-Limit") == "200"
	}, 10*time.Second, 50*time.Millisecond, "counted in Redis again once it answers")
	w, _ = send(http.MethodGet, "/system/ready")
	assert.Equal(t, http.StatusOK, w.Code, "ready again")
}
