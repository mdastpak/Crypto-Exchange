package rabbitmq

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"

	amqp "github.com/rabbitmq/amqp091-go"
)

// RabbitMQService publishes with publisher confirms and opens channels for
// consumers. One supervisor goroutine owns reconnection: it watches the
// connection and the publishing channel, and replaces whichever closes, so
// that a broker restart or a channel-level error is recovered without the
// callers noticing more than a short delay.
type RabbitMQService struct {
	// nodes are the brokers that the service may dial: the nodes of
	// rabbitmq.hosts, or the one of rabbitmq.host (#232).
	nodes     []node
	exchanges []string
	// connectedTo is the address of the node of the current connection, for
	// the logs and the tests.
	connectedTo atomic.Value
	// lastErr holds the error of the last failed connection attempt, until
	// one succeeds, for readiness and the tests.
	lastErr atomic.Pointer[error]

	// mu guards conn, channel, confirms, and ready. Only the supervisor
	// replaces the connection and restores the publishing channel; a publish
	// can only drop a channel that failed.
	mu      sync.RWMutex
	conn    *amqp.Connection
	channel *amqp.Channel
	// confirms routes the publisher confirmations of channel to the publishes
	// waiting for them. It owns the channel's only confirmation listener: the
	// library delivers every confirmation to every listener with a blocking
	// send, so a listener per publish stalls the connection after a few
	// messages.
	confirms *confirmTracker
	// ready is closed while a publishing channel is available, and replaced
	// by an open channel when it is dropped, so that publishes can wait for
	// the supervisor to restore it.
	ready chan struct{}

	closed   atomic.Bool
	stop     chan struct{}
	stopOnce sync.Once
	// wake asks the supervisor to replace a publishing channel that a publish
	// dropped.
	wake chan struct{}
	wg   sync.WaitGroup
}

const (
	reconnectInitialBackoff = time.Second
	reconnectMaxBackoff     = 30 * time.Second
	// dialTimeout bounds a connection attempt, so that Close does not wait
	// for a long dial.
	dialTimeout = 5 * time.Second
	// channelWait bounds how long a publish waits while the supervisor
	// restores the publishing channel.
	channelWait = 5 * time.Second
)

var (
	errServiceClosed      = errors.New("rabbitmq service closed")
	errChannelUnavailable = errors.New("rabbitmq channel is not available")
)

// node is a broker that the service may dial.
type node struct {
	// address is the node's host:port, for the logs.
	address string
	// url holds the password; it is never logged.
	url string
	// tls configures the connection when rabbitmq.tls is enabled, with the
	// node's own host as the server name unless rabbitmq.tls.server_name
	// sets one; it is nil without TLS.
	tls *tls.Config
}

// confirmBufferSize lets the broker run ahead of the dispatcher goroutine
// without blocking the connection's reader.
const confirmBufferSize = 256

const publishConfirmTimeout = 5 * time.Second

var errChannelClosed = errors.New("rabbitmq channel closed before the publish was confirmed")

// confirmTracker matches the confirmations of one channel in confirm mode to
// the publishes waiting for them, by delivery tag. Delivery tags start at 1 on
// every channel and follow the order of publishes on it.
type confirmTracker struct {
	mu      sync.Mutex
	lastTag uint64
	waiters map[uint64]chan bool // delivery tag -> ack (true) or nack (false)
	closed  bool
}

func newConfirmTracker(ch *amqp.Channel) *confirmTracker {
	t := &confirmTracker{waiters: make(map[uint64]chan bool)}
	go t.dispatch(ch.NotifyPublish(make(chan amqp.Confirmation, confirmBufferSize)))
	return t
}

// dispatch runs until the channel closes. The library resequences multiple
// acknowledgements, so every delivery tag is confirmed individually and in order.
func (t *confirmTracker) dispatch(confirms <-chan amqp.Confirmation) {
	for c := range confirms {
		t.mu.Lock()
		if w, ok := t.waiters[c.DeliveryTag]; ok {
			w <- c.Ack
			delete(t.waiters, c.DeliveryTag)
		}
		t.mu.Unlock()
	}
	// The listener is closed with the channel, so no outstanding publish will
	// be confirmed anymore.
	t.mu.Lock()
	t.closed = true
	for tag, w := range t.waiters {
		close(w)
		delete(t.waiters, tag)
	}
	t.mu.Unlock()
}

