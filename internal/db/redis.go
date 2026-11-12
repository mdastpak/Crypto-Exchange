package db

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/redisutil"
	"github.com/mdastpak/crypto-exchange/internal/tracing"

	"github.com/redis/go-redis/v9"
)

// RedisService holds the client of every Redis mode (#231): a standalone
// node, a primary that sentinels monitor, or a cluster.
type RedisService struct {
	Client redis.UniversalClient

	closeOnce sync.Once
	closeErr  error
}

func NewRedisService(cfg *config.Configuration) (*RedisService, error) {
	client, err := NewRedisClient(cfg)
	if err != nil {
		return nil, err
	}

	if err := redisPingWithRetry(client, cfg.Redis.ConnectRetries, time.Duration(cfg.Redis.ConnectRetryDelayMs)*time.Millisecond); err != nil {
		client.Close()
		return nil, fmt.Errorf("error connecting to Redis: %w", err)
	}

	log.Printf("Redis connected successfully (%s mode).", cfg.Redis.Mode)

	return &RedisService{
		Client: client,
	}, nil
}

// NewRedisClient returns the client of redis.mode, without connecting:
// redis.NewClient for the standalone mode, redis.NewFailoverClient for the
// sentinel mode, and redis.NewClusterClient for the cluster mode. The
// credentials, the TLS settings, the pool, and the timeouts apply in every
// mode, to every node.
func NewRedisClient(cfg *config.Configuration) (redis.UniversalClient, error) {
	client, err := newRedisClient(cfg)
	if err != nil {
		return nil, err
	}
	// A request that fails because Redis is away answers 503, not 500
	// (#130).
	redisutil.NoteUnavailable(client)
	// Each command is a span of the trace of its request (#50).
	client.AddHook(tracing.RedisHook{})
	return client, nil
}

// newRedisClient builds the client of the mode.
func newRedisClient(c *config.Configuration) (redis.UniversalClient, error) {
	cfg := c.Redis
	maxRetries := cfg.MaxRetries
	if maxRetries == 0 {
		maxRetries = config.DefaultRedisMaxRetries
	}
	// In the sentinel and cluster modes the client connects to several
	// nodes. Without redis.tls.server_name, each TLS connection then checks
	// the name of the node it dials, which crypto/tls takes from the
	// address when ServerName is empty.
	tlsHost := cfg.Host
	if cfg.Mode == config.RedisModeSentinel || cfg.Mode == config.RedisModeCluster {
		tlsHost = ""
	}
	tlsConfig, err := cfg.TLS.TLSConfig(tlsHost)
	if err != nil {
		return nil, fmt.Errorf("redis.tls: %w", err)
	}
	dialTimeout := config.Milliseconds(cfg.DialTimeoutMs, config.DefaultRedisDialTimeoutMs)
	readTimeout := config.Milliseconds(cfg.ReadTimeoutMs, config.DefaultRedisReadTimeoutMs)
	writeTimeout := config.Milliseconds(cfg.WriteTimeoutMs, config.DefaultRedisWriteTimeoutMs)
	poolTimeout := config.Milliseconds(cfg.PoolTimeoutMs, config.DefaultRedisPoolTimeoutMs)

	switch cfg.Mode {
	case config.RedisModeSentinel:
		return redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:       cfg.Sentinel.MasterName,
			SentinelAddrs:    trimmed(cfg.Sentinel.Addresses),
			SentinelUsername: cfg.Sentinel.Username,
			SentinelPassword: cfg.Sentinel.Password,
			Username:         cfg.Username,
			Password:         cfg.Password,
			TLSConfig:        tlsConfig,
			DB:               cfg.DB,
			PoolSize:         cfg.PoolSize,
			MinIdleConns:     cfg.MinIdleConns,
			DialTimeout:      dialTimeout,
			ReadTimeout:      readTimeout,
			WriteTimeout:     writeTimeout,
			PoolTimeout:      poolTimeout,
			MaxRetries:       maxRetries,
			DialerRetries:    1,
			// Every command goes to the primary, also reads: a replica
			// may lag behind, and the counters must be read where they
			// are written.
			ContextTimeoutEnabled: true,
		}), nil
	case config.RedisModeCluster:
		cluster := redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:         trimmed(cfg.Cluster.Addresses),
			Username:      cfg.Username,
			Password:      cfg.Password,
			TLSConfig:     tlsConfig,
			PoolSize:      cfg.PoolSize,
			MinIdleConns:  cfg.MinIdleConns,
			DialTimeout:   dialTimeout,
			ReadTimeout:   readTimeout,
			WriteTimeout:  writeTimeout,
			PoolTimeout:   poolTimeout,
			MaxRetries:    maxRetries,
			DialerRetries: 1,
			// ReadOnly stays off: every command goes to the primary of its
			// slot, for the same reason as in the sentinel mode.
			ContextTimeoutEnabled:      true,
			ClusterStateReloadInterval: redisutil.ClusterStateReloadInterval,
		})
		redisutil.FollowClusterFailovers(cluster)
		return cluster, nil
	}
	return redis.NewClient(&redis.Options{
		Addr:         net.JoinHostPort(cfg.Host, cfg.Port),
		Username:     cfg.Username,
		Password:     cfg.Password,
		TLSConfig:    tlsConfig,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		MinIdleConns: cfg.MinIdleConns,
		DialTimeout:  dialTimeout,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		PoolTimeout:  poolTimeout,
		MaxRetries:   maxRetries,
		// One dial per attempt, as go-redis v8 did, so that a command waits
		// for dial_timeout_ms per attempt, not five times as long: v9 dials
		// up to five times by default. max_retries still retries a command.
		DialerRetries: 1,
		// The deadline of a request's context also bounds its Redis calls,
		// such as rate_limits.timeout_ms for the limiters. go-redis v8 did
		// this by default; v9 needs it turned on.
		ContextTimeoutEnabled: true,
	}), nil
}

// trimmed returns the addresses without the spaces around them.
func trimmed(addresses []string) []string {
	out := make([]string, 0, len(addresses))
	for _, a := range addresses {
		out = append(out, strings.TrimSpace(a))
	}
	return out
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
func redisPingWithRetry(client redis.UniversalClient, maxAttempts int, delay time.Duration) error {
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
