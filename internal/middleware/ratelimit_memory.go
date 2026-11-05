package middleware

import (
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// memoryBuckets holds a token bucket per key in the memory of this instance,
// for the routes that must not depend on Redis. The buckets refill as those
// in Redis do. A bucket that has been idle long enough to refill completely
// is the same as a new one, so such buckets are dropped from time to time,
// and the memory used follows the number of recent clients.
type memoryBuckets struct {
	rate, burst float64
	// idle is how long an empty bucket takes to refill completely.
	idle time.Duration

	mu      sync.Mutex
	buckets map[string]*memoryBucket
	swept   time.Time
}

type memoryBucket struct {
	tokens float64
	ts     time.Time
}

func newMemoryBuckets(rate, burst int) *memoryBuckets {
	return &memoryBuckets{
		rate:    float64(rate),
		burst:   float64(burst),
		idle:    time.Duration(float64(burst) / float64(rate) * float64(time.Second)),
		buckets: make(map[string]*memoryBucket),
	}
}

// take takes a token from the bucket of key at now. It returns whether a
// token was taken, the whole tokens left, and the wait until the next token.
func (m *memoryBuckets) take(key string, now time.Time) (bool, int, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if now.Sub(m.swept) >= m.idle {
		for k, b := range m.buckets {
			if now.Sub(b.ts) >= m.idle {
				delete(m.buckets, k)
			}
		}
		m.swept = now
	}

	b, ok := m.buckets[key]
	if !ok {
		b = &memoryBucket{tokens: m.burst, ts: now}
		m.buckets[key] = b
	}
	if now.After(b.ts) {
		b.tokens = math.Min(m.burst, b.tokens+now.Sub(b.ts).Seconds()*m.rate)
		b.ts = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, int(b.tokens), 0
	}
	return false, 0, time.Duration(math.Ceil((1 - b.tokens) / m.rate * float64(time.Second)))
}

// size returns the number of buckets held.
func (m *memoryBuckets) size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.buckets)
}

// memoryTokenBucketMiddleware limits requests as tokenBucketMiddleware does,
// but in the memory of this instance, so it works while Redis is down. Each
// instance counts on its own.
func memoryTokenBucketMiddleware(rate, burst int, keyFunc func(*gin.Context) string) gin.HandlerFunc {
	if rate <= 0 || burst <= 0 {
		return func(c *gin.Context) { c.Next() }
	}

	buckets := newMemoryBuckets(rate, burst)
	return func(c *gin.Context) {
		allowed, remaining, wait := buckets.take(keyFunc(c), time.Now())
		if !allowed {
			rejectRateLimited(c, burst, wait)
			return
		}
		c.Header("X-RateLimit-Limit", strconv.Itoa(burst))
		c.Header("X-RateLimit-Remaining", strconv.Itoa(remaining))
		c.Next()
	}
}
