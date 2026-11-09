package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestSecurityHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	headers := func(opts SecurityOptions, path string) http.Header {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, path, nil)
		SecurityHeaders(opts)(c)
		return w.Header()
	}

	h := headers(SecurityOptions{DocsPrefix: "/swagger/"}, "/api/v1/user/profile")
	assert.Equal(t, "nosniff", h.Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", h.Get("X-Frame-Options"))
	assert.Equal(t, "0", h.Get("X-XSS-Protection"), "the XSS auditor is turned off, as recommended")
	assert.Equal(t, "strict-origin-when-cross-origin", h.Get("Referrer-Policy"))
	assert.Equal(t, "no-store", h.Get("Cache-Control"), "tokens and personal data must not be stored")
	assert.Equal(t, "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'", h.Get("Content-Security-Policy"))
	assert.Empty(t, h.Get("Strict-Transport-Security"), "no HSTS unless TLS is terminated")

	h = headers(SecurityOptions{DocsPrefix: "/swagger/"}, "/swagger/index.html")
	csp := h.Get("Content-Security-Policy")
	assert.Contains(t, csp, "script-src 'self' 'unsafe-inline'", "the Swagger UI runs its own scripts")
	assert.Contains(t, csp, "frame-ancestors 'none'")
	assert.NotContains(t, csp, "*")

	h = headers(SecurityOptions{HSTS: true, HSTSMaxAge: 31536000}, "/")
	assert.Equal(t, "max-age=31536000", h.Get("Strict-Transport-Security"))
	h = headers(SecurityOptions{HSTS: true, HSTSMaxAge: 600, HSTSIncludeSubdomains: true}, "/")
	assert.Equal(t, "max-age=600; includeSubDomains", h.Get("Strict-Transport-Security"))
}

func TestSecurityHeaders_HandlersMayCacheTheirAnswers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(SecurityHeaders(SecurityOptions{}))
	engine.GET("/public", func(c *gin.Context) {
		c.Header("Cache-Control", "public, max-age=30")
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/public", nil))
	assert.Equal(t, "public, max-age=30", w.Header().Get("Cache-Control"))
}
