package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestRouter builds the server's router for the end-to-end environment,
// with the configuration as the test has set it.
func newTestRouter(t *testing.T, env *e2eEnv) *gin.Engine {
	t.Helper()
	router, err := newEngine()
	require.NoError(t, err)
	require.NoError(t, RouteHandler(router, env.handlers))
	return router
}

// concretePath turns a route pattern into a path that the route matches.
func concretePath(pattern string) string {
	parts := strings.Split(pattern, "/")
	for i, part := range parts {
		switch {
		case strings.HasPrefix(part, ":"):
			parts[i] = uuid.NewString()
		case strings.HasPrefix(part, "*"):
			parts[i] = "index.html"
		}
	}
	return strings.Join(parts, "/")
}

// TestRouter_EveryRouteGetsTheGlobalMiddleware sends a request to every
// registered route. Gin applies middleware only to the routes registered
// after it, so a route registered too early would miss the security headers,
// the rate limit, and the request ID, as / and /swagger did.
func TestRouter_EveryRouteGetsTheGlobalMiddleware(t *testing.T) {
	env := newE2EEnv(t)
	config.Config.RateLimits.REST.RequestsPerSecond, config.Config.RateLimits.REST.Burst = 1000, 1000
	router := newTestRouter(t, env)

	routes := router.Routes()
	require.NotEmpty(t, routes)
	seen := map[string]bool{}
	for _, route := range routes {
		seen[route.Path] = true
		req := httptest.NewRequest(route.Method, concretePath(route.Path), strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		name := route.Method + " " + route.Path
		assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"), "%s: security headers", name)
		assert.NotEmpty(t, w.Header().Get("X-RateLimit-Limit"), "%s: REST rate limit", name)
		assert.NotEmpty(t, w.Header().Get("X-Request-ID"), "%s: request ID", name)
	}
	assert.True(t, seen["/"], "the root route is checked")
	assert.True(t, seen["/swagger/*any"], "Swagger is served outside release mode")
}

func TestRouter_SwaggerOnlyWhereEnabled(t *testing.T) {
	env := newE2EEnv(t)
	swagger := func() int {
		w := httptest.NewRecorder()
		newTestRouter(t, env).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/swagger/index.html", nil))
		return w.Code
	}

	config.Config.Server.Mode = "debug"
	assert.Equal(t, http.StatusOK, swagger(), "served outside release mode")

	config.Config.Server.Mode = "release"
	assert.Equal(t, http.StatusNotFound, swagger(), "not served in release mode")

	enabled := true
	config.Config.Server.Swagger.Enabled = &enabled
	assert.Equal(t, http.StatusOK, swagger(), "served in release mode when enabled")
}

func TestRouter_RejectsOversizedBodies(t *testing.T) {
	env := newE2EEnv(t)
	config.Config.Server.MaxBodyBytes = 1024
	router := newTestRouter(t, env)

	body := `{"username": "` + strings.Repeat("a", 2048) + `@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/public/auth", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)

	req = httptest.NewRequest(http.MethodPost, "/api/v1/public/auth", strings.NewReader(`{"username": "small@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.NotEqual(t, http.StatusRequestEntityTooLarge, w.Code, "a body under the limit is handled")
}
