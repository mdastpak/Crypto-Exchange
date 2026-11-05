package middleware

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mdastpak/crypto-exchange/config"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/helpers"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

type RateLimiter struct {
	redis *db.RedisService
	// timeout bounds each call to Redis, so that a Redis outage delays a
	// request by at most this long before on_error decides.
	timeout time.Duration
}

func NewRateLimiter(redis *db.RedisService) *RateLimiter {
	return &RateLimiter{
		redis:   redis,
		timeout: config.Milliseconds(config.Config.RateLimits.TimeoutMs, config.DefaultRateLimitTimeoutMs),
	}
}

// denies reports whether a limiter rejects requests while Redis cannot be
// reached, as its on_error setting says. An unset setting allows them.
func denies(onError string) bool {
	return onError == config.RateLimitDeny
}

// RESTRateLimit limits the requests of each client IP. The routes in probes
// are counted in the memory of this instance instead of in Redis, so that the
// health probes answer at once while Redis is down.
func (rl *RateLimiter) RESTRateLimit(probes ...string) gin.HandlerFunc {
	cfg := config.Config.RateLimits.REST
	shared := rl.tokenBucketMiddleware("rest", cfg.RequestsPerSecond, cfg.Burst, denies(cfg.OnError), rl.getClientIP)
	if len(probes) == 0 {
		return shared
	}
	local := memoryTokenBucketMiddleware(cfg.RequestsPerSecond, cfg.Burst, rl.getClientIP)
	isProbe := make(map[string]bool, len(probes))
	for _, route := range probes {
		isProbe[route] = true
	}
	return func(c *gin.Context) {
		if isProbe[c.FullPath()] {
			local(c)
			return
		}
		shared(c)
	}
}

func (rl *RateLimiter) WSRateLimit() gin.HandlerFunc {
	cfg := config.Config.RateLimits.WS
	return rl.tokenBucketMiddleware("ws", cfg.MessagesPerSecond, cfg.Burst, denies(cfg.OnError), rl.getClientIP)
}

// TradingRateLimit limits the requests of each signed-in user or
// administrator. It must run after the authentication middleware.
func (rl *RateLimiter) TradingRateLimit() gin.HandlerFunc {
	cfg := config.Config.RateLimits.Trading
	return rl.tokenBucketMiddleware("trading", cfg.OrdersPerSecond, cfg.Burst, denies(cfg.OnError), rl.getUserID)
}

// Authentication limits, for AuthRateLimit.
const (
	// AuthLogin limits sign-in code verifications, per minute.
	AuthLogin = "login"
	// AuthOTP limits sign-in code requests, per hour.
	AuthOTP = "otp"
)

// authLimit returns the configured limit of an authentication action and the
// window it is counted in.
func authLimit(action string) (int, time.Duration) {
	cfg := config.Config.RateLimits.Auth
	switch action {
	case AuthLogin:
		return cfg.LoginAttemptsPerMinute, time.Minute
	case AuthOTP:
		return cfg.OTPRequestsPerHour, time.Hour
	}
	panic(fmt.Sprintf("unknown authentication rate limit %q", action))
}

// AuthRateLimit limits an authentication route per client IP, in the window
// of the action. Each route counts separately, so that, for example, user
// sign-in traffic cannot use up the budget of admin sign-in.
func (rl *RateLimiter) AuthRateLimit(action string) gin.HandlerFunc {
	limit, window := authLimit(action)
	return rl.fixedWindowMiddleware("auth:"+action, limit, window, denies(config.Config.RateLimits.Auth.OnError), func(c *gin.Context) string {
		return c.FullPath() + ":" + rl.getClientIP(c)
	})
}

// limiterErrorLogInterval is how often each limiter logs that Redis cannot
// be reached, so that an outage writes one line per limiter every few
// seconds instead of one per request.
const limiterErrorLogInterval = 10 * time.Second

// limiterErrorLogged maps a limiter to when it last logged an error, in
// Unix nanoseconds.
var limiterErrorLogged sync.Map

