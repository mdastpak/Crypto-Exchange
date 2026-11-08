package redisutil

import (
	"context"
	"errors"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/dberr"

	"github.com/redis/go-redis/v9"
)

// NoteUnavailable makes every command of c that cannot reach Redis note it
// on the request of its context (#130), as the database tracer does for a
// busy PostgreSQL: the response helpers then answer the request's 500 with
// 503 service_unavailable and Retry-After: 1. The handlers keep failing
// closed where they do, such as on a token whose revocation cannot be
// checked; only the status of the refusal changes.
//
// A hook covers every caller of the client at once, in every redis.mode,
// and a command run outside a request, whose context has no note, is left
// alone.
func NoteUnavailable(c redis.UniversalClient) {
	c.AddHook(unavailableHook{})
}

type unavailableHook struct{}

func (unavailableHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (unavailableHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		noteUnavailable(ctx, err)
		return err
	}
}

func (unavailableHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		noteUnavailable(ctx, err)
		for _, cmd := range cmds {
			noteUnavailable(ctx, cmd.Err())
		}
		return err
	}
}

func noteUnavailable(ctx context.Context, err error) {
	if IsUnavailable(err) {
		dberr.MarkUnavailable(ctx, dberr.StoreUnavailable)
	}
}

// unavailableReplies are the prefixes of the replies of a Redis that cannot
// serve for the moment, as during a failover or a restart.
var unavailableReplies = []string{"LOADING", "MASTERDOWN", "CLUSTERDOWN", "TRYAGAIN", "READONLY"}

// IsUnavailable reports whether err says that Redis could not serve the
// command: it could not be reached, dropped the connection, timed out, had
// no free connection in the pool, or answered that it cannot serve now. A
// missing key (redis.Nil), an error of the command itself, and a request
// that its client canceled are not.
func IsUnavailable(err error) bool {
	switch {
	case err == nil, errors.Is(err, redis.Nil), errors.Is(err, context.Canceled):
		return false
	case IsNetworkError(err), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, redis.ErrPoolTimeout), errors.Is(err, redis.ErrClosed):
		return true
	}
	var reply redis.Error
	if errors.As(err, &reply) {
		for _, prefix := range unavailableReplies {
			if strings.HasPrefix(reply.Error(), prefix) {
				return true
			}
		}
		return false
	}
	// The sentinel client reports that it could not find the primary with
	// errors of its own, which wrap no network error.
	return strings.Contains(err.Error(), "sentinel")
}
