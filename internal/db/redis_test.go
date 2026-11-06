package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/testutil/redistest"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRedisService_Success(t *testing.T) {
	originalConfig := config.Config
	defer func() { config.Config = originalConfig }()

	config.Config.Redis.Host = "localhost"
	config.Config.Redis.Port = "6379"
	config.Config.Redis.DB = redistest.DB
	config.Config.Redis.ConnectRetries = 1
	config.Config.Redis.ConnectRetryDelayMs = 10

	svc, err := NewRedisService()
	require.NoError(t, err)
	require.NotNil(t, svc)
	defer svc.Close()

	assert.NoError(t, svc.Ping(context.Background()))
}

// TestNewRedisService_UsesTheConfiguredTimeouts checks the client options:
// the configured timeouts and retries, and short defaults instead of the
// client library's, which held requests for seconds while Redis was down.
func TestNewRedisService_UsesTheConfiguredTimeouts(t *testing.T) {
	originalConfig := config.Config
	defer func() { config.Config = originalConfig }()
	connect := func() *redis.Options {
		config.Config.Redis.Host, config.Config.Redis.Port, config.Config.Redis.DB = "localhost", "6379", redistest.DB
		config.Config.Redis.ConnectRetries, config.Config.Redis.ConnectRetryDelayMs = 1, 10
		svc, err := NewRedisService()
		require.NoError(t, err)
		t.Cleanup(func() { svc.Close() })
		return svc.Client.Options()
	}

	config.Config = config.Configuration{}
	opts := connect()
	assert.Equal(t, time.Second, opts.DialTimeout, "not set: the defaults of the configuration")
	assert.Equal(t, time.Second, opts.ReadTimeout)
	assert.Equal(t, time.Second, opts.WriteTimeout)
	assert.Equal(t, 2*time.Second, opts.PoolTimeout)
	assert.Equal(t, 1, opts.MaxRetries)
	assert.True(t, opts.ContextTimeoutEnabled, "request deadlines, such as rate_limits.timeout_ms, bound Redis calls")
	assert.Equal(t, 1, opts.DialerRetries, "an attempt dials once, so dial_timeout_ms bounds it")

	config.Config = config.Configuration{}
	r := &config.Config.Redis
	r.DialTimeoutMs, r.ReadTimeoutMs, r.WriteTimeoutMs, r.PoolTimeoutMs, r.MaxRetries = 300, 400, 500, 600, -1
	opts = connect()
	assert.Equal(t, 300*time.Millisecond, opts.DialTimeout)
	assert.Equal(t, 400*time.Millisecond, opts.ReadTimeout)
	assert.Equal(t, 500*time.Millisecond, opts.WriteTimeout)
	assert.Equal(t, 600*time.Millisecond, opts.PoolTimeout)
	assert.Equal(t, 0, opts.MaxRetries, "retries turned off: the client stores -1 as 0, and 0 as its default of 3")
}

func TestNewRedisService_FailsWithBadHost(t *testing.T) {
	originalConfig := config.Config
	defer func() { config.Config = originalConfig }()

	config.Config.Redis.Host = "no-such-redis-host-xyz"
	config.Config.Redis.Port = "6379"
	config.Config.Redis.ConnectRetries = 2
	config.Config.Redis.ConnectRetryDelayMs = 10

	svc, err := NewRedisService()
	assert.Error(t, err)
	assert.Nil(t, svc)
}

func TestRedisService_PingNilClient(t *testing.T) {
	svc := &RedisService{}
	err := svc.Ping(context.Background())
	assert.Error(t, err)
}

func TestRedisService_StatsNilClient(t *testing.T) {
	svc := &RedisService{}
	assert.Nil(t, svc.Stats())
}

func TestRedisService_CloseNilClient(t *testing.T) {
	svc := &RedisService{}
	assert.NoError(t, svc.Close())
}

func TestRedisService_StatsAfterConnect(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: redistest.DB})
	require.NoError(t, client.Ping(context.Background()).Err())
	svc := &RedisService{Client: client}
	defer svc.Close()

	stats := svc.Stats()
	require.NotNil(t, stats)
}

func TestRedisPingWithRetry_Succeeds(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: redistest.DB})
	defer client.Close()

	err := redisPingWithRetry(client, 3, 10*time.Millisecond)
	assert.NoError(t, err)
}

func TestRedisPingWithRetry_ExhaustsAttempts(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DB: redistest.DB})
	defer client.Close()

	start := time.Now()
	err := redisPingWithRetry(client, 2, 20*time.Millisecond)
	elapsed := time.Since(start)

	assert.Error(t, err)
	assert.GreaterOrEqual(t, elapsed, 20*time.Millisecond)
}

func TestRedisPingWithRetry_NormalizesInvalidArguments(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: redistest.DB})
	defer client.Close()

	err := redisPingWithRetry(client, 0, 0)
	assert.NoError(t, err)
}

// TestServices_CloseIsIdempotent closes the Redis and PostgreSQL services
// twice, as a shutdown that runs into a second signal may.
func TestServices_CloseIsIdempotent(t *testing.T) {
	originalConfig := config.Config
	defer func() { config.Config = originalConfig }()
	config.Config.Redis.Host, config.Config.Redis.Port, config.Config.Redis.DB = "localhost", "6379", redistest.DB
	config.Config.Redis.ConnectRetries, config.Config.Redis.ConnectRetryDelayMs = 1, 10

	redisService, err := NewRedisService()
	require.NoError(t, err)
	assert.NoError(t, redisService.Close())
	assert.NoError(t, redisService.Close(), "a second Close must not fail")
	assert.NoError(t, (&RedisService{}).Close())

	ensureMigrationsTestDB(t)
	conn, err := sql.Open("pgx", migrationsTestDBConnStr())
	require.NoError(t, err)
	require.NoError(t, conn.Ping())
	postgres := &PostgresService{DB: conn}
	assert.NoError(t, postgres.Close())
	assert.NoError(t, postgres.Close(), "a second Close must not fail")
}
