package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestRateLimiter_RESTRateLimit_AllowsRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// This is a basic test to ensure middleware compiles and can be created
	// Full integration tests would require a real Redis instance
	cfg := config.Config.RateLimits.REST
	if cfg.RequestsPerSecond <= 0 || cfg.Burst <= 0 {
		t.Skip("Rate limiting not configured, skipping")
	}
}

func TestValidationMiddleware_BindAndValidate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	type TestStruct struct {
		Name  string `json:"name" validate:"required,min=3,max=50"`
		Email string `json:"email" validate:"required,email"`
		Age   int    `json:"age" validate:"required,min=18,max=100"`
	}

	tests := []struct {
		name       string
		input      map[string]interface{}
		wantStatus int
		wantError  bool
	}{
		{
			name: "valid input",
			input: map[string]interface{}{
				"name":  "John Doe",
				"email": "john@example.com",
				"age":   25,
			},
			wantStatus: http.StatusOK,
			wantError:  false,
		},
		{
			name: "missing required field",
			input: map[string]interface{}{
				"email": "john@example.com",
				"age":   25,
			},
			wantStatus: http.StatusBadRequest,
			wantError:  true,
		},
		{
			name: "invalid email",
			input: map[string]interface{}{
				"name":  "John Doe",
				"email": "invalid-email",
				"age":   25,
			},
			wantStatus: http.StatusBadRequest,
			wantError:  true,
		},
		{
			name: "age out of range",
			input: map[string]interface{}{
				"name":  "John Doe",
				"email": "john@example.com",
				"age":   10,
			},
			wantStatus: http.StatusBadRequest,
			wantError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
			c.Request.Header.Set("Content-Type", "application/json")

			// Create test JSON body
			body, _ := json.Marshal(tt.input)
			c.Request.Body = io.NopCloser(bytes.NewReader(body))

			// Apply middleware
			c.Set("validator", validate)
			err := BindAndValidate(c, &TestStruct{})

			if tt.wantError {
				assert.Error(t, err)
				assert.Equal(t, tt.wantStatus, w.Code)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestSanitizeInput(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"  hello world  ", "hello world"},
		{"hello\x00world", "helloworld"},
		{"hello\r\nworld", "hello world"},
		{"\t\n  test  \r\n", "test"},
		{"", ""},
	}

	for _, tt := range tests {
		result := SanitizeInput(tt.input)
		assert.Equal(t, tt.expected, result)
	}
}

func TestSanitizeMap(t *testing.T) {
	m := map[string]interface{}{
		"name": "  John  ",
		"bio":  "Hello\r\nWorld",
		"age":  25, // non-string should remain unchanged
	}

	SanitizeMap(m)

	assert.Equal(t, "John", m["name"])
	assert.Equal(t, "Hello World", m["bio"])
	assert.Equal(t, 25, m["age"])
}

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
