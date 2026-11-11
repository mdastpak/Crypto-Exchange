package app

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRateLimits_PerRouteGroup checks which limits the router applies to the
// public and sign-in routes, with limits small enough to reach. Each client
// IP has budgets of its own, so the subtests use different addresses where
// they must not affect each other.
func TestRateLimits_PerRouteGroup(t *testing.T) {
	env := newE2EEnv(t)
	limits := &config.Config.RateLimits
	limits.REST.RequestsPerSecond, limits.REST.Burst = 1, 40
	limits.Auth.LoginAttemptsPerMinute = 3
	limits.Auth.OTPRequestsPerHour = 2
	router, err := newEngine()
	require.NoError(t, err)
	routeForTest(t, router, env.handlers)

	send := func(method, path, ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = ip + ":40000"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	// allowedBeforeLimit sends requests until one is rejected and returns how
	// many were let through, and the rejection.
	allowedBeforeLimit := func(method, path, ip string, max int) (int, *httptest.ResponseRecorder) {
		for i := 0; i < max; i++ {
			if w := send(method, path, ip); w.Code == http.StatusTooManyRequests {
				return i, w
			}
		}
		return max, nil
	}
	retryAfter := func(w *httptest.ResponseRecorder) int {
		seconds, err := strconv.Atoi(w.Header().Get("Retry-After"))
		require.NoError(t, err)
		return seconds
	}

	t.Run("public reads use only the REST limit", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			w := send(http.MethodGet, "/api/v1/public/currencies", "198.51.100.1")
			require.Equal(t, http.StatusOK, w.Code, "request %d", i+1)
			assert.Equal(t, "40", w.Header().Get("X-RateLimit-Limit"), "the REST limit is the last one applied")
			assert.Equal(t, http.StatusNotFound, send(http.MethodGet, "/api/v1/public/currencies/"+uuid.NewString(), "198.51.100.1").Code)
		}
	})

	t.Run("the REST limit applies to public reads", func(t *testing.T) {
		allowed, rejected := allowedBeforeLimit(http.MethodGet, "/api/v1/public/currencies", "198.51.100.2", 60)
		require.NotNil(t, rejected, "the REST burst must be enforced")
		assert.GreaterOrEqual(t, allowed, 40)
	})

	const signIn = "198.51.100.3"
	t.Run("code requests are limited per hour", func(t *testing.T) {
		allowed, rejected := allowedBeforeLimit(http.MethodPost, "/api/v1/public/auth", signIn, 5)
		require.NotNil(t, rejected)
		assert.Equal(t, 2, allowed)
		assert.Greater(t, retryAfter(rejected), 60)
	})

	t.Run("code verifications have a limit of their own", func(t *testing.T) {
		allowed, rejected := allowedBeforeLimit(http.MethodPost, "/api/v1/public/auth/"+uuid.NewString(), signIn, 5)
		require.NotNil(t, rejected)
		assert.Equal(t, 3, allowed, "used-up code requests must not block verification")
		assert.LessOrEqual(t, retryAfter(rejected), 60)
	})

	t.Run("admin sign-in counts separately from user sign-in", func(t *testing.T) {
		allowed, rejected := allowedBeforeLimit(http.MethodPost, "/api/v1/admin/auth", signIn, 5)
		require.NotNil(t, rejected)
		assert.Equal(t, 2, allowed)
		allowed, rejected = allowedBeforeLimit(http.MethodPost, "/api/v1/admin/auth/"+uuid.NewString(), signIn, 5)
		require.NotNil(t, rejected)
		assert.Equal(t, 3, allowed)
	})

	t.Run("refresh and logout have no sign-in limit", func(t *testing.T) {
		for _, path := range []string{"/api/v1/public/auth/refresh", "/api/v1/public/auth/logout"} {
			allowed, _ := allowedBeforeLimit(http.MethodPost, path, signIn, 6)
			assert.Equal(t, 6, allowed, path)
		}
	})
}