func logLimiterError(prefix string, err error) {
	v, _ := limiterErrorLogged.LoadOrStore(prefix, new(atomic.Int64))
	last := v.(*atomic.Int64)
	now := time.Now().UnixNano()
	previous := last.Load()
	if previous != 0 && now-previous < int64(limiterErrorLogInterval) {
		return
	}
	if last.CompareAndSwap(previous, now) {
		log.Printf("Rate limiter %s cannot reach Redis: %v", prefix, err)
	}
}

// redisUnreachableKey marks a request for which a limiter could not reach
// Redis, so that the next limiters of its route do not wait for Redis again:
// together, they wait at most once.
const redisUnreachableKey = "ratelimit.redis_unreachable"

var errRedisUnreachable = errors.New("an earlier limiter of this request could not reach it")

// redisUnreachable reports whether an earlier limiter of the request could
// not reach Redis.
func redisUnreachable(c *gin.Context) bool {
	return c.GetBool(redisUnreachableKey)
}

// limiterUnavailable handles a request while Redis cannot be reached: it is
// rejected with 503 when the limiter fails closed, and let through otherwise.
func limiterUnavailable(c *gin.Context, prefix string, failClosed bool, err error) {
	c.Set(redisUnreachableKey, true)
	logLimiterError(prefix, err)
	if failClosed {
		c.Header("Retry-After", "1")
		helpers.SendResponse(c, http.StatusServiceUnavailable, "Service temporarily unavailable", nil)
		c.Abort()
		return
	}
	c.Next()
}

// retryAfterSeconds rounds a wait up to whole seconds, as Retry-After needs,
// so that a client never retries before the limiter would let it through.
func retryAfterSeconds(wait time.Duration) string {
	return strconv.FormatInt(int64(math.Max(1, math.Ceil(wait.Seconds()))), 10)
}

func rejectRateLimited(c *gin.Context, limit int, wait time.Duration) {
	c.Header("X-RateLimit-Limit", strconv.Itoa(limit))
	c.Header("X-RateLimit-Remaining", "0")
	c.Header("Retry-After", retryAfterSeconds(wait))
	helpers.SendResponse(c, http.StatusTooManyRequests, "Rate limit exceeded", nil)
	c.Abort()
}

func (rl *RateLimiter) tokenBucketMiddleware(prefix string, rate, burst int, failClosed bool, keyFunc func(*gin.Context) string) gin.HandlerFunc {
	if rate <= 0 || burst <= 0 {
		return func(c *gin.Context) { c.Next() }
	}

	return func(c *gin.Context) {
		if redisUnreachable(c) {
			limiterUnavailable(c, prefix, failClosed, errRedisUnreachable)
			return
		}
		key := fmt.Sprintf("ratelimit:%s:%s", prefix, keyFunc(c))
		ctx, cancel := context.WithTimeout(c.Request.Context(), rl.timeout)
		allowed, remaining, wait, err := rl.consumeToken(ctx, key, rate, burst, time.Now())
		cancel()
		if err != nil {
			limiterUnavailable(c, prefix, failClosed, err)
			return
		}
		if !allowed {
			rejectRateLimited(c, burst, wait)
			return
		}
		c.Header("X-RateLimit-Limit", strconv.Itoa(burst))
		c.Header("X-RateLimit-Remaining", strconv.Itoa(remaining))
		c.Next()
	}
}

func (rl *RateLimiter) fixedWindowMiddleware(prefix string, limit int, window time.Duration, failClosed bool, keyFunc func(*gin.Context) string) gin.HandlerFunc {
	if limit <= 0 {
		return func(c *gin.Context) { c.Next() }
	}

	return func(c *gin.Context) {
		if redisUnreachable(c) {
			limiterUnavailable(c, prefix, failClosed, errRedisUnreachable)
			return
		}
		key := fmt.Sprintf("ratelimit:%s:%s", prefix, keyFunc(c))
		ctx, cancel := context.WithTimeout(c.Request.Context(), rl.timeout)
		count, ttl, err := rl.incrementCounter(ctx, key, window)
		cancel()
		if err != nil {
			limiterUnavailable(c, prefix, failClosed, err)
			return
		}
		if count > int64(limit) {
			rejectRateLimited(c, limit, ttl)
			return
		}
		c.Header("X-RateLimit-Limit", strconv.Itoa(limit))
		c.Header("X-RateLimit-Remaining", strconv.FormatInt(int64(limit)-count, 10))
		c.Next()
	}
}

