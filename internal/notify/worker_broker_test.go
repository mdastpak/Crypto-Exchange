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

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"
	"github.com/mdastpak/crypto-exchange/internal/testutil/tcpproxy"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
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

// TestWorker_Broker_AnotherInstanceTakesOverAtStop runs two workers on the
// queue, each with a connection of its own, as two server instances do. The
// first one stops while it waits to retry a delivery: the job goes back to
// the queue, and the second instance delivers it.
func TestWorker_Broker_AnotherInstanceTakesOverAtStop(t *testing.T) {
	logs := &syncBuffer{}
	topology := newBrokerTopology(t, logs)
	firstAttempt := make(chan struct{}, 1)
	failing := Channels{Email: deliverFunc(func(context.Context, OTPMessage) error {
		firstAttempt <- struct{}{}
		return errors.New("gateway timeout")
	})}
	first := NewWorker(topology.svc, failing, topology.opts)
	// Shorter than the lifetime of the code, so that the worker waits to
	// retry instead of giving up.
	first.retryBackoff = 30 * time.Second
	require.NoError(t, first.Start())
	sender := NewQueueSender(topology.svc, topology.opts.Exchange, failing, time.Minute)
	require.NoError(t, sender.SendOTP(context.Background(), testEmailMessage))
	select {
	case <-firstAttempt:
	case <-time.After(10 * time.Second):
		t.Fatal("the first instance did not try to deliver the code")
	}

	otherService, err := rabbitmq.NewRabbitMQService(topology.opts.Exchange)
	require.NoError(t, err)
	defer otherService.Close()
	delivered := make(chan OTPMessage, 1)
	second := NewWorker(otherService, Channels{Email: deliverFunc(func(ctx context.Context, msg OTPMessage) error {
		delivered <- msg
		return nil
	})}, topology.opts)
	require.NoError(t, second.Start())
	defer stopWorker(t, second)

	stopWorker(t, first)
	select {
	case msg := <-delivered:
		assert.Equal(t, testEmailMessage, msg)
	case <-time.After(10 * time.Second):
		t.Fatalf("the other instance did not deliver the job of the stopped one; worker log:\n%s", logs.String())
	}
	_, ok := getMessage(t, topology.opts.DeadLetterQueue, 0)
	assert.False(t, ok, "the job is handed over, not given up")
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

// TestWorker_Broker_DeclaresQuorumQueues checks the queue types of #232: a
// declaration of the job and dead-letter queues as classic queues is
// refused, because they exist as quorum queues.
func TestWorker_Broker_DeclaresQuorumQueues(t *testing.T) {
	topology := newBrokerTopology(t, &syncBuffer{})
	worker := NewWorker(topology.svc, emailOnlyChannels(), topology.opts)
	require.NoError(t, worker.Start())
	stopWorker(t, worker)

	for _, queue := range []string{topology.opts.Queue, topology.opts.DeadLetterQueue} {
		withBrokerChannel(t, func(ch *amqp.Channel) {
			_, err := ch.QueueDeclare(queue, true, false, false, false, amqp.Table{amqp.QueueTypeArg: amqp.QueueTypeClassic})
			var amqpErr *amqp.Error
			require.ErrorAs(t, err, &amqpErr, queue)
			assert.Equal(t, amqp.PreconditionFailed, amqpErr.Code, "%s is a quorum queue: %v", queue, err)
		})
	}
}

// TestWorker_Broker_DrainsTheLegacyQueue fills a classic queue as a worker
// of an earlier version left it, bound to the exchange (#232). The worker
// unbinds it, delivers its jobs, and deletes it once no other worker
// consumes it; new jobs go to the quorum queue only.
func TestWorker_Broker_DrainsTheLegacyQueue(t *testing.T) {
	saved := legacyPoll
	legacyPoll = 100 * time.Millisecond
	t.Cleanup(func() { legacyPoll = saved })
	logs := &syncBuffer{}
	topology := newBrokerTopology(t, logs)
	legacy := topology.opts.Queue + "_legacy"
	topology.opts.LegacyQueue = legacy

	// An instance of the earlier version: the classic queue, bound, with
	// two jobs in it and a consumer that does not take them yet.
	oldWorker, err := topology.svc.OpenChannel()
	require.NoError(t, err)
	_, err = oldWorker.QueueDeclare(legacy, true, false, false, false, nil)
	require.NoError(t, err)
	require.NoError(t, oldWorker.QueueBind(legacy, "", topology.opts.Exchange, false, nil))
	t.Cleanup(func() {
		withBrokerChannel(t, func(ch *amqp.Channel) { _, _ = ch.QueueDelete(legacy, false, false, false) })
	})
	channels := Channels{Email: deliverFunc(func(context.Context, OTPMessage) error { return nil })}
	sender := NewQueueSender(topology.svc, topology.opts.Exchange, channels, time.Minute)
	require.NoError(t, sender.SendOTP(context.Background(), testEmailMessage))
	require.NoError(t, sender.SendOTP(context.Background(), testEmailMessage))
	require.NoError(t, oldWorker.Qos(1, 0, false))
	held, err := oldWorker.Consume(legacy, "old-worker", false, false, false, false, nil)
	require.NoError(t, err)
	oldJob := <-held // the earlier version holds one job, unacknowledged

	var mu sync.Mutex
	var codes []string
	delivered := Channels{Email: deliverFunc(func(_ context.Context, msg OTPMessage) error {
		mu.Lock()
		defer mu.Unlock()
		codes = append(codes, msg.Code)
		return nil
	})}
	worker := NewWorker(topology.svc, delivered, topology.opts)
	require.NoError(t, worker.Start())
	defer stopWorker(t, worker)
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(codes)
	}

	require.Eventually(t, func() bool { return count() == 1 }, 10*time.Second, 20*time.Millisecond, "the job left in the legacy queue is delivered")
	require.NoError(t, sender.SendOTP(context.Background(), testEmailMessage))
	require.Eventually(t, func() bool { return count() == 2 }, 10*time.Second, 20*time.Millisecond, "a new job reaches the quorum queue")
	assert.Never(t, func() bool { return count() > 2 }, 300*time.Millisecond, 20*time.Millisecond, "the legacy queue is unbound: the new job is not copied to it")
	withBrokerChannel(t, func(ch *amqp.Channel) {
		q, err := ch.QueueDeclarePassive(legacy, true, false, false, false, nil)
		require.NoError(t, err, "the queue stays while the earlier version consumes it")
		assert.Equal(t, 1, q.Consumers)
	})

	// The earlier version stops: its job goes back, is delivered, and the
	// empty queue is deleted.
	require.NoError(t, oldJob.Nack(false, true))
	require.NoError(t, oldWorker.Close())
	require.Eventually(t, func() bool { return count() == 3 }, 10*time.Second, 20*time.Millisecond, "the job of the earlier version is delivered")
	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "deleted the drained classic queue "+legacy)
	}, 10*time.Second, 20*time.Millisecond)
	withBrokerChannel(t, func(ch *amqp.Channel) {
		_, err := ch.QueueDeclarePassive(legacy, true, false, false, false, nil)
		var amqpErr *amqp.Error
		require.ErrorAs(t, err, &amqpErr)
		assert.Equal(t, amqp.NotFound, amqpErr.Code)
	})
	assert.NotContains(t, logs.String(), testEmailMessage.Code)
}

