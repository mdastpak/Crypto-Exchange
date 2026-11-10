package main

import (
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheckRedisEviction_UnreadablePolicy checks the start-up check of #305
// where Redis refuses to tell its policy: release mode stops and names the
// override, the override starts, and outside release mode it is logged.
func TestCheckRedisEviction_UnreadablePolicy(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { client.Close() })

	_, err := checkRedisEviction(client, true, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "REDIS_ASSUME_NOEVICTION")

	watch, err := checkRedisEviction(client, true, true)
	require.NoError(t, err, "the override starts the server")
	assert.Nil(t, watch, "no count can be read, so readiness does not watch")

	_, err = checkRedisEviction(client, false, false)
	assert.NoError(t, err, "outside release mode the problem is only logged")
}