// register reserves the delivery tag of the next publish on the channel. The
// caller must publish right after, without another publish in between.
func (t *confirmTracker) register() (uint64, <-chan bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return 0, nil, errChannelClosed
	}
	t.lastTag++
	w := make(chan bool, 1)
	t.waiters[t.lastTag] = w
	return t.lastTag, w, nil
}

// forget stops tracking a publish that will not wait for its confirmation.
func (t *confirmTracker) forget(tag uint64) {
	t.mu.Lock()
	delete(t.waiters, tag)
	t.mu.Unlock()
}

// NewRabbitMQService connects to the RabbitMQ of cfg and declares the exchanges. When
// no node can be reached, it does not fail (#232): it logs the error and
// keeps connecting in the background, so that a server started during a
// broker outage serves every route that does not need the broker, and a
// rolling deploy does not crash-loop. Until the connection is up,
// IsHealthy is false, publishes fail after a short wait, and OpenChannel
// fails; the consumers retry on their own. It fails only on settings that
// cannot work, such as a TLS file that does not load.
func NewRabbitMQService(cfg *config.Configuration, exchanges ...string) (*RabbitMQService, error) {
	var nodes []node
	for _, n := range cfg.RabbitMQNodes() {
		tlsConfig, err := cfg.RabbitMQ.TLS.TLSConfig(n.Host)
		if err != nil {
			return nil, fmt.Errorf("rabbitmq.tls: %w", err)
		}
		nodes = append(nodes, node{address: n.Address, url: n.URL, tls: tlsConfig})
	}
	if len(nodes) == 0 {
		return nil, errors.New("no RabbitMQ node is configured")
	}
	r := &RabbitMQService{
		nodes:     nodes,
		exchanges: exchanges,
		ready:     make(chan struct{}),
		stop:      make(chan struct{}),
		wake:      make(chan struct{}, 1),
	}

	conn, ch, err := r.open()
	if err != nil {
		log.Printf("WARNING: RabbitMQ cannot be reached (%v); starting without it and connecting in the background", err)
	} else if !r.install(conn, ch) {
		return nil, errServiceClosed
	} else {
		log.Printf("RabbitMQ connected to %s, with publisher confirms", r.ConnectedTo())
	}

	r.wg.Add(1)
	go r.supervise(conn, ch)
	return r, nil
}

// ConnectedTo returns the address (host:port) of the node of the current
// connection, or "" before the first connection.
func (r *RabbitMQService) ConnectedTo() string {
	address, _ := r.connectedTo.Load().(string)
	return address
}

