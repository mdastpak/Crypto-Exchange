package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/redisutil"
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

	svc, err := NewRedisService(&config.Config)
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
		svc, err := NewRedisService(&config.Config)
		require.NoError(t, err)
		t.Cleanup(func() { svc.Close() })
		return svc.Client.(*redis.Client).Options()
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

	svc, err := NewRedisService(&config.Config)
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

	redisService, err := NewRedisService(&config.Config)
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

// TestNewRedisClient_BuildsTheClientOfEachMode checks the client of each
// redis.mode (#231), without connecting: the sentinel mode follows the
// primary of the named master through the sentinels, the cluster mode
// knows its seed nodes, and both carry the credentials, pool, timeouts,
// and TLS of the standalone mode. With TLS, the sentinel and cluster
// clients check the name of each node they dial, not of redis.host.
func TestNewRedisClient_BuildsTheClientOfEachMode(t *testing.T) {
	originalConfig := config.Config
	t.Cleanup(func() { config.Config = originalConfig })
	base := func(mode string) *config.Configuration {
		config.Config = config.Configuration{}
		r := &config.Config.Redis
		r.Mode, r.Username, r.Password = mode, "exchange", "data-secret"
		r.PoolSize, r.MinIdleConns, r.ReadTimeoutMs = 7, 2, 400
		r.TLS.Enabled = true
		return &config.Config
	}

	c := base(config.RedisModeStandalone)
	c.Redis.Host, c.Redis.Port = "redis.internal", "6380"
	client, err := NewRedisClient(&config.Config)
	require.NoError(t, err)
	standalone, ok := client.(*redis.Client)
	require.True(t, ok, "%T", client)
	assert.Equal(t, "redis.internal:6380", standalone.Options().Addr)
	assert.Equal(t, "redis.internal", standalone.Options().TLSConfig.ServerName)
	require.NoError(t, client.Close())

	c = base(config.RedisModeSentinel)
	c.Redis.Sentinel.MasterName = "crypto-exchange"
	c.Redis.Sentinel.Addresses = []string{"s-1:26379", " s-2:26379 "}
	c.Redis.Sentinel.Username, c.Redis.Sentinel.Password = "watcher", "sentinel-secret"
	client, err = NewRedisClient(&config.Config)
	require.NoError(t, err)
	failover, ok := client.(*redis.Client)
	require.True(t, ok, "a sentinel client is a *redis.Client that follows the primary: %T", client)
	opts := failover.Options()
	assert.Equal(t, "FailoverClient", opts.Addr, "the address comes from the sentinels")
	assert.Equal(t, []string{"exchange", "data-secret"}, []string{opts.Username, opts.Password}, "the data nodes use redis.username and redis.password")
	assert.Equal(t, []int{7, 2}, []int{opts.PoolSize, opts.MinIdleConns})
	assert.Equal(t, 400*time.Millisecond, opts.ReadTimeout)
	assert.True(t, opts.ContextTimeoutEnabled)
	assert.Empty(t, opts.TLSConfig.ServerName, "each node's own name is checked")
	require.NoError(t, client.Close())

	c = base(config.RedisModeCluster)
	c.Redis.Cluster.Addresses = []string{"r-1:6379", "r-2:6379 "}
	client, err = NewRedisClient(&config.Config)
	require.NoError(t, err)
	cluster, ok := client.(*redis.ClusterClient)
	require.True(t, ok, "%T", client)
	copts := cluster.Options()
	assert.Equal(t, []string{"r-1:6379", "r-2:6379"}, copts.Addrs)
	assert.Equal(t, []string{"exchange", "data-secret"}, []string{copts.Username, copts.Password})
	assert.Equal(t, 7, copts.PoolSize)
	assert.Equal(t, 400*time.Millisecond, copts.ReadTimeout)
	assert.False(t, copts.ReadOnly, "every command goes to the primary of its slot")
	assert.Equal(t, redisutil.ClusterStateReloadInterval, copts.ClusterStateReloadInterval, "not go-redis's minute")
	assert.True(t, copts.ContextTimeoutEnabled)
	assert.Empty(t, copts.TLSConfig.ServerName)
	require.NoError(t, client.Close())
}
