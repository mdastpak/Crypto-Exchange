package rabbitmq

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/streadway/amqp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file need a real broker. They are skipped when none is
// reachable, so the unit tests still run without RabbitMQ. See the README
// testing section for how to start one.
const brokerTestURL = "amqp://guest:guest@localhost:5672/"

// newBrokerTestService connects a service to the test broker with a fresh
// fan-out exchange, and removes the exchange when the test ends.
func newBrokerTestService(t *testing.T) (*RabbitMQService, string) {
	t.Helper()
	probe, err := amqp.Dial(brokerTestURL)
	if err != nil {
		t.Skipf("RabbitMQ is not reachable at localhost:5672: %v", err)
	}
	probe.Close()

	t.Cleanup(withRabbitMQConfig("localhost", "5672", "guest", "guest"))

	exchange := "test_publish_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	svc, err := NewRabbitMQService(exchange)
	require.NoError(t, err)
	t.Cleanup(func() {
		svc.Close()
		withBrokerChannel(t, func(ch *amqp.Channel) {
			assert.NoError(t, ch.ExchangeDelete(exchange, false, false))
		})
	})
	return svc, exchange
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

// consumeExchange binds an exclusive queue to the exchange and returns its
// deliveries. The queue disappears when the test ends.
func consumeExchange(t *testing.T, exchange string) <-chan amqp.Delivery {
	t.Helper()
	conn, err := amqp.Dial(brokerTestURL)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	ch, err := conn.Channel()
	require.NoError(t, err)

	q, err := ch.QueueDeclare("", false, true, true, false, nil)
	require.NoError(t, err)
	require.NoError(t, ch.QueueBind(q.Name, "", exchange, false, nil))
	deliveries, err := ch.Consume(q.Name, "", true, true, false, false, nil)
	require.NoError(t, err)
	return deliveries
}

func publishWithTimeout(svc *RabbitMQService, exchange, message string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return svc.Publish(ctx, exchange, message)
}

func TestPublish_Broker_SequentialPublishesAreAllConfirmed(t *testing.T) {
	svc, exchange := newBrokerTestService(t)
	require.NoError(t, publishWithTimeout(svc, exchange, `{"n": 0}`))
	goroutinesAfterFirstPublish := runtime.NumGoroutine()

	for i := 1; i < 1000; i++ {
		require.NoError(t, publishWithTimeout(svc, exchange, fmt.Sprintf(`{"n": %d}`, i)), "publish %d", i)
	}

	assert.LessOrEqual(t, runtime.NumGoroutine(), goroutinesAfterFirstPublish+2,
		"publishing must not leave goroutines or listeners behind")
	svc.confirms.mu.Lock()
	pending := len(svc.confirms.waiters)
	svc.confirms.mu.Unlock()
	assert.Zero(t, pending, "every confirmed publish must be removed from the tracker")
}

func TestPublish_Broker_ConcurrentPublishesAreAllConfirmed(t *testing.T) {
	svc, exchange := newBrokerTestService(t)
	const publishers, perPublisher = 20, 50

	errs := make(chan error, publishers*perPublisher)
	var wg sync.WaitGroup
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perPublisher; i++ {
				errs <- publishWithTimeout(svc, exchange, fmt.Sprintf(`{"publisher": %d, "n": %d}`, p, i))
			}
		}(p)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

func TestPublish_Broker_EveryMessageIsDeliveredOnce(t *testing.T) {
	svc, exchange := newBrokerTestService(t)
	deliveries := consumeExchange(t, exchange)
	const publishers, perPublisher = 10, 30

	var wg sync.WaitGroup
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perPublisher; i++ {
				assert.NoError(t, publishWithTimeout(svc, exchange, fmt.Sprintf(`{"id": "%d-%d"}`, p, i)))
			}
		}(p)
	}
	wg.Wait()

	received := map[string]int{}
	deadline := time.After(15 * time.Second)
	for len(received) < publishers*perPublisher {
		select {
		case d := <-deliveries:
			received[string(d.Body)]++
		case <-deadline:
			t.Fatalf("received %d of %d messages", len(received), publishers*perPublisher)
		}
	}
	// Give duplicates a moment to show up before checking for them.
	quiet := time.After(500 * time.Millisecond)
drain:
	for {
		select {
		case d := <-deliveries:
			received[string(d.Body)]++
		case <-quiet:
			break drain
		}
	}
	for body, count := range received {
		assert.Equal(t, 1, count, "message %s delivered %d times", body, count)
	}
}

func TestPublish_Broker_RecoversAfterChannelError(t *testing.T) {
	svc, exchange := newBrokerTestService(t)
	require.NoError(t, publishWithTimeout(svc, exchange, `{"before": true}`))

	// The broker closes the channel when a message targets a missing exchange.
	missing := "missing_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.Error(t, publishWithTimeout(svc, missing, `{}`))

	require.NoError(t, publishWithTimeout(svc, exchange, `{"after": true}`), "the next publish must use a fresh channel")
}

func TestPublish_Broker_RecoversAfterConnectionLoss(t *testing.T) {
	svc, exchange := newBrokerTestService(t)
	require.NoError(t, publishWithTimeout(svc, exchange, `{"before": true}`))

	svc.mu.RLock()
	conn := svc.conn
	svc.mu.RUnlock()
	require.NoError(t, conn.Close())

	assert.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return svc.Publish(ctx, exchange, `{"after": true}`) == nil
	}, 20*time.Second, 200*time.Millisecond, "publishing must resume after the connection is re-established")
}
