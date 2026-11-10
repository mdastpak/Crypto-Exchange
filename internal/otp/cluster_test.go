package otp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/dberr"
	"github.com/mdastpak/crypto-exchange/internal/redisutil"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slotRecorder is a go-redis hook that records, for every script run and
// every MULTI of the store, the keys it touched, so that a test can check
// that Redis Cluster could run it: Cluster refuses a script or a
// transaction whose keys are in several hash slots (CROSSSLOT).
type slotRecorder struct {
	mu     sync.Mutex
	groups [][]string
}

func (r *slotRecorder) add(keys []string) {
	if len(keys) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.groups = append(r.groups, keys)
}

func (r *slotRecorder) DialHook(next redis.DialHook) redis.DialHook { return next }

func (r *slotRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		r.add(keysOf(cmd))
		return next(ctx, cmd)
	}
}

func (r *slotRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		// A transaction runs on one node: all of its keys must share a
		// slot. go-redis wraps it in MULTI and EXEC.
		if len(cmds) > 0 && cmds[0].Name() == "multi" {
			var keys []string
			for _, cmd := range cmds {
				keys = append(keys, keysOf(cmd)...)
			}
			r.add(keys)
		} else {
			for _, cmd := range cmds {
				r.add(keysOf(cmd))
			}
		}
		return next(ctx, cmds)
	}
}

// keysOf returns the keys of the commands that the store sends: the KEYS
// of a script, and the first argument of a command on keys.
func keysOf(cmd redis.Cmder) []string {
	args := cmd.Args()
	switch cmd.Name() {
	case "eval", "evalsha", "eval_ro", "evalsha_ro":
		n := 0
		_, _ = fmt.Sscan(fmt.Sprint(args[2]), &n)
		keys := make([]string, 0, n)
		for _, k := range args[3 : 3+n] {
			keys = append(keys, fmt.Sprint(k))
		}
		return keys
	case "del", "exists", "mget":
		keys := make([]string, 0, len(args)-1)
		for _, k := range args[1:] {
			keys = append(keys, fmt.Sprint(k))
		}
		return keys
	case "multi", "exec", "script", "ping", "hello", "client", "select", "flushdb", "wait":
		return nil
	}
	if len(args) > 1 {
		return []string{fmt.Sprint(args[1])}
	}
	return nil
}

// TestStore_EveryScriptStaysInOneHashSlot runs every path of the store, for
// several contacts and clients and in a namespace, and checks that each
// script and each transaction touched keys of a single hash slot (#231),
// as Redis Cluster requires.
func TestStore_EveryScriptStaysInOneHashSlot(t *testing.T) {
	recorder := &slotRecorder{}
	client := newTestRedis(t)
	client.AddHook(recorder)
	ctx := context.Background()

	for _, namespace := range []string{"", "admin"} {
		store := NewStore(client, Settings{Namespace: namespace, AttemptsPerCode: 2, ContactAttemptsPerHour: 3})
		for i := range 5 {
			contact := fmt.Sprintf("user%d@example.com", i)
			client := fmt.Sprintf("10.0.0.%d", i)
			// A right code: the session ends, the budget resets, the
			// client becomes known.
			issued, err := store.Issue(ctx, contact, client)
			require.NoError(t, err)
			_, err = store.Verify(ctx, issued.SessionID, issued.Code)
			require.NoError(t, err)

			// Wrong codes until the session locks, then the contact's
			// budget runs out.
			issued, err = store.Issue(ctx, contact, client)
			require.NoError(t, err)
			for range 3 {
				_, _ = store.Verify(ctx, issued.SessionID, wrongCode(issued.Code))
			}
			issued, err = store.Issue(ctx, contact, client)
			require.NoError(t, err)
			_, _ = store.Verify(ctx, issued.SessionID, wrongCode(issued.Code))
			_, err = store.Verify(ctx, issued.SessionID, wrongCode(issued.Code))
			assert.True(t, errors.Is(err, ErrTooManyAttempts) || errors.Is(err, ErrInvalidCode), "%v", err)
		}
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	require.NotEmpty(t, recorder.groups)
	scripts := 0
	for _, keys := range recorder.groups {
		slot := redisutil.Slot(keys[0])
		for _, key := range keys[1:] {
			assert.Equal(t, slot, redisutil.Slot(key), "%v share no hash slot", keys)
		}
		if len(keys) > 1 {
			scripts++
		}
		for _, key := range keys {
			if strings.Contains(key, ":contact:") {
				assert.Contains(t, key, ":contact:{", "a key of a contact carries the contact's hash tag: %s", key)
			}
		}
	}
	assert.Positive(t, scripts, "the check saw the multi-key scripts and transactions")
}

// TestStore_DurableWritesFailClosed configures the store to require a
// replica for its counters, against a test server that has none (#231).
// Neither a code request nor a verification may then go on: a request is
// not sent, and a code is not checked, not even the right one, because the
// attempt it costs could be lost in a failover. Once the replica condition
// holds again, here by not waiting, the same session signs in.
func TestStore_DurableWritesFailClosed(t *testing.T) {
	store, client := newTestStore(t, Settings{})
	ctx := dberr.Track(context.Background())
	issued, err := store.Issue(ctx, "durable@example.com", "client")
	require.NoError(t, err)

	durable := NewStore(client, Settings{Durability: redisutil.Durability{Replicas: 1, Timeout: 50 * time.Millisecond}})
	_, err = durable.Issue(ctx, "durable@example.com", "client")
	require.ErrorIs(t, err, redisutil.ErrNotReplicated)

	_, err = durable.Verify(ctx, issued.SessionID, issued.Code)
	require.ErrorIs(t, err, redisutil.ErrNotReplicated, "the right code is not checked")
	assert.True(t, dberr.Busy(ctx), "the request is answered as busy, with Retry-After")

	// The refused verification still counted its attempt on the primary.
	attempts, err := client.HGet(ctx, store.sessionKey(issued.SessionID), fieldAttempts).Int()
	require.NoError(t, err)
	assert.Equal(t, 1, attempts)

	got, err := store.Verify(ctx, issued.SessionID, issued.Code)
	require.NoError(t, err)
	assert.Equal(t, "durable@example.com", got)
}