// open dials the nodes in random order, so that the instances spread over
// the cluster and a node that is down delays a connection by one dial at
// most, and opens the publishing channel on the first node that answers.
// It returns the error of every node when none does.
func (r *RabbitMQService) open() (*amqp.Connection, *amqp.Channel, error) {
	var errs []error
	// The order only spreads the load; it need not be unpredictable.
	for _, i := range rand.Perm(len(r.nodes)) { //nolint:gosec // G404: not a security decision
		n := r.nodes[i]
		conn, ch, err := r.openNode(n)
		if err == nil {
			r.connectedTo.Store(n.address)
			r.lastErr.Store(nil)
			return conn, ch, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", n.address, err))
	}
	err := errors.Join(errs...)
	r.lastErr.Store(&err)
	return nil, nil, err
}

// LastError returns the error of the last connection attempt when it
// failed, and nil once one succeeds.
func (r *RabbitMQService) LastError() error {
	if err := r.lastErr.Load(); err != nil {
		return *err
	}
	return nil
}

// openNode dials one node and opens the publishing channel.
func (r *RabbitMQService) openNode(n node) (*amqp.Connection, *amqp.Channel, error) {
	conn, err := amqp.DialConfig(n.url, amqp.Config{
		Heartbeat: 10 * time.Second,
		Locale:    "en_US",
		Dial:      amqp.DefaultDial(dialTimeout),
		// With an amqps URL, the client dials with this configuration
		// (#186). It is nil without TLS.
		TLSClientConfig: n.tls,
	})
	if err != nil {
		return nil, nil, err
	}
	ch, err := r.openChannel(conn)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, ch, nil
}

// openChannel opens a publishing channel in confirm mode on conn and declares
// the exchanges, which are missing after the broker lost its state.
func (r *RabbitMQService) openChannel(conn *amqp.Connection) (*amqp.Channel, error) {
	ch, err := conn.Channel()
	if err != nil {
		return nil, err
	}
	if err := ch.Confirm(false); err != nil {
		ch.Close()
		return nil, fmt.Errorf("failed to enable publisher confirms: %w", err)
	}
	for _, exchange := range r.exchanges {
		if err := declareExchange(ch, exchange); err != nil {
			ch.Close()
			return nil, fmt.Errorf("failed to declare exchange %s: %w", exchange, err)
		}
	}
	return ch, nil
}

func declareExchange(ch *amqp.Channel, exchangeName string) error {
	return ch.ExchangeDeclare(
		exchangeName,
		"fanout",
		true,  // durable
		false, // auto-deleted
		false, // internal
		false, // no-wait
		nil,
	)
}

// install makes conn and ch current and closes the connection they replace.
// It refuses them once the service is closed, so that a reconnect racing with
// Close does not leak a connection.
func (r *RabbitMQService) install(conn *amqp.Connection, ch *amqp.Channel) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed.Load() {
		conn.Close()
		return false
	}
	if r.conn != nil && r.conn != conn {
		r.conn.Close()
	}
	r.conn = conn
	r.setChannelLocked(ch)
	return true
}

// setChannelLocked makes ch the publishing channel, or marks the channel
// unavailable when ch is nil. ready is closed exactly while a channel is
// available. r.mu must be held.
func (r *RabbitMQService) setChannelLocked(ch *amqp.Channel) {
	if ch == nil {
		if r.channel != nil {
			r.ready = make(chan struct{})
		}
		r.channel, r.confirms = nil, nil
		return
	}
	if r.channel == nil && r.ready != nil {
		close(r.ready)
	}
	r.channel = ch
	r.confirms = newConfirmTracker(ch)
}

// dropChannel marks ch unavailable, if it is still the publishing channel,
// and asks the supervisor to replace it.
func (r *RabbitMQService) dropChannel(ch *amqp.Channel) {
	r.mu.Lock()
	if r.channel == ch {
		r.setChannelLocked(nil)
	}
	r.mu.Unlock()
	r.requestChannel()
}

func (r *RabbitMQService) requestChannel() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// supervise watches the connection and the publishing channel and replaces
// whichever closes, until the service is closed. The close notifications are
// local to this goroutine, so no other goroutine reads them.
//
// Without a connection, which happens when the service started while no
// node could be reached, it connects first.
func (r *RabbitMQService) supervise(conn *amqp.Connection, ch *amqp.Channel) {
	defer r.wg.Done()
	if conn == nil {
		conn, ch = r.reconnect()
		if conn == nil {
			return
		}
		log.Printf("RabbitMQ connected to %s, with publisher confirms", r.ConnectedTo())
	}
	connClosed := conn.NotifyClose(make(chan *amqp.Error, 1))
	chClosed := ch.NotifyClose(make(chan *amqp.Error, 1))

	for {
		select {
		case <-r.stop:
			return

		case err := <-connClosed:
			if r.closed.Load() {
				return
			}
			log.Printf("RabbitMQ connection closed (%v), reconnecting", err)
			r.dropChannel(ch)
			conn, ch = r.reconnect()
			if conn == nil {
				return
			}
			connClosed = conn.NotifyClose(make(chan *amqp.Error, 1))
			chClosed = ch.NotifyClose(make(chan *amqp.Error, 1))
			log.Printf("RabbitMQ reconnected to %s", r.ConnectedTo())

		case err := <-chClosed:
			// After a channel-level error, such as a publish to an unknown
			// exchange, the broker closes the publishing channel but keeps
			// the connection.
			chClosed = nil
			if conn.IsClosed() {
				continue // connClosed handles it
			}
			log.Printf("RabbitMQ publishing channel closed (%v), restoring it", err)
			r.dropChannel(ch)
			ch = r.restoreChannel(conn)
			if ch != nil {
				chClosed = ch.NotifyClose(make(chan *amqp.Error, 1))
			}

		case <-r.wake:
			r.mu.RLock()
			missing := r.channel == nil
			r.mu.RUnlock()
			if !missing || conn.IsClosed() {
				continue
			}
			if ch != nil {
				ch.Close()
			}
			ch = r.restoreChannel(conn)
			chClosed = nil
			if ch != nil {
				chClosed = ch.NotifyClose(make(chan *amqp.Error, 1))
			}
		}
	}
}

