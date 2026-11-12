package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
)

// TestRouter_SecurityHeaders checks the headers of #191 on the server's
// router: no caching, the content security policies, HSTS, and the CORS
// headers that clients read.
func TestRouter_SecurityHeaders(t *testing.T) {
	env := newE2EEnv(t)
	config.Config.RateLimits.REST.RequestsPerSecond, config.Config.RateLimits.REST.Burst = 1000, 1000
	config.Config.Server.CORS.AllowedOrigins = []string{"https://app.example.com"}
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Origin", "https://app.example.com")
		newTestRouter(t, env).ServeHTTP(w, req)
		return w
	}

	w := get("/api/v1/public/currencies")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.Equal(t, "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'", w.Header().Get("Content-Security-Policy"))
	assert.Equal(t, "0", w.Header().Get("X-XSS-Protection"))
	assert.Empty(t, w.Header().Get("Strict-Transport-Security"), "no HSTS without TLS")
	assert.Contains(t, w.Header().Get("Access-Control-Expose-Headers"), "X-Request-ID")
	assert.Contains(t, w.Header().Get("Access-Control-Expose-Headers"), "Retry-After")

	w = get("/swagger/index.html")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Security-Policy"), "script-src 'self' 'unsafe-inline'")

	config.Config.Server.TLS.BehindProxy = true
	assert.Equal(t, "max-age=31536000", get("/system/live").Header().Get("Strict-Transport-Security"), "HSTS behind a TLS proxy")
	disabled := false
	config.Config.Server.HSTS.Enabled = &disabled
	assert.Empty(t, get("/system/live").Header().Get("Strict-Transport-Security"), "HSTS can be turned off")
}

// Preflight requests are counted by the REST rate limit, like any other
// request (#191).
func TestRouter_PreflightsAreRateLimited(t *testing.T) {
	env := newE2EEnv(t)
	config.Config.RateLimits.REST.RequestsPerSecond, config.Config.RateLimits.REST.Burst = 1, 3
	config.Config.Server.CORS.AllowedOrigins = []string{"https://app.example.com"}
	router := newTestRouter(t, env)

	var codes []int
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodOptions, "/api/v1/user/profile", nil)
		req.Header.Set("Origin", "https://app.example.com")
		req.Header.Set("Access-Control-Request-Method", http.MethodGet)
		req.RemoteAddr = "203.0.113.77:40000"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		codes = append(codes, w.Code)
		if w.Code == http.StatusNoContent {
			assert.Equal(t, "https://app.example.com", w.Header().Get("Access-Control-Allow-Origin"))
		}
	}
	assert.Equal(t, []int{http.StatusNoContent, http.StatusNoContent, http.StatusNoContent}, codes[:3])
	assert.Contains(t, codes[3:], http.StatusTooManyRequests)
}
