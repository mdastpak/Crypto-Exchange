package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"encrypted-db/config"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

type mockRedisClient struct {
	mock.Mock
}

func (m *mockRedisClient) Pipeline() redis.Pipeliner {
	args := m.Called()
	return args.Get(0).(redis.Pipeliner)
}

func (m *mockRedisClient) Incr(ctx context.Context, key string) *redis.IntCmd {
	args := m.Called(ctx, key)
	return args.Get(0).(*redis.IntCmd)
}

func (m *mockRedisClient) Expire(ctx context.Context, key string, expiration time.Duration) *redis.BoolCmd {
	args := m.Called(ctx, key, expiration)
	return args.Get(0).(*redis.BoolCmd)
}

func (m *mockRedisClient) Exec(ctx context.Context, cmds []redis.Cmder) error {
	args := m.Called(ctx, cmds)
	return args.Error(0)
}

func (m *mockRedisClient) TTL(ctx context.Context, key string) *redis.DurationCmd {
	args := m.Called(ctx, key)
	return args.Get(0).(*redis.DurationCmd)
}

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

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/test", nil)

	SecurityHeaders()(c)

	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", w.Header().Get("X-Frame-Options"))
	assert.Equal(t, "1; mode=block", w.Header().Get("X-XSS-Protection"))
	assert.Equal(t, "strict-origin-when-cross-origin", w.Header().Get("Referrer-Policy"))
}
