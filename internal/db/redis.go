package db

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/redis/go-redis/v9"
)

type RedisService struct {
	Client *redis.Client

	closeOnce sync.Once
	closeErr  error
}

func NewRedisService() (*RedisService, error) {
	cfg := config.Config.Redis
	maxRetries := cfg.MaxRetries
	if maxRetries == 0 {
		maxRetries = config.DefaultRedisMaxRetries
	}
	tlsConfig, err := cfg.TLS.TLSConfig(cfg.Host)
	if err != nil {
		return nil, fmt.Errorf("redis.tls: %w", err)
	}
	opts := &redis.Options{
		Addr:         net.JoinHostPort(cfg.Host, cfg.Port),
		Username:     cfg.Username,
		Password:     cfg.Password,
		TLSConfig:    tlsConfig,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		MinIdleConns: cfg.MinIdleConns,
		DialTimeout:  config.Milliseconds(cfg.DialTimeoutMs, config.DefaultRedisDialTimeoutMs),
		ReadTimeout:  config.Milliseconds(cfg.ReadTimeoutMs, config.DefaultRedisReadTimeoutMs),
		WriteTimeout: config.Milliseconds(cfg.WriteTimeoutMs, config.DefaultRedisWriteTimeoutMs),
		PoolTimeout:  config.Milliseconds(cfg.PoolTimeoutMs, config.DefaultRedisPoolTimeoutMs),
		MaxRetries:   maxRetries,
		// One dial per attempt, as go-redis v8 did, so that a command waits
		// for dial_timeout_ms per attempt, not five times as long: v9 dials
		// up to five times by default. max_retries still retries a command.
		DialerRetries: 1,
		// The deadline of a request's context also bounds its Redis calls,
		// such as rate_limits.timeout_ms for the limiters. go-redis v8 did
		// this by default; v9 needs it turned on.
		ContextTimeoutEnabled: true,
	}

	client := redis.NewClient(opts)

	if err := redisPingWithRetry(client, config.Config.Redis.ConnectRetries, time.Duration(config.Config.Redis.ConnectRetryDelayMs)*time.Millisecond); err != nil {
		client.Close()
		return nil, fmt.Errorf("error connecting to Redis: %w", err)
	}

	log.Println("Redis connected successfully.")

	return &RedisService{
		Client: client,
	}, nil
}

// Close closes the client once; later calls return the result of the first.
func (r *RedisService) Close() error {
	r.closeOnce.Do(func() {
		if r.Client == nil {
			return
		}
		if err := r.Client.Close(); err != nil {
			log.Printf("Error closing Redis connection: %v", err)
			r.closeErr = err
		}
	})
	return r.closeErr
}

func (r *RedisService) Ping(ctx context.Context) error {
	if r.Client == nil {
		return fmt.Errorf("redis not initialized")
	}
	return r.Client.Ping(ctx).Err()
}

func (r *RedisService) Stats() *redis.PoolStats {
	if r.Client == nil {
		return nil
	}
	return r.Client.PoolStats()
}

// redisPingWithRetry pings Redis up to maxAttempts times with an exponential
// backoff between attempts, so transient startup ordering issues do not fail
// the service. A maxAttempts value <= 1 performs a single attempt with no retry.
func redisPingWithRetry(client *redis.Client, maxAttempts int, delay time.Duration) error {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if delay <= 0 {
		delay = 500 * time.Millisecond
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		lastErr = client.Ping(ctx).Err()
		cancel()
		if lastErr == nil {
			return nil
		}
		if attempt < maxAttempts {
			log.Printf("Redis ping attempt %d/%d failed: %v, retrying in %v", attempt, maxAttempts, lastErr, delay)
			time.Sleep(delay)
			delay *= 2
		}
	}
	return lastErr
}
