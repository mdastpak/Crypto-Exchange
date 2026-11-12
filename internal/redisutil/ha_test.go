package redisutil_test

// The failover tests of #231. They need a Sentinel setup and a Cluster, which
// scripts/redis-ha/failover-drill.sh starts and describes in the REDIS_HA_*
// variables; without them, the tests are skipped. Each test stops a primary,
// so the drill runs them once, against nodes of their own.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/redisutil"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// durable is the durability of the drill: one replica must hold every
// counter. Each primary has two replicas, so that the promoted one still
// has a replica after the failover, and durable writes go on.
var durable = redisutil.Durability{Replicas: 1, Timeout: 2 * time.Second}

// haService connects as the server does, with redis.mode set to mode and
// the nodes of the drill, and skips the test without them.
func haService(t *testing.T, mode string) *db.RedisService {
	t.Helper()
	saved := config.Config
	t.Cleanup(func() { config.Config = saved })
	config.Config = config.Configuration{}
	r := &config.Config.Redis
	r.Mode, r.PoolSize, r.ConnectRetries, r.ConnectRetryDelayMs = mode, 10, 3, 200
	r.MaxRetries = 3
	switch mode {
	case config.RedisModeSentinel:
		addresses := os.Getenv("REDIS_HA_SENTINEL_ADDRESSES")
		if addresses == "" {
			t.Skip("REDIS_HA_SENTINEL_ADDRESSES is not set; scripts/redis-ha/failover-drill.sh runs this test")
		}
		r.Sentinel.MasterName = os.Getenv("REDIS_HA_SENTINEL_MASTER")
		r.Sentinel.Addresses = strings.Split(addresses, ",")
	case config.RedisModeCluster:
		addresses := os.Getenv("REDIS_HA_CLUSTER_ADDRESSES")
		if addresses == "" {
			t.Skip("REDIS_HA_CLUSTER_ADDRESSES is not set; scripts/redis-ha/failover-drill.sh runs this test")
		}
		r.Cluster.Addresses = strings.Split(addresses, ",")
	}
	svc, err := db.NewRedisService(&config.Config)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

// stop stops the Redis node at addr as a crash would: SHUTDOWN NOSAVE
// ends the process without saving, and the connection with it.
func stop(t *testing.T, addr string) {
	t.Helper()
	node := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	defer node.Close()
	// The node closes the connection instead of answering, so the error of
	// SHUTDOWN says nothing; the ping that follows must fail.
	_ = node.Shutdown(context.Background()).Err()
	require.Error(t, node.Ping(context.Background()).Err(), "%s is down", addr)
	t.Logf("stopped the primary at %s", addr)
}

// attemptsOf reads the attempts that a session's hash holds on the current
// primary, through the client of the store.
func attemptsOf(t *testing.T, client redis.UniversalClient, session string) int {
	t.Helper()
	var attempts int
	require.Eventually(t, func() bool {
		var err error
		attempts, err = client.HGet(context.Background(), "otp:session:"+session, "attempts").Int()
		return err == nil
	}, 30*time.Second, 200*time.Millisecond)
	return attempts
}

// failAttempts makes n wrong attempts at a code, each of which must be
// counted and refused as a wrong code.
func failAttempts(t *testing.T, store *otp.Store, issued otp.Issued, n int) {
	t.Helper()
	wrong := "000000"
	if issued.Code == wrong {
		wrong = "111111"
	}
	for i := range n {
		_, err := store.Verify(context.Background(), issued.SessionID, wrong)
		require.ErrorIs(t, err, otp.ErrInvalidCode, "attempt %d", i+1)
	}
}

// TestHA_SentinelFailoverKeepsTheAttemptsMade makes attempts at a code,
// blacklists a token, kills the primary, and checks after the promotion
// that the attempts were not given back, that the token is still
// blacklisted, and that signing in works on the new primary.
func TestHA_SentinelFailoverKeepsTheAttemptsMade(t *testing.T) {
	svc := haService(t, config.RedisModeSentinel)
	ctx := context.Background()
	store := otp.NewStore(svc.Client, otp.Settings{AttemptsPerCode: 5, ContactAttemptsPerHour: 50, Durability: durable})
	blacklist := &auth.TokenBlacklist{RedisClient: svc.Client, Prefix: "blacklist:token:", Durability: durable}

	issued, err := store.Issue(ctx, "failover@example.com", "client")
	require.NoError(t, err)
	failAttempts(t, store, issued, 3)
	require.NoError(t, blacklist.AddTokenToBlacklist(ctx, "an-admin-token", time.Now().Add(time.Hour)))

	sentinel := redis.NewSentinelClient(&redis.Options{Addr: config.Config.Redis.Sentinel.Addresses[0]})
	defer sentinel.Close()
	before, err := sentinel.GetMasterAddrByName(ctx, config.Config.Redis.Sentinel.MasterName).Result()
	require.NoError(t, err)
	stop(t, before[0]+":"+before[1])

	require.Eventually(t, func() bool {
		after, err := sentinel.GetMasterAddrByName(ctx, config.Config.Redis.Sentinel.MasterName).Result()
		return err == nil && after[1] != before[1]
	}, 60*time.Second, 250*time.Millisecond, "the sentinels promote a replica")

	// The client finds the new primary itself. The attempts made before
	// the failover are all there: the next two use up the code.
	assert.Equal(t, 3, attemptsOf(t, svc.Client, issued.SessionID), "no attempt was given back")
	failAttempts(t, store, issued, 2)
	_, err = store.Verify(ctx, issued.SessionID, issued.Code)
	require.ErrorIs(t, err, otp.ErrTooManyAttempts, "even the right code is refused after the fifth attempt")

	blacklisted, err := blacklist.IsTokenBlacklisted(ctx, "an-admin-token")
	require.NoError(t, err)
	assert.True(t, blacklisted, "the logged-out token stays logged out")

	fresh, err := store.Issue(ctx, "after-failover@example.com", "client")
	require.NoError(t, err)
	contact, err := store.Verify(ctx, fresh.SessionID, fresh.Code)
	require.NoError(t, err)
	assert.Equal(t, "after-failover@example.com", contact)
}

// TestHA_ClusterRunsTheStoresAndSurvivesAFailover signs in with many
// contacts, whose keys spread over every primary of the cluster, which
// fails with CROSSSLOT if a script mixes slots. Then it kills the primary
// of one session and checks that its attempts survive the promotion of
// the replica, and that signing in works again.
func TestHA_ClusterRunsTheStoresAndSurvivesAFailover(t *testing.T) {
	svc := haService(t, config.RedisModeCluster)
	ctx := context.Background()
	cluster, ok := svc.Client.(*redis.ClusterClient)
	require.True(t, ok, "%T", svc.Client)
	store := otp.NewStore(svc.Client, otp.Settings{AttemptsPerCode: 5, ContactAttemptsPerHour: 50, Durability: durable})
	blacklist := &auth.TokenBlacklist{RedisClient: svc.Client, Prefix: "blacklist:token:", Durability: durable}

	primaries := map[string]bool{}
	for i := range 60 {
		contact := fmt.Sprintf("cluster-%d@example.com", i)
		issued, err := store.Issue(ctx, contact, fmt.Sprintf("10.1.0.%d", i))
		require.NoError(t, err, contact)
		if i%2 == 0 {
			failAttempts(t, store, issued, 1)
		}
		got, err := store.Verify(ctx, issued.SessionID, issued.Code)
		require.NoError(t, err, contact)
		assert.Equal(t, contact, got)

		node, err := cluster.MasterForKey(ctx, "otp:session:"+issued.SessionID)
		require.NoError(t, err)
		primaries[node.Options().Addr] = true
		require.NoError(t, blacklist.AddTokenToBlacklist(ctx, fmt.Sprintf("token-%d", i), time.Now().Add(time.Hour)))
	}
	assert.Len(t, primaries, 3, "the sessions spread over every primary")

	issued, err := store.Issue(ctx, "cluster-failover@example.com", "client")
	require.NoError(t, err)
	failAttempts(t, store, issued, 3)
	node, err := cluster.MasterForKey(ctx, "otp:session:"+issued.SessionID)
	require.NoError(t, err)
	stop(t, node.Options().Addr)
	stopped := time.Now()

	// The cluster promotes the replica after cluster-node-timeout (2 s in
	// the drill); until then, commands for the slots of the stopped
	// primary fail. The client must follow within seconds: without
	// redisutil.FollowClusterFailovers it kept dialing the dead primary
	// for almost a minute.
	require.Eventually(t, func() bool {
		_, err := cluster.Ping(ctx).Result()
		attempts, getErr := cluster.HGet(ctx, "otp:session:"+issued.SessionID, "attempts").Int()
		return err == nil && getErr == nil && attempts == 3
	}, 20*time.Second, 250*time.Millisecond, "the replica took over with the attempts made")
	t.Logf("the session's slot was served again after %v", time.Since(stopped).Round(time.Millisecond))
	failAttempts(t, store, issued, 2)
	_, err = store.Verify(ctx, issued.SessionID, issued.Code)
	require.ErrorIs(t, err, otp.ErrTooManyAttempts)
	t.Logf("the session was used up after %v", time.Since(stopped).Round(time.Millisecond))

	for i := range 60 {
		blacklisted, err := blacklist.IsTokenBlacklisted(ctx, fmt.Sprintf("token-%d", i))
		require.NoError(t, err)
		assert.True(t, blacklisted, "token-%d", i)
	}
	require.Eventually(t, func() bool {
		fresh, err := store.Issue(ctx, "cluster-after@example.com", "client")
		if err != nil {
			return false
		}
		_, err = store.Verify(ctx, fresh.SessionID, fresh.Code)
		return err == nil
	}, 30*time.Second, 500*time.Millisecond, "signing in works after the failover")
}
