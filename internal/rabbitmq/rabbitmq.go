package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"encrypted-db/config"

	"github.com/streadway/amqp"
)

type RabbitMQService struct {
	conn    *amqp.Connection
	connMu  sync.RWMutex
	channel *amqp.Channel
	// confirms routes the publisher confirmations of channel to the publishes
	// waiting for them. It owns the channel's only confirmation listener: the
	// library delivers every confirmation to every listener with a blocking
	// send, so a listener per publish stalls the connection after a few
	// messages.
	confirms    *confirmTracker
	chMu        sync.RWMutex
	closed      atomic.Bool
	reconnectWg sync.WaitGroup
	notifyClose chan *amqp.Error
	url         string
	exchanges   []string
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

func NewRabbitMQService(exchanges ...string) (*RabbitMQService, error) {
	url := config.GetRabbitMQURL()
	r := &RabbitMQService{
		url:         url,
		exchanges:   exchanges,
		notifyClose: make(chan *amqp.Error, 1),
	}

	if err := r.connect(); err != nil {
		return nil, fmt.Errorf("failed to connect to RabbitMQ: %w", err)
	}

	r.reconnectWg.Add(1)
	go r.reconnectLoop()

	return r, nil
}

func (r *RabbitMQService) connect() error {
	r.connMu.Lock()
	defer r.connMu.Unlock()

	conn, err := amqp.DialConfig(r.url, amqp.Config{
		Heartbeat: 10 * time.Second,
		Locale:    "en_US",
	})
	if err != nil {
		return err
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return err
	}

	if err := ch.Confirm(false); err != nil {
		ch.Close()
		conn.Close()
		return fmt.Errorf("failed to enable publisher confirms: %w", err)
	}

	r.conn = conn
	r.useChannel(ch)
	r.closed.Store(false)

	r.notifyClose = conn.NotifyClose(make(chan *amqp.Error, 1))

	for _, exchange := range r.exchanges {
		if err := r.declareExchange(ch, exchange); err != nil {
			ch.Close()
			conn.Close()
			return fmt.Errorf("failed to declare exchange %s: %w", exchange, err)
		}
	}

	log.Println("RabbitMQ connection and channel established with publisher confirms")
	return nil
}

func (r *RabbitMQService) declareExchange(ch *amqp.Channel, exchangeName string) error {
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

func (r *RabbitMQService) GetChannel() (*amqp.Channel, error) {
	r.chMu.RLock()
	ch := r.channel
	r.chMu.RUnlock()

	if ch != nil {
		return ch, nil
	}

	return r.recreateChannel()
}

func (r *RabbitMQService) recreateChannel() (*amqp.Channel, error) {
	r.chMu.Lock()
	defer r.chMu.Unlock()

	if r.channel != nil {
		return r.channel, nil
	}

	r.connMu.RLock()
	conn := r.conn
	r.connMu.RUnlock()

	if conn == nil || conn.IsClosed() {
		if err := r.connect(); err != nil {
			return nil, err
		}
	} else {
		ch, err := conn.Channel()
		if err != nil {
			return nil, err
		}
		if err := ch.Confirm(false); err != nil {
			ch.Close()
			return nil, err
		}
		for _, exchange := range r.exchanges {
			if err := r.declareExchange(ch, exchange); err != nil {
				ch.Close()
				return nil, err
			}
		}
		r.useChannel(ch)
	}
	return r.channel, nil
}

// useChannel makes ch the publishing channel, with its own confirmation tracker.
func (r *RabbitMQService) useChannel(ch *amqp.Channel) {
	r.channel = ch
	r.confirms = newConfirmTracker(ch)
}

// dropChannel forgets ch, if it is still the current channel, so that the next
// publish opens a new one.
func (r *RabbitMQService) dropChannel(ch *amqp.Channel) {
	r.chMu.Lock()
	if r.channel == ch {
		r.channel = nil
	}
	r.chMu.Unlock()
}

func (r *RabbitMQService) reconnectLoop() {
	defer r.reconnectWg.Done()

	for {
		select {
		case err := <-r.notifyClose:
			if err != nil {
				log.Printf("RabbitMQ connection closed: %v, reconnecting...", err)
			} else {
				log.Println("RabbitMQ connection closed gracefully")
			}
			if r.closed.Load() {
				return
			}
			r.reconnect()
		case <-time.After(30 * time.Second):
			if r.closed.Load() {
				return
			}
			if err := r.healthCheck(); err != nil {
				log.Printf("RabbitMQ health check failed: %v, reconnecting...", err)
				r.reconnect()
			}
		}
	}
}

func (r *RabbitMQService) healthCheck() error {
	r.chMu.RLock()
	ch := r.channel
	r.chMu.RUnlock()

	if ch == nil {
		return fmt.Errorf("no channel")
	}
	return nil
}

// IsHealthy reports whether the service has an active connection and channel.
// It is safe for concurrent use and does not perform any network I/O.
func (r *RabbitMQService) IsHealthy() bool {
	if r.closed.Load() {
		return false
	}

	r.connMu.RLock()
	conn := r.conn
	r.connMu.RUnlock()
	if conn == nil || conn.IsClosed() {
		return false
	}

	return r.healthCheck() == nil
}

func (r *RabbitMQService) reconnect() {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for !r.closed.Load() {
		if err := r.connect(); err != nil {
			log.Printf("RabbitMQ reconnect failed: %v, retrying in %v", err, backoff)
			time.Sleep(backoff)
			backoff = time.Duration(float64(backoff) * 1.5)
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		log.Println("RabbitMQ reconnected successfully")
		return
	}
}

func (r *RabbitMQService) Close() error {
	r.closed.Store(true)

	r.chMu.Lock()
	if r.channel != nil {
		r.channel.Close()
		r.channel = nil
	}
	r.chMu.Unlock()

	r.connMu.Lock()
	if r.conn != nil {
		r.conn.Close()
		r.conn = nil
	}
	r.connMu.Unlock()

	r.reconnectWg.Wait()
	log.Println("RabbitMQ connection closed gracefully")
	return nil
}

func (r *RabbitMQService) Publish(ctx context.Context, exchange, message string) error {
	if r.closed.Load() {
		return fmt.Errorf("rabbitmq service closed")
	}

	if _, err := r.GetChannel(); err != nil {
		return err
	}

	// Reserve the delivery tag and publish under chMu, so that tags follow the
	// order of publishes on the channel and a reconnect cannot swap the channel
	// in between. Waiting for the confirmation happens outside the lock, so
	// concurrent publishes are pipelined.
	r.chMu.Lock()
	ch, tracker := r.channel, r.confirms
	if ch == nil || tracker == nil {
		r.chMu.Unlock()
		return fmt.Errorf("rabbitmq channel is not available")
	}
	tag, ack, err := tracker.register()
	if err != nil {
		if r.channel == ch {
			r.channel = nil
		}
		r.chMu.Unlock()
		return err
	}
	err = ch.Publish(
		exchange,
		"",
		false,
		false,
		amqp.Publishing{
			ContentType:  "application/json",
			Body:         []byte(message),
			DeliveryMode: amqp.Persistent,
			Timestamp:    time.Now(),
		},
	)
	if err != nil {
		// The tag was never used, so this channel's tags no longer line up;
		// the next publish opens a new channel with a new tracker.
		tracker.forget(tag)
		r.channel = nil
		r.chMu.Unlock()
		return err
	}
	r.chMu.Unlock()

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
