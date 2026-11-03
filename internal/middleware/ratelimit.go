package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"encrypted-db/config"
	"encrypted-db/internal/db"
	"encrypted-db/internal/helpers"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
)

type RateLimiter struct {
	redis *db.RedisService
}

func NewRateLimiter(redis *db.RedisService) *RateLimiter {
	return &RateLimiter{redis: redis}
}

func (rl *RateLimiter) RESTRateLimit() gin.HandlerFunc {
	cfg := config.Config.RateLimits.REST
	return rl.tokenBucketMiddleware("rest", cfg.RequestsPerSecond, cfg.Burst, rl.getClientIP)
}

func (rl *RateLimiter) WSRateLimit() gin.HandlerFunc {
	cfg := config.Config.RateLimits.WS
	return rl.tokenBucketMiddleware("ws", cfg.MessagesPerSecond, cfg.Burst, rl.getClientIP)
}

func (rl *RateLimiter) TradingRateLimit() gin.HandlerFunc {
	cfg := config.Config.RateLimits.Trading
	return rl.tokenBucketMiddleware("trading", cfg.OrdersPerSecond, cfg.Burst, rl.getUserID)
}

func (rl *RateLimiter) AuthRateLimit(action string) gin.HandlerFunc {
	cfg := config.Config.RateLimits.Auth
	var limit, burst int
	var keyPrefix string

	switch action {
	case "login":
		keyPrefix = "auth:login"
		limit = cfg.LoginAttemptsPerMinute
		burst = cfg.LoginAttemptsPerMinute
	case "otp":
		keyPrefix = "auth:otp"
		limit = cfg.OTPRequestsPerHour
		burst = cfg.OTPRequestsPerHour
	default:
		keyPrefix = "auth:default"
		limit = 10
		burst = 10
	}

	return rl.fixedWindowMiddleware(keyPrefix, limit, burst, time.Minute, rl.getClientIP)
}

func (rl *RateLimiter) tokenBucketMiddleware(prefix string, rate, burst int, keyFunc func(*gin.Context) string) gin.HandlerFunc {
	if rate <= 0 || burst <= 0 {
		return func(c *gin.Context) { c.Next() }
	}

	return func(c *gin.Context) {
		key := fmt.Sprintf("ratelimit:%s:%s", prefix, keyFunc(c))
		allowed, retryAfter := rl.consumeToken(c.Request.Context(), key, rate, burst)
		if !allowed {
			c.Header("X-RateLimit-Limit", strconv.Itoa(burst))
			c.Header("X-RateLimit-Remaining", "0")
			c.Header("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
			helpers.SendResponse(c, http.StatusTooManyRequests, "Rate limit exceeded", nil)
			c.Abort()
			return
		}
		c.Header("X-RateLimit-Limit", strconv.Itoa(burst))
		c.Next()
	}
}

func (rl *RateLimiter) fixedWindowMiddleware(prefix string, limit, burst int, window time.Duration, keyFunc func(*gin.Context) string) gin.HandlerFunc {
	if limit <= 0 || burst <= 0 {
		return func(c *gin.Context) { c.Next() }
	}

	return func(c *gin.Context) {
		key := fmt.Sprintf("ratelimit:%s:%s", prefix, keyFunc(c))
		count, ttl := rl.incrementCounter(c.Request.Context(), key, window)
		if count > int64(limit) {
			c.Header("X-RateLimit-Limit", strconv.Itoa(limit))
			c.Header("X-RateLimit-Remaining", "0")
			c.Header("Retry-After", strconv.Itoa(int(ttl.Seconds())))
			helpers.SendResponse(c, http.StatusTooManyRequests, "Rate limit exceeded", nil)
			c.Abort()
			return
		}
		c.Header("X-RateLimit-Limit", strconv.Itoa(limit))
		c.Header("X-RateLimit-Remaining", strconv.FormatInt(int64(limit)-count, 10))
		c.Next()
	}
}

func (rl *RateLimiter) consumeToken(ctx context.Context, key string, rate, burst int) (bool, time.Duration) {
	now := time.Now().UnixMilli()
	windowMs := int64(1000 / rate)

	script := redis.NewScript(`
		local key = KEYS[1]
		local now = tonumber(ARGV[1])
		local rate = tonumber(ARGV[2])
		local burst = tonumber(ARGV[3])
		local window = tonumber(ARGV[4])

		local bucket = redis.call('HMGET', key, 'tokens', 'last_refill')
		local tokens = tonumber(bucket[1])
		local lastRefill = tonumber(bucket[2])

		if tokens == nil then
			tokens = burst
			lastRefill = now
		end

		local elapsed = now - lastRefill
		local refillTokens = math.floor(elapsed / window)
		tokens = math.min(burst, tokens + refillTokens)

		if tokens >= 1 then
			tokens = tokens - 1
			redis.call('HMSET', key, 'tokens', tokens, 'last_refill', now)
			redis.call('EXPIRE', key, math.ceil(window * burst / 1000) + 1)
			return {1, 0}
		else
			local waitTime = (window * (1 - tokens)) - (elapsed % window)
			redis.call('HMSET', key, 'tokens', tokens, 'last_refill', lastRefill)
			return {0, waitTime}
		end
	`)

	result, err := script.Run(ctx, rl.redis.Client, []string{key}, now, rate, burst, windowMs).Slice()
	if err != nil {
		return true, 0
	}

	allowed := result[0].(int64) == 1
	waitMs := result[1].(int64)
	return allowed, time.Duration(waitMs) * time.Millisecond
}

func (rl *RateLimiter) incrementCounter(ctx context.Context, key string, window time.Duration) (int64, time.Duration) {
	pipe := rl.redis.Client.Pipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, window)
	_, err := pipe.Exec(ctx)
	if err != nil {
		return 0, window
	}
	count := incr.Val()
	ttl, _ := rl.redis.Client.TTL(ctx, key).Result()
	if ttl < 0 {
		ttl = window
	}
	return count, ttl
}

// getClientIP identifies the client for rate limiting. gin's ClientIP honors
// forwarding headers (X-Forwarded-For, X-Real-IP) only when the request comes
// from a proxy listed in server.trusted_proxies. Reading those headers directly
// would let every client choose a fresh rate-limit identity for each request.
func (rl *RateLimiter) getClientIP(c *gin.Context) string {
	return c.ClientIP()
}

func (rl *RateLimiter) getUserID(c *gin.Context) string {
	if userID, exists := c.Get("user_id"); exists {
		return fmt.Sprintf("user:%v", userID)
	}
	return rl.getClientIP(c)
}
