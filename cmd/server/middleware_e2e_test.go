package main

import (
	"encoding/json"
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
	routeForTest(t, router, env.handlers)
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
		assertErrorContract(t, w, name)
	}
	assert.True(t, seen["/system/live"], "the probes are checked")
	assert.True(t, seen["/swagger/*any"], "Swagger is served outside release mode")
}

// assertErrorContract checks the error contract of #53 on a response: an
// error in the JSON envelope has a code, and the request ID of its header.
// The health probes answer plain JSON without the envelope.
func assertErrorContract(t *testing.T, w *httptest.ResponseRecorder, name string) {
	t.Helper()
	if w.Code < http.StatusBadRequest || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		return
	}
	var body struct {
		Status    int    `json:"status"`
		Code      string `json:"code"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Status == 0 {
		return
	}
	assert.NotEmpty(t, body.Code, "%s: an error has a code", name)
	assert.Equal(t, w.Header().Get("X-Request-ID"), body.RequestID, "%s: an error quotes its request ID", name)
}

// TestRouter_UnknownRoutesAnswerInTheEnvelope checks that a path or a method
// that no route has gets the envelope, a code, and the request ID, instead of
// Gin's plain-text answer.
func TestRouter_UnknownRoutesAnswerInTheEnvelope(t *testing.T) {
	env := newE2EEnv(t)
	router := newTestRouter(t, env)
	for _, tc := range []struct {
		method, path, code string
		status             int
	}{
		{http.MethodGet, "/api/v1/no-such-route", "not_found", http.StatusNotFound},
		{http.MethodDelete, "/api/v1/public/currencies", "method_not_allowed", http.StatusMethodNotAllowed},
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		assert.Equal(t, tc.status, w.Code, tc.path)
		var body struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
		assert.Equal(t, tc.code, body.Code)
		assert.Equal(t, w.Header().Get("X-Request-ID"), body.RequestID)
		assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"), "the global middleware applies")
	}
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

// TestRouter_UnsupportedMethodsAnswer405 sends, to every path that the
// router lists, a method that the path does not support (#124). The answer
// is 405 with an Allow header that lists exactly the methods of the path.
func TestRouter_UnsupportedMethodsAnswer405(t *testing.T) {
	env := newE2EEnv(t)
	config.Config.RateLimits.REST.RequestsPerSecond, config.Config.RateLimits.REST.Burst = 1000, 1000
	router := newTestRouter(t, env)

	methods := map[string]map[string]bool{}
	for _, route := range router.Routes() {
		if methods[route.Path] == nil {
			methods[route.Path] = map[string]bool{}
		}
		methods[route.Path][route.Method] = true
	}
	require.NotEmpty(t, methods)
	candidates := []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	for path, allowed := range methods {
		unsupported := ""
		for _, method := range candidates {
			if !allowed[method] {
				unsupported = method
				break
			}
		}
		require.NotEmpty(t, unsupported, "%s supports every method", path)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(unsupported, concretePath(path), nil))
		name := unsupported + " " + path
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code, name)
		got := map[string]bool{}
		for _, method := range strings.Split(w.Header().Get("Allow"), ",") {
			got[strings.TrimSpace(method)] = true
		}
		assert.Equal(t, allowed, got, "%s: Allow lists the methods of the path", name)
		assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"), "%s: the global middleware applies", name)
	}
}
