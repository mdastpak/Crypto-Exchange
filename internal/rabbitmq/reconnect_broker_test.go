package rabbitmq

import (
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/testutil/tcpproxy"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newProxiedService connects a service to the test broker through a TCP
// proxy, so that a test can take the broker away as a restart would.
func newProxiedService(t *testing.T) (*RabbitMQService, string, *tcpproxy.Proxy) {
	t.Helper()
	probe, err := amqp.Dial(brokerTestURL)
	if err != nil {
		t.Skipf("RabbitMQ is not reachable at localhost:5672: %v", err)
	}
	probe.Close()

	proxy, err := tcpproxy.New("localhost:5672")
	require.NoError(t, err)
	t.Cleanup(proxy.Close)
	t.Cleanup(withRabbitMQConfig(proxy.Host(), proxy.Port(), "guest", "guest"))

	exchange := "test_reconnect_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	svc, err := NewRabbitMQService(exchange)
	require.NoError(t, err)
	t.Cleanup(func() {
		svc.Close()
		withBrokerChannel(t, func(ch *amqp.Channel) {
			assert.NoError(t, ch.ExchangeDelete(exchange, false, false))
		})
	})
	return svc, exchange, proxy
}

// consumeThrough binds an exclusive queue to exchange on a channel opened by
// svc and returns its deliveries.
func consumeThrough(t *testing.T, svc *RabbitMQService, exchange string) <-chan amqp.Delivery {
	t.Helper()
	ch, err := svc.OpenChannel()
	require.NoError(t, err)
	t.Cleanup(func() { ch.Close() })
	queue, err := ch.QueueDeclare("", false, true, true, false, nil)
	require.NoError(t, err)
	require.NoError(t, ch.QueueBind(queue.Name, "", exchange, false, nil))
	deliveries, err := ch.Consume(queue.Name, "", true, true, false, false, nil)
	require.NoError(t, err)
	return deliveries
}

func TestReconnect_Broker_ResumesAfterABrokerRestart(t *testing.T) {
	svc, exchange, proxy := newProxiedService(t)
	require.NoError(t, publishWithTimeout(svc, exchange, `{"n": 1}`))
	require.True(t, svc.IsHealthy())

	// The broker goes away and refuses connections until it is back.
	proxy.SetDown(true)
	proxy.Cut()

	require.Eventually(t, func() bool { return !svc.IsHealthy() }, 5*time.Second, 10*time.Millisecond,
		"health must report the lost connection")
	assert.Never(t, svc.IsHealthy, 1500*time.Millisecond, 50*time.Millisecond,
		"health must stay degraded while the broker is away")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	assert.Error(t, svc.Publish(ctx, exchange, `{"n": 2}`), "publishing cannot succeed while the broker is away")

	proxy.SetDown(false)
	require.Eventually(t, svc.IsHealthy, 20*time.Second, 50*time.Millisecond,
		"the service must reconnect when the broker is back")

	deliveries := consumeThrough(t, svc, exchange)
	require.NoError(t, publishWithTimeout(svc, exchange, `{"n": 3}`))
	select {
	case msg := <-deliveries:
		assert.JSONEq(t, `{"n": 3}`, string(msg.Body), "publishing and consuming resume")
	case <-time.After(5 * time.Second):
		t.Fatal("the message published after the reconnect was not consumed")
	}
}

// TestReconnect_Broker_ReportsAndRestoresAClosedChannel keeps the channel
// closed on purpose: the service's exchange is redeclared with another type,
// so that declaring it again on a new channel fails until the test fixes it.
func TestReconnect_Broker_ReportsAndRestoresAClosedChannel(t *testing.T) {
	svc, exchange := newBrokerTestService(t)
	withBrokerChannel(t, func(ch *amqp.Channel) {
		require.NoError(t, ch.ExchangeDelete(exchange, false, false))
		require.NoError(t, ch.ExchangeDeclare(exchange, "direct", true, false, false, false, nil))
	})

	// Publishing to a missing exchange makes the broker close the channel.
	assert.Error(t, publishWithTimeout(svc, "missing_"+strings.ReplaceAll(uuid.NewString(), "-", ""), `{}`))

	require.Eventually(t, func() bool { return !svc.IsHealthy() }, 5*time.Second, 10*time.Millisecond,
		"health must report the closed channel")
	assert.Never(t, svc.IsHealthy, 1500*time.Millisecond, 50*time.Millisecond,
		"health must stay degraded while the channel cannot be restored")

	withBrokerChannel(t, func(ch *amqp.Channel) {
		require.NoError(t, ch.ExchangeDelete(exchange, false, false))
		require.NoError(t, ch.ExchangeDeclare(exchange, "fanout", true, false, false, false, nil))
	})
	require.Eventually(t, svc.IsHealthy, 20*time.Second, 50*time.Millisecond,
		"the channel must be restored without waiting for a publish")
	assert.NoError(t, publishWithTimeout(svc, exchange, `{"after": true}`))
}

// TestReconnect_Broker_ConcurrentUseDuringOutages publishes, checks health,
// and opens channels from several goroutines while the connection is cut
// repeatedly. Run with -race, it covers the reconnect paths for data races.
func TestReconnect_Broker_ConcurrentUseDuringOutages(t *testing.T) {
	svc, exchange, proxy := newProxiedService(t)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var published atomic.Int64
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				if svc.Publish(ctx, exchange, `{}`) == nil {
					published.Add(1)
				}
				cancel()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = svc.IsHealthy()
			if ch, err := svc.OpenChannel(); err == nil {
				ch.Close()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	for i := 0; i < 3; i++ {
		time.Sleep(400 * time.Millisecond)
		proxy.Cut()
	}
	require.Eventually(t, svc.IsHealthy, 20*time.Second, 50*time.Millisecond)
	close(stop)
	wg.Wait()

	assert.Positive(t, published.Load())
	assert.NoError(t, publishWithTimeout(svc, exchange, `{"final": true}`))
}

func TestReconnect_Broker_CloseStopsAReconnect(t *testing.T) {
	svc, _, proxy := newProxiedService(t)
	proxy.SetDown(true)
	proxy.Cut()
	require.Eventually(t, func() bool { return !svc.IsHealthy() }, 5*time.Second, 10*time.Millisecond)

	done := make(chan struct{})
	go func() {
		svc.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close must stop the reconnect loop")
	}
	assert.False(t, svc.IsHealthy())
}

// TestReconnect_Broker_RestoresAChannelClosedOutsidePublish closes the
// publishing channel without going through Publish, so only the supervisor's
// watch on the channel can notice it.
func TestReconnect_Broker_RestoresAChannelClosedOutsidePublish(t *testing.T) {
	svc, exchange := newBrokerTestService(t)
	svc.mu.RLock()
	ch := svc.channel
	svc.mu.RUnlock()

	// A publish to a missing exchange is a channel-level error.
	require.NoError(t, ch.Publish("missing_"+strings.ReplaceAll(uuid.NewString(), "-", ""), "", false, false, amqp.Publishing{Body: []byte(`{}`)}))

	require.Eventually(t, func() bool {
		svc.mu.RLock()
		defer svc.mu.RUnlock()
		return svc.channel != nil && svc.channel != ch
	}, 10*time.Second, 10*time.Millisecond, "the supervisor must replace the closed channel by itself")
	assert.True(t, svc.IsHealthy())
	assert.NoError(t, publishWithTimeout(svc, exchange, `{"after": true}`))
}

// TestStartup_Broker_ConnectsWhenTheBrokerComesBack starts the service while
// the broker is away (#232): it starts, unhealthy, and once the broker is
// back it connects, declares its exchange, and publishes.
func TestStartup_Broker_ConnectsWhenTheBrokerComesBack(t *testing.T) {
	probe, err := amqp.Dial(brokerTestURL)
	if err != nil {
		t.Skipf("RabbitMQ is not reachable at localhost:5672: %v", err)
	}
	probe.Close()
	proxy, err := tcpproxy.New("localhost:5672")
	require.NoError(t, err)
	t.Cleanup(proxy.Close)
	t.Cleanup(withRabbitMQConfig(proxy.Host(), proxy.Port(), "guest", "guest"))
	proxy.SetDown(true)

	exchange := "test_late_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	svc, err := NewRabbitMQService(exchange)
	require.NoError(t, err, "the service starts without the broker")
	t.Cleanup(func() {
		svc.Close()
		withBrokerChannel(t, func(ch *amqp.Channel) { _ = ch.ExchangeDelete(exchange, false, false) })
	})
	assert.False(t, svc.IsHealthy())
	assert.Error(t, svc.LastError())

	proxy.SetDown(false)
	require.Eventually(t, svc.IsHealthy, 20*time.Second, 50*time.Millisecond, "it connects once the broker is back")
	assert.NoError(t, svc.LastError())
	deliveries := consumeThrough(t, svc, exchange)
	require.NoError(t, publishWithTimeout(svc, exchange, `{"late": true}`), "the exchange was declared on connecting")
	select {
	case d := <-deliveries:
		assert.JSONEq(t, `{"late": true}`, string(d.Body))
	case <-time.After(5 * time.Second):
		t.Fatal("the message was not delivered")
	}
}

// TestHosts_Broker_SkipsANodeThatIsDown lists a node where nothing listens
// next to a working one (#232): the service connects to the working node,
// whatever the random order, also after a lost connection.
func TestHosts_Broker_SkipsANodeThatIsDown(t *testing.T) {
	probe, err := amqp.Dial(brokerTestURL)
	if err != nil {
		t.Skipf("RabbitMQ is not reachable at localhost:5672: %v", err)
	}
	probe.Close()
	proxy, err := tcpproxy.New("localhost:5672")
	require.NoError(t, err)
	t.Cleanup(proxy.Close)
	t.Cleanup(withRabbitMQConfig("", "", "guest", "guest"))
	config.Config.RabbitMQ.Hosts = []string{"127.0.0.2:1", proxy.Host() + ":" + proxy.Port()}

	for range 3 {
		svc, err := NewRabbitMQService()
		require.NoError(t, err)
		assert.True(t, svc.IsHealthy())
		assert.Equal(t, net.JoinHostPort(proxy.Host(), proxy.Port()), svc.ConnectedTo())
		require.NoError(t, svc.Close())
	}

	svc, err := NewRabbitMQService()
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	proxy.Cut()
	require.Eventually(t, func() bool { return svc.IsHealthy() && svc.LastError() == nil }, 20*time.Second, 50*time.Millisecond)
	assert.Equal(t, net.JoinHostPort(proxy.Host(), proxy.Port()), svc.ConnectedTo())
}