// tokenBucketScript takes a token from a bucket that holds up to burst tokens
// and refills at rate tokens per second. Tokens are fractional, so that any
// rate, also one above 1000 per second, refills correctly between requests
// that are less than a millisecond apart. It returns whether a token was
// taken, the whole tokens left, and the milliseconds until the next token.
var tokenBucketScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local burst = tonumber(ARGV[3])

local bucket = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(bucket[1])
local ts = tonumber(bucket[2])
if tokens == nil or ts == nil then
	tokens = burst
	ts = now
end
if now > ts then
	tokens = math.min(burst, tokens + (now - ts) * rate / 1000)
	ts = now
end

local allowed = 0
local wait = 0
if tokens >= 1 then
	tokens = tokens - 1
	allowed = 1
else
	wait = math.ceil((1 - tokens) * 1000 / rate)
end
redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'ts', ts)
redis.call('PEXPIRE', KEYS[1], math.ceil(burst * 1000 / rate) + 1000)
return {allowed, math.floor(tokens), wait}
`)

// consumeToken takes a token for the request at now.
func (rl *RateLimiter) consumeToken(ctx context.Context, key string, rate, burst int, now time.Time) (bool, int, time.Duration, error) {
	if rl.redis == nil || rl.redis.Client == nil {
		return false, 0, 0, fmt.Errorf("redis is not configured")
	}
	result, err := tokenBucketScript.Run(ctx, rl.redis.Client, []string{key}, now.UnixMilli(), rate, burst).Slice()
	if err != nil {
		return false, 0, 0, err
	}
	allowed := result[0].(int64) == 1
	remaining := int(result[1].(int64))
	wait := time.Duration(result[2].(int64)) * time.Millisecond
	return allowed, remaining, wait, nil
}

// fixedWindowScript counts a request and returns the count with the time
// left in the window, in milliseconds. The window starts with the first
// request and is not extended by later ones, so a blocked client is allowed
// again when it ends, however often it retries. A counter without an expiry
// gets one, so that it cannot block a client forever.
var fixedWindowScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
local ttl = redis.call('PTTL', KEYS[1])
if count == 1 or ttl < 0 then
	redis.call('PEXPIRE', KEYS[1], ARGV[1])
	ttl = tonumber(ARGV[1])
end
return {count, ttl}
`)

func (rl *RateLimiter) incrementCounter(ctx context.Context, key string, window time.Duration) (int64, time.Duration, error) {
	if rl.redis == nil || rl.redis.Client == nil {
		return 0, 0, fmt.Errorf("redis is not configured")
	}
	result, err := fixedWindowScript.Run(ctx, rl.redis.Client, []string{key}, window.Milliseconds()).Slice()
	if err != nil {
		return 0, 0, err
	}
	return result[0].(int64), time.Duration(result[1].(int64)) * time.Millisecond, nil
}

// getClientIP identifies the client for rate limiting. gin's ClientIP honors
// forwarding headers (X-Forwarded-For, X-Real-IP) only when the request comes
// from a proxy listed in server.trusted_proxies. Reading those headers directly
// would let every client choose a fresh rate-limit identity for each request.
func (rl *RateLimiter) getClientIP(c *gin.Context) string {
	return c.ClientIP()
}

// getUserID identifies the signed-in user or administrator by the subject
// and role of the access token, so that each of them has a budget of their
// own whatever address they use. A request without a token, which the
// authentication middleware normally rejects first, counts by its address.
func (rl *RateLimiter) getUserID(c *gin.Context) string {
	if claims, ok := auth.GetClaims(c); ok && claims.UUID != "" {
		return claims.Role + ":" + claims.UUID
	}
	return "ip:" + rl.getClientIP(c)
}
