package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/testutil/redistest"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clientIPSeenBy returns the rate-limit identity of a request that reaches an
// engine trusting the given proxies.
func clientIPSeenBy(t *testing.T, trustedProxies []string, remoteAddr string, headers map[string]string) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	require.NoError(t, engine.SetTrustedProxies(trustedProxies))

	rl := NewRateLimiter(&config.Config, nil)
	var seen string
	engine.GET("/", func(c *gin.Context) { seen = rl.getClientIP(c) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = remoteAddr
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	engine.ServeHTTP(httptest.NewRecorder(), req)
	return seen
}

func TestGetClientIP_IgnoresForwardingHeadersWithoutTrustedProxies(t *testing.T) {
	seen := clientIPSeenBy(t, nil, "203.0.113.7:40000", map[string]string{
		"X-Forwarded-For": "10.0.0.1",
		"X-Real-IP":       "10.0.0.2",
	})
	assert.Equal(t, "203.0.113.7", seen, "a client must not choose its own rate-limit identity")
}

func TestGetClientIP_UsesForwardingHeaderFromTrustedProxy(t *testing.T) {
	trusted := []string{"10.1.0.0/16"}

	seen := clientIPSeenBy(t, trusted, "10.1.2.3:50000", map[string]string{"X-Forwarded-For": "198.51.100.9"})
	assert.Equal(t, "198.51.100.9", seen, "the address reported by a trusted proxy is the client")

	// A client can prepend anything to the header; only the entry added by the
	// trusted proxy counts.
	seen = clientIPSeenBy(t, trusted, "10.1.2.3:50000", map[string]string{"X-Forwarded-For": "1.2.3.4, 198.51.100.9"})
	assert.Equal(t, "198.51.100.9", seen)

	seen = clientIPSeenBy(t, trusted, "203.0.113.7:40000", map[string]string{"X-Forwarded-For": "198.51.100.9"})
	assert.Equal(t, "203.0.113.7", seen, "the header is ignored when the sender is not a trusted proxy")
}

// TestAuthRateLimit_SpoofedForwardedForDoesNotBypassTheLimit needs Redis on
// localhost:6379 and uses the database of this package.
func TestAuthRateLimit_SpoofedForwardedForDoesNotBypassTheLimit(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: redistest.Middleware})
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("Redis is not reachable at localhost:6379: %v", err)
	}
	t.Cleanup(func() {
		client.FlushDB(context.Background())
		client.Close()
	})

	savedLimits := config.Config.RateLimits
	t.Cleanup(func() { config.Config.RateLimits = savedLimits })
	config.Config.RateLimits.Auth.LoginAttemptsPerMinute = 5

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	require.NoError(t, engine.SetTrustedProxies(nil))
	rl := NewRateLimiter(&config.Config, &db.RedisService{Client: client})
	engine.POST("/public/auth", rl.AuthRateLimit(AuthLogin), func(c *gin.Context) { c.Status(http.StatusOK) })

	codes := make([]int, 0, 6)
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest(http.MethodPost, "/public/auth", nil)
		req.RemoteAddr = "203.0.113.7:40000"
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.0.0.%d", i+1))
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		codes = append(codes, w.Code)
	}

	assert.Equal(t, []int{200, 200, 200, 200, 200, http.StatusTooManyRequests}, codes,
		"changing X-Forwarded-For on every request must not reset the limit")
}

// TestGetClientIP_GroupsIPv6ByPrefix checks the identity of IPv6 clients
// (#188): the addresses of one /64 are one client.
func TestGetClientIP_GroupsIPv6ByPrefix(t *testing.T) {
	saved := config.Config.RateLimits
	t.Cleanup(func() { config.Config.RateLimits = saved })

	config.Config.RateLimits.IPv6PrefixLength = 64
	assert.Equal(t, "2001:db8:1:2::/64", clientIPSeenBy(t, nil, "[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:40000", nil))
	assert.Equal(t, "203.0.113.7", clientIPSeenBy(t, nil, "[::ffff:203.0.113.7]:40000", nil), "IPv4-mapped addresses count as IPv4")
	assert.Equal(t, "2001:db8:5::/64", clientIPSeenBy(t, []string{"10.1.0.0/16"}, "10.1.2.3:50000",
		map[string]string{"X-Forwarded-For": "2001:db8:5::77"}), "an address from a trusted proxy is grouped too")

	config.Config.RateLimits.IPv6PrefixLength = 56
	assert.Equal(t, "2001:db8:1::/56", clientIPSeenBy(t, nil, "[2001:db8:1:2::1]:40000", nil))
}

// TestAuthRateLimit_RotatingIPv6AddressesDoNotBypassTheLimit needs Redis on
// localhost:6379 and uses the database of this package.
func TestAuthRateLimit_RotatingIPv6AddressesDoNotBypassTheLimit(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: redistest.Middleware})
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("Redis is not reachable at localhost:6379: %v", err)
	}
	t.Cleanup(func() {
		client.FlushDB(context.Background())
		client.Close()
	})

	savedLimits := config.Config.RateLimits
	t.Cleanup(func() { config.Config.RateLimits = savedLimits })
	config.Config.RateLimits.Auth.LoginAttemptsPerMinute = 5
	config.Config.RateLimits.IPv6PrefixLength = 64

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	require.NoError(t, engine.SetTrustedProxies(nil))
	rl := NewRateLimiter(&config.Config, &db.RedisService{Client: client})
	engine.POST("/public/auth/:uuid", rl.AuthRateLimit(AuthLogin), func(c *gin.Context) { c.Status(http.StatusOK) })

	send := func(addr string) int {
		req := httptest.NewRequest(http.MethodPost, "/public/auth/x", nil)
		req.RemoteAddr = "[" + addr + "]:40000"
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w.Code
	}
	codes := make([]int, 0, 6)
	for i := 0; i < 6; i++ {
		codes = append(codes, send(fmt.Sprintf("2001:db8:aa:1::%x", i+1)))
	}
	assert.Equal(t, []int{200, 200, 200, 200, 200, http.StatusTooManyRequests}, codes,
		"a fresh address of the same /64 on every request must not reset the limit")
	assert.Equal(t, http.StatusOK, send("2001:db8:aa:2::1"), "another /64 is another client")
}
