package middleware

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/db"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type take struct {
	at      time.Duration // after the first request
	allowed bool
	wait    time.Duration // until the next token, when not allowed
}

// takeTokens runs the token bucket script at the given times, as if the
// requests arrived then.
func takeTokens(t *testing.T, rate, burst int, takes []take) {
	t.Helper()
	_, rl, _ := newAuthLimitEngine(t)
	key := "ratelimit:test:" + uuid.NewString()
	start := time.UnixMilli(time.Now().UnixMilli())
	for i, want := range takes {
		allowed, _, wait, err := rl.consumeToken(context.Background(), key, rate, burst, start.Add(want.at))
		require.NoError(t, err)
		assert.Equal(t, want.allowed, allowed, "request %d at %v", i+1, want.at)
		if !want.allowed {
			assert.Equal(t, want.wait, wait, "request %d at %v", i+1, want.at)
		}
	}
}

func TestTokenBucket_RefillsFractionalTokens(t *testing.T) {
	// Three tokens per second: a token every 333.3 ms.
	takeTokens(t, 3, 1, []take{
		{0, true, 0},
		{100 * time.Millisecond, false, 234 * time.Millisecond},
		{300 * time.Millisecond, false, 34 * time.Millisecond},
		{334 * time.Millisecond, true, 0},
	})
}

func TestTokenBucket_RatesAboveOneThousandPerSecond(t *testing.T) {
	// The bucket used to refill in whole milliseconds per token, which is
	// zero above 1000 per second.
	takeTokens(t, 2000, 2, []take{
		{0, true, 0},
		{0, true, 0},
		{0, false, time.Millisecond},
		{time.Millisecond, true, 0},
		{time.Millisecond, true, 0},
		{time.Millisecond, false, time.Millisecond},
	})
	takeTokens(t, 10000, 1, []take{
		{0, true, 0},
		{0, false, time.Millisecond},
		{time.Millisecond, true, 0},
	})
}

func TestTokenBucket_Headers(t *testing.T) {
	engine, rl, _ := newAuthLimitEngine(t)
	// Four tokens per second: the next token comes after 250 ms.
	engine.POST("/limited", rl.tokenBucketMiddleware("headers", 4, 2, false, rl.getClientIP), ok)

	w := post(engine, "/limited", "203.0.113.7")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "2", w.Header().Get("X-RateLimit-Limit"))
	assert.Equal(t, "1", w.Header().Get("X-RateLimit-Remaining"))
	w = post(engine, "/limited", "203.0.113.7")
	assert.Equal(t, "0", w.Header().Get("X-RateLimit-Remaining"))

	w = post(engine, "/limited", "203.0.113.7")
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "1", w.Header().Get("Retry-After"), "a wait under a second rounds up, never down to 0")
}

// unreachableLimiter returns a limiter whose Redis cannot be reached.
func unreachableLimiter(t *testing.T) *RateLimiter {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { client.Close() })
	return NewRateLimiter(&config.Config, &db.RedisService{Client: client})
}

func TestRateLimiters_FollowTheirFailurePolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rl := unreachableLimiter(t)
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
		engine.POST("/", tc.limiter, ok)
		w := post(engine, "/", "203.0.113.7")
		assert.Equal(t, tc.want, w.Code, name)
		if tc.want == http.StatusServiceUnavailable {
			assert.Equal(t, "1", w.Header().Get("Retry-After"), name)
		}
	}
}

func TestAuthRateLimit_FailsClosedAsConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	saved := config.Config.RateLimits.Auth
	t.Cleanup(func() { config.Config.RateLimits.Auth = saved })
	config.Config.RateLimits.Auth.LoginAttemptsPerMinute = 5
	rl := unreachableLimiter(t)

	for onError, want := range map[string]int{config.RateLimitDeny: http.StatusServiceUnavailable, config.RateLimitAllow: http.StatusOK} {
		config.Config.RateLimits.Auth.OnError = onError
		engine := gin.New()
		engine.POST("/public/auth/:uuid", rl.AuthRateLimit(AuthLogin), ok)
		assert.Equal(t, want, post(engine, "/public/auth/x", "203.0.113.7").Code, onError)
	}
}

func testKeyPair(t *testing.T) auth.KeyPair {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return auth.KeyPair{
		PrivateKey: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
		PublicKey:  pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&key.PublicKey)}),
	}
}

// TestTradingRateLimit_CountsPerSubject signs requests with real access
// tokens: two users behind one address each have a budget of their own.
func TestTradingRateLimit_CountsPerSubject(t *testing.T) {
	engine, rl, client := newAuthLimitEngine(t)
	savedJWT, savedTrading := config.Config.JWT, config.Config.RateLimits.Trading
	t.Cleanup(func() { config.Config.JWT, config.Config.RateLimits.Trading = savedJWT, savedTrading })
	config.Config.JWT.Issuer = "ratelimit-test"
	config.Config.JWT.AccessToken.Expiration = 15
	config.Config.RateLimits.Trading.OrdersPerSecond, config.Config.RateLimits.Trading.Burst = 1, 1
	require.NoError(t, auth.SetKeys(map[string]auth.KeyPair{auth.RoleUser: testKeyPair(t)}))

	engine.POST("/user/orders", auth.JWTUserVerification(auth.NewTokenBlacklist(client)), rl.TradingRateLimit(), ok)
	send := func(userID string) int {
		token, err := auth.GenerateAccessToken(userID, auth.RoleUser, uuid.NewString())
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/user/orders", nil)
		req.RemoteAddr = "203.0.113.7:40000"
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w.Code
	}

	alice, bob := uuid.NewString(), uuid.NewString()
	assert.Equal(t, http.StatusOK, send(alice))
	assert.Equal(t, http.StatusTooManyRequests, send(alice), "the second request of a user in the same second is limited")
	assert.Equal(t, http.StatusOK, send(bob), "another user behind the same address has a budget of their own")
}

func TestLimiterErrors_AreLoggedOncePerInterval(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	prefix := "logged-" + uuid.NewString()
	for i := 0; i < 50; i++ {
		logLimiterError(prefix, errors.New("connection refused"))
	}
	assert.Equal(t, 1, strings.Count(logs.String(), prefix), "an outage writes one line per interval")

	logLimiterError("other-"+prefix, errors.New("connection refused"))
	assert.Equal(t, 2, strings.Count(logs.String(), prefix), "each limiter logs its own errors")
}