// reconnect dials until it succeeds or the service is closed, which returns
// nil values.
func (r *RabbitMQService) reconnect() (*amqp.Connection, *amqp.Channel) {
	backoff := reconnectInitialBackoff
	for !r.closed.Load() {
		conn, ch, err := r.open()
		if err == nil {
			if !r.install(conn, ch) {
				return nil, nil
			}
			return conn, ch
		}
		log.Printf("RabbitMQ reconnect failed: %v, retrying in %v", err, backoff)
		if !r.sleep(backoff) {
			return nil, nil
		}
		backoff = min(2*backoff, reconnectMaxBackoff)
	}
	return nil, nil
}

// restoreChannel opens a new publishing channel on conn, retrying while the
// connection stays open. It returns nil when the connection closes or the
// service is closed.
func (r *RabbitMQService) restoreChannel(conn *amqp.Connection) *amqp.Channel {
	backoff := reconnectInitialBackoff
	for !r.closed.Load() && !conn.IsClosed() {
		ch, err := r.openChannel(conn)
		if err == nil {
			r.mu.Lock()
			r.setChannelLocked(ch)
			r.mu.Unlock()
			log.Println("RabbitMQ publishing channel restored")
			return ch
		}
		log.Printf("RabbitMQ failed to restore the publishing channel: %v, retrying in %v", err, backoff)
		if !r.sleep(backoff) {
			return nil
		}
		backoff = min(2*backoff, reconnectMaxBackoff)
	}
	return nil
}

// sleep waits for d and reports false when the service is closed first.
func (r *RabbitMQService) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-r.stop:
		return false
	}
}

// IsHealthy reports whether the service has an open connection and a usable
// publishing channel. It is false while the supervisor restores either. It is
// safe for concurrent use and does not perform any network I/O.
func (r *RabbitMQService) IsHealthy() bool {
	if r.closed.Load() {
		return false
	}
	r.mu.RLock()
	conn, ch := r.conn, r.channel
	r.mu.RUnlock()
	return conn != nil && !conn.IsClosed() && ch != nil
}

// Close stops the supervisor and closes the channel and the connection. It
// is idempotent.
func (r *RabbitMQService) Close() error {
	r.closed.Store(true)
	r.stopOnce.Do(func() {
		if r.stop != nil {
			close(r.stop)
		}
	})

	r.mu.Lock()
	ch, conn := r.channel, r.conn
	r.setChannelLocked(nil)
	r.conn = nil
	r.mu.Unlock()
	if ch != nil {
		ch.Close()
	}
	if conn != nil {
		conn.Close()
	}

	r.wg.Wait()
	log.Println("RabbitMQ connection closed gracefully")
	return nil
}

// OpenChannel opens a new channel on the current connection, for consumers.
// The caller owns the channel and must close it. It is not in confirm mode,
// and it closes when the connection drops, so a consumer opens a new one after
// the service reconnects.
func (r *RabbitMQService) OpenChannel() (*amqp.Channel, error) {
	if r.closed.Load() {
		return nil, errServiceClosed
	}
	r.mu.RLock()
	conn := r.conn
	r.mu.RUnlock()
	if conn == nil || conn.IsClosed() {
		return nil, fmt.Errorf("rabbitmq connection is not available")
	}
	return conn.Channel()
}

// Publish publishes a persistent JSON message and waits until the broker
// confirms it.
func (r *RabbitMQService) Publish(ctx context.Context, exchange, message string) error {
	return r.PublishMessage(ctx, exchange, amqp.Publishing{
		ContentType:  "application/json",
		Body:         []byte(message),
		DeliveryMode: amqp.Persistent,
	})
}

