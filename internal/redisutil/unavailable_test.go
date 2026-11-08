package redisutil

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/dberr"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsUnavailable(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	for err, want := range map[error]bool{
		nil:                               false,
		redis.Nil:                         false,
		context.Canceled:                  false,
		errors.New("ERR unknown command"): false,
		errors.New("WRONGTYPE Operation against a key holding the wrong kind of value"): false,
		ErrNotReplicated:               false, // busy, which durable writes note themselves
		refused:                        true,
		fmt.Errorf("get: %w", refused): true,
		context.DeadlineExceeded:       true,
		redis.ErrPoolTimeout:           true,
		redis.ErrClosed:                true,
		errors.New("redis: all sentinels specified in configuration are unreachable"): true,
	} {
		assert.Equal(t, want, IsUnavailable(err), "%v", err)
	}
}

// replyError is a reply of Redis, as go-redis returns it.
type replyError string

func (e replyError) Error() string { return string(e) }
func (replyError) RedisError()     {}

func TestIsUnavailable_RepliesOfARedisThatCannotServe(t *testing.T) {
	for _, reply := range []string{
		"LOADING Redis is loading the dataset in memory",
		"MASTERDOWN Link with MASTER is down and replica-serve-stale-data is set to 'no'.",
		"CLUSTERDOWN The cluster is down",
		"TRYAGAIN Multiple keys request during rehashing of slot",
		"READONLY You can't write against a read only replica.",
	} {
		assert.True(t, IsUnavailable(replyError(reply)), reply)
	}
	assert.False(t, IsUnavailable(replyError("ERR syntax error")))
}

// TestNoteUnavailable_MarksTheRequest runs commands against an address
// where nothing listens: a tracked request is noted with StoreUnavailable,
// by a single command and by a pipeline, and a command outside a request
// is left alone.
func TestNoteUnavailable_MarksTheRequest(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	defer client.Close()
	NoteUnavailable(client)

	ctx := dberr.Track(context.Background())
	_, ok := dberr.UnavailableOf(ctx)
	require.False(t, ok)
	require.Error(t, client.Get(ctx, "key").Err())
	answer, ok := dberr.UnavailableOf(ctx)
	require.True(t, ok, "a command that cannot reach Redis notes the request")
	assert.Equal(t, dberr.StoreUnavailable, answer)

	piped := dberr.Track(context.Background())
	_, err := client.Pipelined(piped, func(p redis.Pipeliner) error {
		p.Incr(piped, "a")
		p.Expire(piped, "a", 0)
		return nil
	})
	require.Error(t, err)
	_, ok = dberr.UnavailableOf(piped)
	assert.True(t, ok, "so does a pipeline")

	assert.Error(t, client.Get(context.Background(), "key").Err(), "the error is passed through outside a request")
}
