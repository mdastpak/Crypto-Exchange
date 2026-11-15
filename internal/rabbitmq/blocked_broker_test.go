package rabbitmq

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBlocked_Broker_AWriteThatTheBrokerDoesNotReadHoldsNothing reproduces
// a broker under a memory or disk alarm (#402): it keeps the connection but
// stops reading what the client sends, so that, once the socket buffers are
// full, a publish blocks in its socket write. That write must not hold the
// health check, the other publishes, or Close.
func TestBlocked_Broker_AWriteThatTheBrokerDoesNotReadHoldsNothing(t *testing.T) {
	svc, exchange, proxy := newProxiedService(t)
	require.NoError(t, publishWithTimeout(svc, exchange, `{"before": true}`))

	proxy.SetPaused(true)
	body := `"` + strings.Repeat("x", 1<<20) + `"`
	const publishCtx = 300 * time.Millisecond
	const margin = 500 * time.Millisecond
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var worst atomic.Int64
	var published atomic.Int64
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ctx, cancel := context.WithTimeout(context.Background(), publishCtx)
				start := time.Now()
				if svc.Publish(ctx, exchange, body) == nil {
					published.Add(1)
				}
				cancel()
				if d := int64(time.Since(start)); d > worst.Load() {
					worst.Store(d)
				}
			}
		}()
	}

	// The scenario holds: a write waits for the broker, with the slot of the
	// publishing channel taken.
	svc.mu.RLock()
	tracker := svc.confirms
	svc.mu.RUnlock()
	require.NotNil(t, tracker)
	require.Eventually(t, func() bool {
		select {
		case tracker.slot <- struct{}{}:
			<-tracker.slot
			return false
		default:
			return true
		}
	}, 10*time.Second, 50*time.Millisecond, "the proxy must block a socket write")
	time.Sleep(time.Second)
	held := published.Load()

	start := time.Now()
	_ = svc.IsHealthy()
	assert.Less(t, time.Since(start), 50*time.Millisecond, "the health check does not wait for the write")
	_ = svc.ConnectedTo()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	start = time.Now()
	err := svc.Publish(ctx, exchange, `{"during": true}`)
	cancel()
	assert.Error(t, err)
	assert.Less(t, time.Since(start), 200*time.Millisecond+margin, "a publish ends with its context")

	close(stop)
	wg.Wait()
	assert.Less(t, time.Duration(worst.Load()), publishCtx+margin, "every publish ended with its context")
	assert.Equal(t, held, published.Load(), "nothing is confirmed while the broker does not read")

	done := make(chan struct{})
	start = time.Now()
	go func() {
		svc.Close()
		close(done)
	}()
	select {
	case <-done:
		t.Logf("Close returned after %v", time.Since(start))
	case <-time.After(closeTimeout + 3*time.Second):
		t.Fatal("Close must be bounded while the broker does not read")
	}
}

// TestBlocked_Broker_ABlockedConnectionIsDegradedAndFailsFast follows the
// connection.blocked notifications of the broker (#402): while blocked, the
// service reports itself unhealthy and a publish fails at once, and both
// recover on connection.unblocked. The notifications are fed by the test,
// since raising a real alarm would block every test that shares the broker.
func TestBlocked_Broker_ABlockedConnectionIsDegradedAndFailsFast(t *testing.T) {
	svc, exchange := newBrokerTestService(t)
	svc.mu.RLock()
	conn := svc.conn
	svc.mu.RUnlock()
	blocks := make(chan amqp.Blocking)
	go svc.watchBlocked(conn, blocks)
	t.Cleanup(func() { close(blocks) })

	blocks <- amqp.Blocking{Active: true, Reason: "low on memory"}
	require.Eventually(t, func() bool { return !svc.IsHealthy() }, time.Second, 5*time.Millisecond)
	start := time.Now()
	assert.ErrorIs(t, publishWithTimeout(svc, exchange, `{}`), errBrokerBlocked)
	assert.Less(t, time.Since(start), 50*time.Millisecond, "a publish fails at once")

	blocks <- amqp.Blocking{Active: false}
	require.Eventually(t, svc.IsHealthy, time.Second, 5*time.Millisecond)
	assert.NoError(t, publishWithTimeout(svc, exchange, `{"after": true}`))

	// A notification of a connection that was replaced is ignored.
	stale := make(chan amqp.Blocking, 1)
	go svc.watchBlocked(&amqp.Connection{}, stale)
	stale <- amqp.Blocking{Active: true}
	close(stale)
	assert.Never(t, func() bool { return !svc.IsHealthy() }, 200*time.Millisecond, 10*time.Millisecond)
}
