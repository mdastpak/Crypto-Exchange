package notify

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/testutil/redistest"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memoryLog is a DeliveryLog in memory, which fails while err is set.
type memoryLog struct {
	mu   sync.Mutex
	ids  map[string]time.Duration
	err  error
	gets int
}

func (l *memoryLog) Delivered(_ context.Context, id string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gets++
	if l.err != nil {
		return false, l.err
	}
	_, ok := l.ids[id]
	return ok, nil
}

func (l *memoryLog) MarkDelivered(_ context.Context, id string, ttl time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	if l.ids == nil {
		l.ids = map[string]time.Duration{}
	}
	l.ids[id] = ttl
	return nil
}

// TestWorker_DoesNotSendARedeliveredJobTwice hands the worker the same job
// twice, as the broker does when a worker stopped after sending a code but
// before acknowledging it (#232): the code is sent once, and both copies
// are acknowledged. A job of an earlier version, without an ID, is
// recognized by its body.
func TestWorker_DoesNotSendARedeliveredJobTwice(t *testing.T) {
	for name, id := range map[string]string{"with an ID": "job-1", "of an earlier version": ""} {
		t.Run(name, func(t *testing.T) {
			deliverer := &countingDeliverer{}
			wt := newWorkerTest(t, Channels{Email: deliverer})
			deliveries := &memoryLog{}
			wt.worker.delivered = deliveries

			job := testJob(time.Minute)
			job.ID = id
			first, firstAck := jobDelivery(t, job)
			wt.worker.handle(first)
			again, againAck := jobDelivery(t, job)
			again.Redelivered = true
			wt.worker.handle(again)

			assert.Equal(t, int32(1), deliverer.calls.Load(), "the code is sent once")
			acks, _, _ := firstAck.settled()
			assert.Equal(t, 1, acks)
			acks, _, _ = againAck.settled()
			assert.Equal(t, 1, acks, "the copy is acknowledged, not requeued")
			assert.Contains(t, wt.logs.String(), "already delivered (redelivered: true)")
			wt.assertLogsAreRedacted(t)

			require.Len(t, deliveries.ids, 1)
			for key, ttl := range deliveries.ids {
				if id != "" {
					assert.Equal(t, id, key)
				} else {
					assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, key)
				}
				assert.InDelta(t, (2 * time.Minute).Seconds(), ttl.Seconds(), 2, "kept until a minute after the code expires")
			}

			// Another job, with the same contact and code but another ID,
			// is delivered.
			other := testJob(time.Minute)
			other.ID = "job-2"
			d, _ := jobDelivery(t, other)
			wt.worker.handle(d)
			assert.Equal(t, int32(2), deliverer.calls.Load())
		})
	}
}

// TestWorker_DeliversWhenTheLogFails checks that a delivery log that cannot
// be read does not stop delivery: a code sent twice is better than none.
func TestWorker_DeliversWhenTheLogFails(t *testing.T) {
	deliverer := &countingDeliverer{}
	wt := newWorkerTest(t, Channels{Email: deliverer})
	wt.worker.delivered = &memoryLog{err: errors.New("redis is down")}
	d, ack := jobDelivery(t, testJob(time.Minute))
	wt.worker.handle(d)
	assert.Equal(t, int32(1), deliverer.calls.Load())
	acks, _, _ := ack.settled()
	assert.Equal(t, 1, acks)
	assert.Contains(t, wt.logs.String(), "Could not check whether the sign-in code for email ali*****@example.com was delivered, delivering it: redis is down")
	assert.Contains(t, wt.logs.String(), "Could not record the delivery")
}

// TestWorker_DoesNotLogAFailedDelivery checks that only a delivered code is
// recorded, so that a job that failed is tried again when it comes back.
func TestWorker_DoesNotLogAFailedDelivery(t *testing.T) {
	wt := newWorkerTest(t, Channels{Email: &countingDeliverer{failures: []error{permanent(errors.New("rejected"))}}})
	deliveries := &memoryLog{}
	wt.worker.delivered = deliveries
	d, _ := jobDelivery(t, testJob(time.Minute))
	wt.worker.handle(d)
	assert.Empty(t, deliveries.ids)
}

// TestRedisDeliveryLog checks the log in Redis, which every instance
// shares: a job marked by one instance is delivered on all of them, the key
// expires, and it holds neither the code nor the contact.
func TestRedisDeliveryLog(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: redistest.Addr, DB: redistest.Notify})
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		t.Skipf("Redis is not reachable at %s: %v", redistest.Addr, err)
	}
	require.NoError(t, client.FlushDB(ctx).Err())
	t.Cleanup(func() {
		client.FlushDB(context.Background())
		client.Close()
	})

	one, other := NewRedisDeliveryLog(client), NewRedisDeliveryLog(client)
	delivered, err := other.Delivered(ctx, "job-1")
	require.NoError(t, err)
	assert.False(t, delivered)
	require.NoError(t, one.MarkDelivered(ctx, "job-1", time.Minute))
	delivered, err = other.Delivered(ctx, "job-1")
	require.NoError(t, err)
	assert.True(t, delivered, "the instances share the log")

	ttl := client.TTL(ctx, "notify:otp:delivered:job-1").Val()
	assert.InDelta(t, time.Minute.Seconds(), ttl.Seconds(), 2)
	keys := client.Keys(ctx, "*").Val()
	encoded, _ := json.Marshal(keys)
	assert.NotContains(t, string(encoded), "482913")
	assert.NotContains(t, string(encoded), "alice@example.com")

	failing := NewRedisDeliveryLog(redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1}))
	_, err = failing.Delivered(ctx, "job-1")
	assert.Error(t, err)
}
