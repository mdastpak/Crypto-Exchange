package redisutil

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"github.com/redis/go-redis/v9"
)

// ClusterStateReloadInterval bounds how long a cluster client keeps a map
// of the slots that no error has questioned. go-redis reloads the map at
// once after a MOVED reply, and otherwise only after this interval, whose
// default is a minute.
const ClusterStateReloadInterval = 5 * time.Second

// FollowClusterFailovers makes c reload its map of the slots after a
// network error (#231). When a primary dies, the cluster promotes its
// replica within cluster-node-timeout, but the dead primary answers no
// MOVED: the client keeps dialing it until its next periodic reload, which
// the failover drill measured at nearly a minute of failed sign-ins. A
// reload after the first refused dial finds the promoted replica as soon
// as the cluster has promoted it. go-redis runs at most one reload at a
// time, and coalesces the requests that come in meanwhile.
func FollowClusterFailovers(c *redis.ClusterClient) {
	c.AddHook(reloadHook{c})
}

type reloadHook struct{ c *redis.ClusterClient }

func (h reloadHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h reloadHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		reloadOnNetworkError(ctx, h.c, err)
		return err
	}
}

func (h reloadHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		reloadOnNetworkError(ctx, h.c, err)
		return err
	}
}

// reloadOnNetworkError reloads the slot map of client, if it is a cluster
// client and err says that a node could not be reached.
func reloadOnNetworkError(ctx context.Context, client redis.UniversalClient, err error) {
	cluster, ok := client.(*redis.ClusterClient)
	if ok && IsNetworkError(err) {
		cluster.ReloadState(ctx)
	}
}

// IsNetworkError reports whether err says that a node could not be reached
// or dropped the connection, rather than that it answered with an error.
func IsNetworkError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
