package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBroker records published dead-letter records. The unit tests call
// handle directly, so it never opens a channel.
type fakeBroker struct {
	recordingPublisher
}

func (b *fakeBroker) OpenChannel() (*amqp.Channel, error) {
	return nil, errors.New("the fake broker has no channels")
}

// fakeAcknowledger records how a delivery was settled.
type fakeAcknowledger struct {
	mu       sync.Mutex
	acks     int
	nacks    int
	requeued bool
}

func (a *fakeAcknowledger) Ack(tag uint64, multiple bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.acks++
	return nil
}

func (a *fakeAcknowledger) Nack(tag uint64, multiple, requeue bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nacks++
	a.requeued = requeue
	return nil
}

func (a *fakeAcknowledger) Reject(tag uint64, requeue bool) error {
	return a.Nack(tag, false, requeue)
}

func (a *fakeAcknowledger) settled() (acks, nacks int, requeued bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.acks, a.nacks, a.requeued
}

// countingDeliverer fails with the errors in failures, in order, and then
// succeeds.
type countingDeliverer struct {
	calls    atomic.Int32
	failures []error
}

func (d *countingDeliverer) Deliver(ctx context.Context, msg OTPMessage) error {
	call := int(d.calls.Add(1))
	if call <= len(d.failures) {
		return d.failures[call-1]
	}
	return nil
}

type workerTest struct {
	worker *Worker
	broker *fakeBroker
	logs   *bytes.Buffer
}

func newWorkerTest(t *testing.T, channels Channels) *workerTest {
	t.Helper()
	broker := &fakeBroker{}
	logs := &bytes.Buffer{}
	w := NewWorker(broker, channels, WorkerOptions{
		Exchange:    "notifications",
		MaxAttempts: 3,
		Logger:      log.New(logs, "", 0),
	})
	w.retryBackoff = time.Millisecond
	return &workerTest{worker: w, broker: broker, logs: logs}
}

func jobDelivery(t *testing.T, job otpJob) (amqp.Delivery, *fakeAcknowledger) {
	t.Helper()
	body, err := json.Marshal(job)
	require.NoError(t, err)
	ack := &fakeAcknowledger{}
	return amqp.Delivery{Acknowledger: ack, Body: body, Type: otpJobType}, ack
}

func testJob(expiresIn time.Duration) otpJob {
	return otpJob{
		Type:          otpJobType,
		ContactType:   ContactEmail,
		Contact:       "alice@example.com",
		MaskedContact: "ali*****@example.com",
		Code:          "482913",
		ExpiresAt:     time.Now().Add(expiresIn),
	}
}

// deadLetters decodes the records published to the dead-letter exchange.
func (wt *workerTest) deadLetters(t *testing.T) []deadLetter {
	t.Helper()
	var records []deadLetter
	for _, p := range wt.broker.messages() {
		assert.Equal(t, DefaultDeadLetterExchange, p.exchange)
		assert.NotContains(t, string(p.msg.Body), "482913", "dead letters must not contain the code")
		assert.NotContains(t, string(p.msg.Body), "alice@example.com", "dead letters must not contain the raw contact")
		var record deadLetter
		require.NoError(t, json.Unmarshal(p.msg.Body, &record))
		records = append(records, record)
	}
	return records
}

func (wt *workerTest) assertLogsAreRedacted(t *testing.T) {
	t.Helper()
	assert.NotContains(t, wt.logs.String(), "482913", "the code must not be logged")
	assert.NotContains(t, wt.logs.String(), "alice@example.com", "the raw contact must not be logged")
}

func TestWorker_DeliversAndAcknowledges(t *testing.T) {
	deliverer := &countingDeliverer{}
	wt := newWorkerTest(t, Channels{Email: deliverer})
	d, ack := jobDelivery(t, testJob(time.Minute))

	wt.worker.handle(d)

	assert.EqualValues(t, 1, deliverer.calls.Load())
	acks, nacks, _ := ack.settled()
	assert.Equal(t, 1, acks)
	assert.Zero(t, nacks)
	assert.Empty(t, wt.deadLetters(t))
	assert.Contains(t, wt.logs.String(), "Delivered the sign-in code to email ali*****@example.com (attempt 1)")
	wt.assertLogsAreRedacted(t)
}

func TestWorker_RetriesTemporaryFailures(t *testing.T) {
	temporary := errors.New("connection reset while sending to alice@example.com")
	deliverer := &countingDeliverer{failures: []error{temporary, temporary}}
	wt := newWorkerTest(t, Channels{Email: deliverer})
	d, ack := jobDelivery(t, testJob(time.Minute))

	wt.worker.handle(d)

	assert.EqualValues(t, 3, deliverer.calls.Load())
	acks, _, _ := ack.settled()
	assert.Equal(t, 1, acks)
	assert.Empty(t, wt.deadLetters(t))
	assert.Contains(t, wt.logs.String(), "Attempt 2 to deliver")
	assert.Contains(t, wt.logs.String(), "ali*****@example.com", "errors are logged with the masked contact")
	wt.assertLogsAreRedacted(t)
}

