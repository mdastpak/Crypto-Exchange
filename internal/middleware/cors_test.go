package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// corsResponse sends a request with the given method and Origin through a
// CORS middleware built from allowedOrigins.
func corsResponse(t *testing.T, allowedOrigins []string, method, origin string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cors, err := NewCORSMiddleware(allowedOrigins)
	require.NoError(t, err)

	engine := gin.New()
	engine.Use(cors, Preflight())
	engine.Any("/resource", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(method, "/resource", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func TestCORS_EmptyListGrantsNoCrossOriginAccess(t *testing.T) {
	w := corsResponse(t, nil, http.MethodGet, "https://evil.example")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"), "an empty list must not reflect any origin")
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Credentials"))
}

func TestCORS_AllowsOnlyListedOrigins(t *testing.T) {
	allowed := []string{"https://app.example.com", "http://localhost:3000"}

	w := corsResponse(t, allowed, http.MethodGet, "https://app.example.com")
	assert.Equal(t, "https://app.example.com", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "true", w.Header().Get("Access-Control-Allow-Credentials"))
	assert.Contains(t, w.Header().Values("Vary"), "Origin", "caches must not reuse a response for another origin")

	w = corsResponse(t, allowed, http.MethodGet, "http://localhost:3000")
	assert.Equal(t, "http://localhost:3000", w.Header().Get("Access-Control-Allow-Origin"))

	for _, origin := range []string{"https://evil.example", "https://app.example.com.evil.example", "http://app.example.com", "null"} {
		w = corsResponse(t, allowed, http.MethodGet, origin)
		assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"), "origin %s must not be allowed", origin)
		assert.Empty(t, w.Header().Get("Access-Control-Allow-Credentials"), "origin %s must not get credentials", origin)
	}
}

func TestCORS_MatchesOriginsCaseInsensitively(t *testing.T) {
	w := corsResponse(t, []string{"https://App.Example.com"}, http.MethodGet, "https://app.example.com")
	assert.Equal(t, "https://app.example.com", w.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORS_Preflight(t *testing.T) {
	allowed := []string{"https://app.example.com"}

	w := corsResponse(t, allowed, http.MethodOptions, "https://app.example.com")
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, "https://app.example.com", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "GET, POST, PUT, PATCH, DELETE, OPTIONS", w.Header().Get("Access-Control-Allow-Methods"))
	assert.Contains(t, w.Header().Get("Access-Control-Allow-Headers"), "Authorization")
	assert.Equal(t, "X-Request-ID, Retry-After, X-RateLimit-Limit, X-RateLimit-Remaining",
		w.Header().Get("Access-Control-Expose-Headers"), "browsers may read only the exposed headers")

	w = corsResponse(t, allowed, http.MethodOptions, "https://evil.example")
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"), "a preflight from another origin must not be granted")
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Methods"))
}

func TestCORS_SameOriginRequestsAreUnaffected(t *testing.T) {
	w := corsResponse(t, []string{"https://app.example.com"}, http.MethodGet, "")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
}

func TestNewCORSMiddleware_RejectsUnsafeOrInvalidOrigins(t *testing.T) {
	for _, origin := range []string{
		"*",
		"app.example.com",
		"ftp://app.example.com",
		"https://app.example.com/path",
		"https://app.example.com?query=1",
		"https://user@app.example.com",
		"https://",
	} {
		_, err := NewCORSMiddleware([]string{origin})
		assert.Error(t, err, "origin %q must be rejected", origin)
	}
}
