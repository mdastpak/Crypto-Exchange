package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"encrypted-db/config"
	"encrypted-db/internal/rabbitmq"

	"github.com/google/uuid"
	"github.com/streadway/amqp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file need a real broker. They are skipped when none is
// reachable. See the README testing section for how to start one.
const brokerTestURL = "amqp://guest:guest@localhost:5672/"

// syncBuffer is a log destination that the worker goroutines and the test can
// use at the same time.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type brokerTopology struct {
	opts WorkerOptions
	svc  *rabbitmq.RabbitMQService
}

// newBrokerTopology connects a service to the test broker with fresh exchange
// and queue names, and removes them when the test ends.
func newBrokerTopology(t *testing.T, logs *syncBuffer) *brokerTopology {
	t.Helper()
	probe, err := amqp.Dial(brokerTestURL)
	if err != nil {
		t.Skipf("RabbitMQ is not reachable at localhost:5672: %v", err)
	}
	probe.Close()

	saved := config.Config.RabbitMQ
	config.Config.RabbitMQ.Host = "localhost"
	config.Config.RabbitMQ.Port = "5672"
	config.Config.RabbitMQ.Username = "guest"
	config.Config.RabbitMQ.Password = "guest"
	t.Cleanup(func() { config.Config.RabbitMQ = saved })

	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	opts := WorkerOptions{
		Exchange:           "test_notifications_" + id,
		Queue:              "test_otp_" + id,
		DeadLetterExchange: "test_dead_letter_" + id,
		DeadLetterQueue:    "test_otp_dead_letter_" + id,
		Concurrency:        2,
		MaxAttempts:        3,
		Logger:             log.New(logs, "", 0),
	}
	svc, err := rabbitmq.NewRabbitMQService(opts.Exchange)
	require.NoError(t, err)
	t.Cleanup(func() {
		svc.Close()
		withBrokerChannel(t, func(ch *amqp.Channel) {
			for _, q := range []string{opts.Queue, opts.DeadLetterQueue} {
				_, err := ch.QueueDelete(q, false, false, false)
				assert.NoError(t, err)
			}
			for _, ex := range []string{opts.Exchange, opts.DeadLetterExchange} {
				assert.NoError(t, ch.ExchangeDelete(ex, false, false))
			}
		})
	})
	return &brokerTopology{opts: opts, svc: svc}
}

func withBrokerChannel(t *testing.T, fn func(ch *amqp.Channel)) {
	t.Helper()
	conn, err := amqp.Dial(brokerTestURL)
	require.NoError(t, err)
	defer conn.Close()
	ch, err := conn.Channel()
	require.NoError(t, err)
	defer ch.Close()
	fn(ch)
}

// getMessage polls queue until it holds a message or the timeout passes.
func getMessage(t *testing.T, queue string, timeout time.Duration) (amqp.Delivery, bool) {
	t.Helper()
	var msg amqp.Delivery
	var ok bool
	deadline := time.Now().Add(timeout)
	withBrokerChannel(t, func(ch *amqp.Channel) {
		for {
			var err error
			msg, ok, err = ch.Get(queue, true)
			require.NoError(t, err)
			if ok || time.Now().After(deadline) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	return msg, ok
}

func stopWorker(t *testing.T, w *Worker) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, w.Stop(ctx))
}

func TestWorker_Broker_DeliversQueuedCodes(t *testing.T) {
	logs := &syncBuffer{}
	topology := newBrokerTopology(t, logs)
	delivered := make(chan OTPMessage, 1)
	channels := Channels{Email: deliverFunc(func(ctx context.Context, msg OTPMessage) error {
		delivered <- msg
		return nil
	})}
	worker := NewWorker(topology.svc, channels, topology.opts)
	require.NoError(t, worker.Start())
	defer stopWorker(t, worker)

	// Start declares the queue, so a code queued right away is not dropped.
	sender := NewQueueSender(topology.svc, topology.opts.Exchange, channels, time.Minute)
	require.NoError(t, sender.SendOTP(context.Background(), testEmailMessage))

	select {
	case msg := <-delivered:
		assert.Equal(t, testEmailMessage, msg)
	case <-time.After(10 * time.Second):
		t.Fatal("the queued code was not delivered")
	}
	assert.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "Delivered the sign-in code to email ali*****@example.com")
	}, 5*time.Second, 10*time.Millisecond)
	assert.NotContains(t, logs.String(), testEmailMessage.Code)
	assert.NotContains(t, logs.String(), testEmailMessage.Contact)
}

func TestWorker_Broker_RecordsUndeliveredCodesWithoutSecrets(t *testing.T) {
	logs := &syncBuffer{}
	topology := newBrokerTopology(t, logs)
	var calls sync.WaitGroup
	calls.Add(3)
	channels := Channels{SMS: deliverFunc(func(ctx context.Context, msg OTPMessage) error {
		calls.Done()
		return errors.New("gateway timeout while sending " + msg.Code + " to " + msg.Contact)
	})}
	worker := NewWorker(topology.svc, channels, topology.opts)
	worker.retryBackoff = 10 * time.Millisecond
	require.NoError(t, worker.Start())
	defer stopWorker(t, worker)

	sender := NewQueueSender(topology.svc, topology.opts.Exchange, channels, time.Minute)
	require.NoError(t, sender.SendOTP(context.Background(), testSMSMessage))

	msg, ok := getMessage(t, topology.opts.DeadLetterQueue, 10*time.Second)
	require.True(t, ok, "the undelivered code must be recorded on the dead-letter queue")
	calls.Wait()
	assert.NotContains(t, string(msg.Body), testSMSMessage.Code)
	assert.NotContains(t, string(msg.Body), testSMSMessage.Contact)
	var record deadLetter
	require.NoError(t, json.Unmarshal(msg.Body, &record))
	assert.Equal(t, reasonRetriesExhausted, record.Reason)
	assert.Equal(t, 3, record.Attempts)
	assert.Equal(t, testSMSMessage.MaskedContact, record.MaskedContact)
	assert.Contains(t, record.Error, "gateway timeout")

	_, ok = getMessage(t, topology.opts.Queue, 0)
	assert.False(t, ok, "the job is removed from the queue once it is given up")
	assert.NotContains(t, logs.String(), testSMSMessage.Code)
	assert.NotContains(t, logs.String(), testSMSMessage.Contact)
}

func TestWorker_Broker_DropsCodesThatExpireInTheQueue(t *testing.T) {
	logs := &syncBuffer{}
	topology := newBrokerTopology(t, logs)
	channels := Channels{Email: deliverFunc(func(context.Context, OTPMessage) error {
		t.Error("an expired code must not be delivered")
		return nil
	})}
	// Declare the queues without consuming from them.
	worker := NewWorker(topology.svc, channels, topology.opts)
	require.NoError(t, worker.Start())
	stopWorker(t, worker)

	sender := NewQueueSender(topology.svc, topology.opts.Exchange, channels, 100*time.Millisecond)
	require.NoError(t, sender.SendOTP(context.Background(), testEmailMessage))
	time.Sleep(300 * time.Millisecond)

	_, ok := getMessage(t, topology.opts.Queue, 0)
	assert.False(t, ok, "the broker drops a job once its code has expired")
}
