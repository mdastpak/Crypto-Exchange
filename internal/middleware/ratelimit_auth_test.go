package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"encrypted-db/config"
	"encrypted-db/internal/db"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAuthLimitEngine returns an engine whose routes answer 200, and a Redis
// client on database 3. The authentication limits are restored afterwards.
func newAuthLimitEngine(t *testing.T) (*gin.Engine, *RateLimiter, *redis.Client) {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: 3})
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("Redis is not reachable at localhost:6379: %v", err)
	}
	t.Cleanup(func() {
		client.FlushDB(context.Background())
		client.Close()
	})
	saved := config.Config.RateLimits.Auth
	t.Cleanup(func() { config.Config.RateLimits.Auth = saved })

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	require.NoError(t, engine.SetTrustedProxies(nil))
	return engine, NewRateLimiter(&db.RedisService{Client: client}), client
}

func ok(c *gin.Context) { c.Status(http.StatusOK) }

func post(engine *gin.Engine, path, ip string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.RemoteAddr = ip + ":40000"
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func retryAfter(t *testing.T, w *httptest.ResponseRecorder) time.Duration {
	t.Helper()
	seconds, err := strconv.Atoi(w.Header().Get("Retry-After"))
	require.NoError(t, err)
	return time.Duration(seconds) * time.Second
}

func TestAuthRateLimit_OTPRequestsAreCountedPerHour(t *testing.T) {
	engine, rl, client := newAuthLimitEngine(t)
	config.Config.RateLimits.Auth.OTPRequestsPerHour = 2
	engine.POST("/public/auth", rl.AuthRateLimit(AuthOTP), ok)

	assert.Equal(t, http.StatusOK, post(engine, "/public/auth", "203.0.113.7").Code)
	assert.Equal(t, http.StatusOK, post(engine, "/public/auth", "203.0.113.7").Code)
	w := post(engine, "/public/auth", "203.0.113.7")
	require.Equal(t, http.StatusTooManyRequests, w.Code)

	assert.Greater(t, retryAfter(t, w), 59*time.Minute, "the window of code requests is an hour")
	ttl, err := client.TTL(context.Background(), "ratelimit:auth:otp:/public/auth:203.0.113.7").Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, 59*time.Minute)
}

func TestAuthRateLimit_LoginAttemptsAreCountedPerMinute(t *testing.T) {
	engine, rl, _ := newAuthLimitEngine(t)
	config.Config.RateLimits.Auth.LoginAttemptsPerMinute = 2
	engine.POST("/public/auth/:uuid", rl.AuthRateLimit(AuthLogin), ok)

	assert.Equal(t, http.StatusOK, post(engine, "/public/auth/a", "203.0.113.7").Code)
	assert.Equal(t, http.StatusOK, post(engine, "/public/auth/b", "203.0.113.7").Code)
	w := post(engine, "/public/auth/c", "203.0.113.7")
	require.Equal(t, http.StatusTooManyRequests, w.Code, "every session of a route shares the budget")

	assert.Greater(t, retryAfter(t, w), time.Duration(0))
	assert.LessOrEqual(t, retryAfter(t, w), time.Minute)
}

func TestAuthRateLimit_EachRouteAndClientCountsSeparately(t *testing.T) {
	engine, rl, _ := newAuthLimitEngine(t)
	config.Config.RateLimits.Auth.OTPRequestsPerHour = 1
	engine.POST("/public/auth", rl.AuthRateLimit(AuthOTP), ok)
	engine.POST("/admin/auth", rl.AuthRateLimit(AuthOTP), ok)

	assert.Equal(t, http.StatusOK, post(engine, "/public/auth", "203.0.113.7").Code)
	assert.Equal(t, http.StatusTooManyRequests, post(engine, "/public/auth", "203.0.113.7").Code)

	assert.Equal(t, http.StatusOK, post(engine, "/admin/auth", "203.0.113.7").Code,
		"user sign-in must not use up the budget of admin sign-in")
	assert.Equal(t, http.StatusOK, post(engine, "/public/auth", "198.51.100.9").Code,
		"another client has a budget of its own")
}

func TestAuthRateLimit_UnknownActionPanics(t *testing.T) {
	assert.Panics(t, func() { NewRateLimiter(nil).AuthRateLimit("signup") })
}