// waitForChannel returns the publishing channel, waiting up to channelWait
// while the supervisor restores it. When the broker is known to be away,
// because there is no connection and the last attempt to make one failed,
// it fails at once (#232): a request that needs the broker during an outage
// is answered without a wait, while one that comes during a reconnect,
// which usually takes a moment, waits for it.
func (r *RabbitMQService) waitForChannel(ctx context.Context) (*amqp.Channel, error) {
	if r.stop == nil {
		// Never connected, so no supervisor will ever provide a channel.
		return nil, errChannelUnavailable
	}
	timer := time.NewTimer(channelWait)
	defer timer.Stop()
	for {
		if r.closed.Load() {
			return nil, errServiceClosed
		}
		r.mu.RLock()
		ch, ready, conn := r.channel, r.ready, r.conn
		r.mu.RUnlock()
		if ch != nil {
			return ch, nil
		}
		if (conn == nil || conn.IsClosed()) && r.LastError() != nil {
			return nil, fmt.Errorf("%w: %w", errChannelUnavailable, r.LastError())
		}
		r.requestChannel()
		select {
		case <-ready:
		case <-r.stop:
			return nil, errServiceClosed
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, errChannelUnavailable
		}
	}
}

// PublishMessage publishes msg and waits until the broker confirms it. The
// timestamp is set when msg has none. While the publishing channel is being
// restored, it waits up to channelWait for it.
func (r *RabbitMQService) PublishMessage(ctx context.Context, exchange string, msg amqp.Publishing) error {
	if r.closed.Load() {
		return errServiceClosed
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}

	ch, tracker, tag, ack, err := r.send(ctx, exchange, msg)
	if err != nil {
		return err
	}

	timer := time.NewTimer(publishConfirmTimeout)
	defer timer.Stop()
	select {
	case acked, ok := <-ack:
		if !ok {
			// The broker closed the channel, for example because the exchange
			// does not exist.
			r.dropChannel(ch)
			return errChannelClosed
		}
		if !acked {
			return fmt.Errorf("message nacked by broker")
		}
		return nil
	case <-ctx.Done():
		tracker.forget(tag)
		return ctx.Err()
	case <-timer.C:
		tracker.forget(tag)
		return fmt.Errorf("publish confirm timeout")
	}
}

// send reserves the delivery tag and publishes under mu, so that tags follow
// the order of publishes on the channel and the channel cannot be swapped in
// between. Waiting for the confirmation happens outside the lock, so
// concurrent publishes are pipelined.
func (r *RabbitMQService) send(ctx context.Context, exchange string, msg amqp.Publishing) (*amqp.Channel, *confirmTracker, uint64, <-chan bool, error) {
	for {
		ch, err := r.waitForChannel(ctx)
		if err != nil {
			return nil, nil, 0, nil, err
		}

		r.mu.Lock()
		if r.channel != ch {
			// Replaced while waiting for the lock.
			r.mu.Unlock()
			continue
		}
		tracker := r.confirms
		tag, ack, err := tracker.register()
		if err != nil {
			r.setChannelLocked(nil)
			r.mu.Unlock()
			r.requestChannel()
			return nil, nil, 0, nil, err
		}
		if err := ch.Publish(exchange, "", false, false, msg); err != nil {
			// The tag was never used, so this channel's tags no longer line
			// up; the supervisor replaces it with a new channel and tracker.
			tracker.forget(tag)
			r.setChannelLocked(nil)
			r.mu.Unlock()
			r.requestChannel()
			return nil, nil, 0, nil, err
		}
		r.mu.Unlock()
		return ch, tracker, tag, ack, nil
	}
}

func (r *RabbitMQService) PublishWithRetry(ctx context.Context, exchange, message string, retryCount int) error {
	var lastErr error
	for i := 0; i < retryCount; i++ {
		if err := r.Publish(ctx, exchange, message); err != nil {
			lastErr = err
			log.Printf("Failed to publish message, attempt %d/%d: %v", i+1, retryCount, err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(i+1) * 500 * time.Millisecond):
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("failed to publish message after %d attempts: %w", retryCount, lastErr)
}
