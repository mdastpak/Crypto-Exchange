package middleware

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/db"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryBuckets_RefillLikeTheRedisBuckets(t *testing.T) {
	buckets := newMemoryBuckets(1, 2)
	start := time.Now()
	at := func(d time.Duration) time.Time { return start.Add(d) }

	allowed, remaining, _ := buckets.take("a", at(0))
	assert.True(t, allowed)
	assert.Equal(t, 1, remaining)
	allowed, remaining, _ = buckets.take("a", at(0))
	assert.True(t, allowed)
	assert.Equal(t, 0, remaining)

	allowed, _, wait := buckets.take("a", at(0))
	assert.False(t, allowed, "the burst is used up")
	assert.Equal(t, time.Second, wait)
	allowed, _, wait = buckets.take("a", at(500*time.Millisecond))
	assert.False(t, allowed)
	assert.Equal(t, 500*time.Millisecond, wait, "half a token has refilled")

	allowed, _, _ = buckets.take("b", at(500*time.Millisecond))
	assert.True(t, allowed, "each key has a bucket of its own")
	allowed, _, _ = buckets.take("a", at(time.Second))
	assert.True(t, allowed, "a whole token has refilled")
}

func TestMemoryBuckets_DropBucketsThatHaveRefilled(t *testing.T) {
	// A bucket refills completely in a second.
	buckets := newMemoryBuckets(10, 10)
	start := time.Now()

	buckets.take("idle", start)
	buckets.take("recent", start.Add(600*time.Millisecond))
	require.Equal(t, 2, buckets.size())

	buckets.take("new", start.Add(time.Second))
	assert.Equal(t, 2, buckets.size(), "the idle bucket is dropped, the recent one kept")

	allowed, remaining, _ := buckets.take("idle", start.Add(time.Second))
	assert.True(t, allowed)
	assert.Equal(t, 9, remaining, "a dropped bucket starts full, as it would have refilled")
}

// stalledRedis returns a Redis client whose server accepts connections and
// never answers, as a hung Redis or a network that drops packets would. The
// client keeps the library's default timeouts and retries, which are much
// longer than the limiter's deadline. Like the server's client
// (db.NewRedisService), it respects context deadlines.
func stalledRedis(t *testing.T) *db.RedisService {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	client := redis.NewClient(&redis.Options{Addr: listener.Addr().String(), ContextTimeoutEnabled: true})
	t.Cleanup(func() {
		client.Close()
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			conn.Close()
		}
	})
	return &db.RedisService{Client: client}
}

// timedGet serves a GET of path and returns the response and how long it
// took.
func timedGet(engine *gin.Engine, path string) (*httptest.ResponseRecorder, time.Duration) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "203.0.113.9:40000"
	w := httptest.NewRecorder()
	start := time.Now()
	engine.ServeHTTP(w, req)
	return w, time.Since(start)
}

func TestRateLimiters_GiveUpOnRedisAtTheirDeadline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	saved := config.Config.RateLimits
	t.Cleanup(func() { config.Config.RateLimits = saved })
	const deadline = 100 * time.Millisecond
	config.Config.RateLimits.TimeoutMs = int(deadline / time.Millisecond)
	config.Config.RateLimits.Auth.LoginAttemptsPerMinute = 5
	rl := NewRateLimiter(stalledRedis(t))
	require.Equal(t, deadline, rl.timeout, "rate_limits.timeout_ms")

	cases := map[string]struct {
		limiter gin.HandlerFunc
		want    int
	}{
		"token bucket that fails open":   {rl.tokenBucketMiddleware("open", 10, 10, false, rl.getClientIP), http.StatusOK},
		"token bucket that fails closed": {rl.tokenBucketMiddleware("closed", 10, 10, true, rl.getClientIP), http.StatusServiceUnavailable},
		"fixed window that fails open":   {rl.fixedWindowMiddleware("open", 10, time.Minute, false, rl.getClientIP), http.StatusOK},
		"fixed window that fails closed": {rl.fixedWindowMiddleware("closed", 10, time.Minute, true, rl.getClientIP), http.StatusServiceUnavailable},
	}
	for name, tc := range cases {
		engine := gin.New()
		engine.GET("/", tc.limiter, ok)
		w, took := timedGet(engine, "/")
		assert.Equal(t, tc.want, w.Code, name)
		assert.GreaterOrEqual(t, took, deadline, "%s: Redis was waited for", name)
		assert.Less(t, took, deadline+400*time.Millisecond, "%s: but not past the deadline", name)
	}
}

func TestRateLimiters_OfOneRouteWaitForRedisOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	saved := config.Config.RateLimits
	t.Cleanup(func() { config.Config.RateLimits = saved })
	const deadline = 200 * time.Millisecond
	config.Config.RateLimits.TimeoutMs = int(deadline / time.Millisecond)
	rl := NewRateLimiter(stalledRedis(t))

	engine := gin.New()
	engine.GET("/",
		rl.tokenBucketMiddleware("first", 10, 10, false, rl.getClientIP),
		rl.fixedWindowMiddleware("second", 10, time.Minute, false, rl.getClientIP),
		rl.tokenBucketMiddleware("last", 10, 10, true, rl.getClientIP),
		ok)
	w, took := timedGet(engine, "/")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "the last limiter still follows its own on_error")
	assert.GreaterOrEqual(t, took, deadline)
	assert.Less(t, took, 2*deadline, "the later limiters do not wait for Redis again")
}

func TestNewRateLimiter_DefaultDeadline(t *testing.T) {
	saved := config.Config.RateLimits
	t.Cleanup(func() { config.Config.RateLimits = saved })
	config.Config.RateLimits.TimeoutMs = 0
	assert.Equal(t, config.DefaultRateLimitTimeoutMs*time.Millisecond, NewRateLimiter(nil).timeout,
		"a configuration that Load did not fill in gets the default, not an unbounded wait")
}

func TestRESTRateLimit_CountsProbesInMemory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	saved := config.Config.RateLimits
	t.Cleanup(func() { config.Config.RateLimits = saved })
	limits := &config.Config.RateLimits
	limits.TimeoutMs = 100
	limits.REST.RequestsPerSecond, limits.REST.Burst = 1, 3

	for onError, want := range map[string]int{config.RateLimitAllow: http.StatusOK, config.RateLimitDeny: http.StatusServiceUnavailable} {
		limits.REST.OnError = onError
		engine := gin.New()
		require.NoError(t, engine.SetTrustedProxies(nil))
		engine.Use(NewRateLimiter(stalledRedis(t)).RESTRateLimit("/system/live"))
		engine.GET("/system/live", ok)
		engine.GET("/system/other", ok)

		for i := 0; i < 3; i++ {
			w, took := timedGet(engine, "/system/live")
			require.Equal(t, http.StatusOK, w.Code, "%s: probe %d", onError, i+1)
			assert.Less(t, took, 50*time.Millisecond, "%s: a probe does not wait for Redis", onError)
			assert.Equal(t, "3", w.Header().Get("X-RateLimit-Limit"), "%s: a probe is still limited", onError)
		}
		w, _ := timedGet(engine, "/system/live")
		assert.Equal(t, http.StatusTooManyRequests, w.Code, "%s: the burst applies to probes too", onError)

		w, took := timedGet(engine, "/system/other")
		assert.Equal(t, want, w.Code, "%s: other routes count in Redis and follow on_error", onError)
		assert.GreaterOrEqual(t, took, 100*time.Millisecond, onError)
	}
}