// TestWorker_Broker_StartsWithoutTheBroker starts the worker of a service
// that cannot reach the broker (#232): Start succeeds, and once the broker
// is back, the worker declares its queues and delivers.
func TestWorker_Broker_StartsWithoutTheBroker(t *testing.T) {
	logs := &syncBuffer{}
	topology := newBrokerTopology(t, logs)
	proxy, err := tcpproxy.New("localhost:5672")
	require.NoError(t, err)
	t.Cleanup(proxy.Close)
	saved := config.Config.RabbitMQ
	t.Cleanup(func() { config.Config.RabbitMQ = saved })
	config.Config.RabbitMQ.Host, config.Config.RabbitMQ.Port = proxy.Host(), proxy.Port()
	proxy.SetDown(true)
	late, err := rabbitmq.NewRabbitMQService(topology.opts.Exchange)
	require.NoError(t, err)
	t.Cleanup(func() { late.Close() })

	delivered := make(chan OTPMessage, 1)
	channels := Channels{Email: deliverFunc(func(_ context.Context, msg OTPMessage) error {
		delivered <- msg
		return nil
	})}
	worker := NewWorker(late, channels, topology.opts)
	require.NoError(t, worker.Start(), "the worker starts without the broker")
	defer stopWorker(t, worker)
	assert.Contains(t, logs.String(), "RabbitMQ is not available")

	sender := NewQueueSender(late, topology.opts.Exchange, channels, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	assert.Error(t, sender.SendOTP(ctx, testEmailMessage), "no code can be queued while the broker is away")

	proxy.SetDown(false)
	require.Eventually(t, late.IsHealthy, 20*time.Second, 50*time.Millisecond)
	// The worker declares the queue once it consumes; until then the
	// exchange drops what is published.
	require.Eventually(t, func() bool {
		withBrokerChannel(t, func(ch *amqp.Channel) {
			_, err = ch.QueueDeclarePassive(topology.opts.Queue, true, false, false, false, nil)
		})
		return err == nil
	}, 30*time.Second, 100*time.Millisecond)
	require.NoError(t, sender.SendOTP(context.Background(), testEmailMessage))
	select {
	case msg := <-delivered:
		assert.Equal(t, testEmailMessage, msg)
	case <-time.After(10 * time.Second):
		t.Fatalf("the code was not delivered once the broker was back; log:\n%s", logs.String())
	}
}
