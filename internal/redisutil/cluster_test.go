package redisutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
)

func TestIsNetworkError(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	for err, want := range map[error]bool{
		nil:                              false,
		refused:                          true,
		fmt.Errorf("count: %w", refused): true,
		io.EOF:                           true,
		fmt.Errorf("read: %w", io.ErrUnexpectedEOF): true,
		redis.Nil:                         false,
		errors.New("ERR unknown command"): false,
		context.Canceled:                  false,
		ErrNotReplicated:                  false,
	} {
		assert.Equal(t, want, IsNetworkError(err), "%v", err)
	}
}

// TestFollowClusterFailovers checks that the hook passes the commands and
// their errors through unchanged; the failover drill checks that it finds
// a promoted replica.
func TestFollowClusterFailovers(t *testing.T) {
	cluster := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{"127.0.0.1:1"}, MaxRetries: -1})
	defer cluster.Close()
	FollowClusterFailovers(cluster)
	ctx := context.Background()
	err := cluster.Get(ctx, "k").Err()
	assert.True(t, IsNetworkError(err), "a cluster that cannot be reached: %v", err)
	_, err = cluster.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.Get(ctx, "k")
		return nil
	})
	assert.Error(t, err)
	reloadOnNetworkError(ctx, redis.NewClient(&redis.Options{}), io.EOF) // not a cluster: nothing to reload
}
