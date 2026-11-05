package db

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/mdastpak/crypto-exchange/config"

	"github.com/go-redis/redis/v8"
)

type RedisService struct {
	Client *redis.Client
}

func NewRedisService() (*RedisService, error) {
	cfg := config.Config.Redis
	maxRetries := cfg.MaxRetries
	if maxRetries == 0 {
		maxRetries = config.DefaultRedisMaxRetries
	}
	opts := &redis.Options{
		Addr:         fmt.Sprintf("%s:%s", cfg.Host, cfg.Port),
		Password:     cfg.Password,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		MinIdleConns: cfg.MinIdleConns,
		DialTimeout:  config.Milliseconds(cfg.DialTimeoutMs, config.DefaultRedisDialTimeoutMs),
		ReadTimeout:  config.Milliseconds(cfg.ReadTimeoutMs, config.DefaultRedisReadTimeoutMs),
		WriteTimeout: config.Milliseconds(cfg.WriteTimeoutMs, config.DefaultRedisWriteTimeoutMs),
		PoolTimeout:  config.Milliseconds(cfg.PoolTimeoutMs, config.DefaultRedisPoolTimeoutMs),
		MaxRetries:   maxRetries,
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

func (r *RedisService) Close() error {
	if r.Client != nil {
		if err := r.Client.Close(); err != nil {
			log.Printf("Error closing Redis connection: %v", err)
			return err
		}
	}
	return nil
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
