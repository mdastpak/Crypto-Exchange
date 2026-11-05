package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/config"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/testutil/redistest"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAuthLimitEngine returns an engine whose routes answer 200, and a Redis
// client on the database of this package. The authentication limits are restored afterwards.
func newAuthLimitEngine(t *testing.T) (*gin.Engine, *RateLimiter, *redis.Client) {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: redistest.Middleware})
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

// TestFixedWindow_EndsOnTimeWhileTheClientRetries keeps a blocked client
// retrying until shortly before the window ends. Retrying must not extend
// the window.
func TestFixedWindow_EndsOnTimeWhileTheClientRetries(t *testing.T) {
	engine, rl, client := newAuthLimitEngine(t)
	const window = 400 * time.Millisecond
	engine.POST("/limited", rl.fixedWindowMiddleware("test", 2, window, false, rl.getClientIP), ok)

	start := time.Now()
	assert.Equal(t, http.StatusOK, post(engine, "/limited", "203.0.113.7").Code)
	assert.Equal(t, http.StatusOK, post(engine, "/limited", "203.0.113.7").Code)
	for time.Since(start) < window-100*time.Millisecond {
		require.Equal(t, http.StatusTooManyRequests, post(engine, "/limited", "203.0.113.7").Code)
		time.Sleep(20 * time.Millisecond)
	}

	time.Sleep(time.Until(start.Add(window + 50*time.Millisecond)))
	assert.Equal(t, http.StatusOK, post(engine, "/limited", "203.0.113.7").Code,
		"the window ends a window after the first request")

	// A counter left without an expiry gets one on the next request.
	require.NoError(t, client.Set(context.Background(), "ratelimit:test:198.51.100.9", 5, 0).Err())
	assert.Equal(t, http.StatusTooManyRequests, post(engine, "/limited", "198.51.100.9").Code)
	ttl, err := client.PTTL(context.Background(), "ratelimit:test:198.51.100.9").Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0))
	assert.LessOrEqual(t, ttl, window)
}