func TestWorker_GivesUp(t *testing.T) {
	cases := []struct {
		name         string
		failures     []error
		expiresIn    time.Duration
		wantCalls    int32
		wantReason   string
		wantAttempts int
	}{
		{
			name:         "after the last attempt",
			failures:     []error{errors.New("timeout"), errors.New("timeout"), errors.New("timeout")},
			expiresIn:    time.Minute,
			wantCalls:    3,
			wantReason:   reasonRetriesExhausted,
			wantAttempts: 3,
		},
		{
			name:         "on a permanent failure",
			failures:     []error{permanent(errors.New("550 unknown user alice@example.com"))},
			expiresIn:    time.Minute,
			wantCalls:    1,
			wantReason:   reasonRejected,
			wantAttempts: 1,
		},
		{
			name:         "when the code expired before delivery",
			expiresIn:    -time.Second,
			wantCalls:    0,
			wantReason:   reasonExpired,
			wantAttempts: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deliverer := &countingDeliverer{failures: tc.failures}
			wt := newWorkerTest(t, Channels{Email: deliverer})
			d, ack := jobDelivery(t, testJob(tc.expiresIn))

			wt.worker.handle(d)

			assert.Equal(t, tc.wantCalls, deliverer.calls.Load())
			acks, nacks, _ := ack.settled()
			assert.Equal(t, 1, acks, "a job that was given up is removed from the queue")
			assert.Zero(t, nacks)
			records := wt.deadLetters(t)
			require.Len(t, records, 1)
			assert.Equal(t, tc.wantReason, records[0].Reason)
			assert.Equal(t, tc.wantAttempts, records[0].Attempts)
			assert.Equal(t, ContactEmail, records[0].ContactType)
			assert.Equal(t, "ali*****@example.com", records[0].MaskedContact)
			assert.Contains(t, wt.logs.String(), "Gave up delivering the sign-in code")
			wt.assertLogsAreRedacted(t)
		})
	}
}

func TestWorker_DoesNotWaitPastTheExpiry(t *testing.T) {
	deliverer := &countingDeliverer{failures: []error{errors.New("timeout")}}
	wt := newWorkerTest(t, Channels{Email: deliverer})
	wt.worker.retryBackoff = time.Minute
	d, _ := jobDelivery(t, testJob(time.Second))

	start := time.Now()
	wt.worker.handle(d)

	assert.Less(t, time.Since(start), 5*time.Second, "a retry after the expiry is pointless")
	assert.EqualValues(t, 1, deliverer.calls.Load())
	records := wt.deadLetters(t)
	require.Len(t, records, 1)
	assert.Equal(t, reasonExpired, records[0].Reason)
}

func TestWorker_DeliveryContextEndsWithTheCode(t *testing.T) {
	var deadline time.Time
	deliverer := deliverFunc(func(ctx context.Context, msg OTPMessage) error {
		deadline, _ = ctx.Deadline()
		return nil
	})
	wt := newWorkerTest(t, Channels{Email: deliverer})
	job := testJob(time.Minute)
	d, _ := jobDelivery(t, job)

	wt.worker.handle(d)

	assert.WithinDuration(t, job.ExpiresAt, deadline, time.Millisecond)
}

func TestWorker_DeadLettersJobsItCannotDeliver(t *testing.T) {
	t.Run("malformed message", func(t *testing.T) {
		wt := newWorkerTest(t, Channels{Email: &countingDeliverer{}})
		ack := &fakeAcknowledger{}

		wt.worker.handle(amqp.Delivery{Acknowledger: ack, Body: []byte("not json")})

		acks, _, _ := ack.settled()
		assert.Equal(t, 1, acks)
		records := wt.deadLetters(t)
		require.Len(t, records, 1)
		assert.Equal(t, reasonMalformed, records[0].Reason)
	})

	t.Run("contact type without a channel", func(t *testing.T) {
		wt := newWorkerTest(t, Channels{Email: &countingDeliverer{}})
		job := testJob(time.Minute)
		job.ContactType = ContactMobile
		d, ack := jobDelivery(t, job)

		wt.worker.handle(d)

		acks, _, _ := ack.settled()
		assert.Equal(t, 1, acks)
		records := wt.deadLetters(t)
		require.Len(t, records, 1)
		assert.Equal(t, reasonNoChannel, records[0].Reason)
	})
}

func TestWorker_StopReturnsJobsToTheQueue(t *testing.T) {
	t.Run("while waiting for a retry", func(t *testing.T) {
		deliverer := &countingDeliverer{failures: []error{errors.New("timeout")}}
		wt := newWorkerTest(t, Channels{Email: deliverer})
		wt.worker.retryBackoff = 30 * time.Second
		d, ack := jobDelivery(t, testJob(time.Minute))

		handled := make(chan struct{})
		go func() {
			wt.worker.handle(d)
			close(handled)
		}()
		require.Eventually(t, func() bool { return deliverer.calls.Load() == 1 }, 5*time.Second, time.Millisecond)
		require.NoError(t, wt.worker.Stop(context.Background()))

		select {
		case <-handled:
		case <-time.After(5 * time.Second):
			t.Fatal("handle kept waiting for the retry after Stop")
		}
		acks, nacks, requeued := ack.settled()
		assert.Zero(t, acks)
		assert.Equal(t, 1, nacks)
		assert.True(t, requeued, "another worker delivers the job")
		assert.Empty(t, wt.deadLetters(t))
	})

	t.Run("before delivery starts", func(t *testing.T) {
		deliverer := &countingDeliverer{}
		wt := newWorkerTest(t, Channels{Email: deliverer})
		require.NoError(t, wt.worker.Stop(context.Background()))
		d, ack := jobDelivery(t, testJob(time.Minute))

		wt.worker.handle(d)

		assert.Zero(t, deliverer.calls.Load())
		_, nacks, requeued := ack.settled()
		assert.Equal(t, 1, nacks)
		assert.True(t, requeued)
	})
}
