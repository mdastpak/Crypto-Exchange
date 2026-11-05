package notify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Default queue topology of the Worker.
const (
	// DefaultOTPQueue receives the sign-in code jobs published to the
	// notifications exchange.
	DefaultOTPQueue = "notifications.otp"
	// DefaultDeadLetterExchange receives a record of every job that could not
	// be delivered.
	DefaultDeadLetterExchange = "notifications.dead-letter"
	// DefaultOTPDeadLetterQueue keeps those records for deadLetterTTL.
	DefaultOTPDeadLetterQueue = "notifications.otp.dead-letter"
)

const (
	deadLetterTTL       = 7 * 24 * time.Hour
	defaultRetryBackoff = time.Second
	maxReconnectBackoff = 30 * time.Second
)

// Reasons recorded when a job is given up.
const (
	reasonMalformed        = "malformed"
	reasonNoChannel        = "no_channel"
	reasonExpired          = "expired"
	reasonRejected         = "rejected"
	reasonRetriesExhausted = "retries_exhausted"
)

var errStopping = errors.New("the worker is stopping")

// Broker is the part of the RabbitMQ service that the Worker uses.
type Broker interface {
	Publisher
	// OpenChannel opens a channel for consuming. The caller closes it.
	OpenChannel() (*amqp.Channel, error)
}

// WorkerOptions configures a Worker. Zero values select the defaults.
type WorkerOptions struct {
	// Exchange is the exchange QueueSender publishes to.
	Exchange           string
	Queue              string
	DeadLetterExchange string
	DeadLetterQueue    string
	// Concurrency is the number of jobs delivered in parallel (default 4).
	Concurrency int
	// MaxAttempts bounds the delivery attempts per job (default 3). A job is
	// also given up once its code expires.
	MaxAttempts int
	Logger      *log.Logger
}

// Worker consumes sign-in code jobs and delivers them through the configured
// channels. Temporary failures are retried with exponential backoff until the
// attempts run out or the code expires. A job that cannot be delivered is
// recorded on the dead-letter queue, without its code or raw contact.
type Worker struct {
	broker             Broker
	channels           Channels
	exchange           string
	queue              string
	deadLetterExchange string
	deadLetterQueue    string
	concurrency        int
	maxAttempts        int
	retryBackoff       time.Duration
	logger             *log.Logger
	now                func() time.Time

	started  atomic.Bool
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// NewWorker returns a worker that delivers jobs through channels.
func NewWorker(broker Broker, channels Channels, opts WorkerOptions) *Worker {
	w := &Worker{
		broker:             broker,
		channels:           channels,
		exchange:           opts.Exchange,
		queue:              opts.Queue,
		deadLetterExchange: opts.DeadLetterExchange,
		deadLetterQueue:    opts.DeadLetterQueue,
		concurrency:        opts.Concurrency,
		maxAttempts:        opts.MaxAttempts,
		retryBackoff:       defaultRetryBackoff,
		logger:             opts.Logger,
		now:                time.Now,
		stop:               make(chan struct{}),
		done:               make(chan struct{}),
	}
	if w.queue == "" {
		w.queue = DefaultOTPQueue
	}
	if w.deadLetterExchange == "" {
		w.deadLetterExchange = DefaultDeadLetterExchange
	}
	if w.deadLetterQueue == "" {
		w.deadLetterQueue = DefaultOTPDeadLetterQueue
	}
	if w.concurrency <= 0 {
		w.concurrency = 4
	}
	if w.maxAttempts <= 0 {
		w.maxAttempts = 3
	}
	if w.logger == nil {
		w.logger = log.Default()
	}
	return w
}

// Start declares the queues and starts consuming in the background. The
// queues are declared before Start returns, because the exchange drops jobs
// published while no queue is bound to it.
func (w *Worker) Start() error {
	if w.exchange == "" {
		return errors.New("the worker needs the notifications exchange")
	}
	ch, err := w.broker.OpenChannel()
	if err != nil {
		return fmt.Errorf("open a channel: %w", err)
	}
	err = w.declare(ch)
	ch.Close()
	if err != nil {
		return err
	}
	if !w.started.CompareAndSwap(false, true) {
		return errors.New("the worker is already started")
	}
	go w.run()
	return nil
}

// Stop stops consuming and waits until the jobs in progress are finished or
// ctx ends. Jobs that are waiting for a retry are returned to the queue.
func (w *Worker) Stop(ctx context.Context) error {
	w.stopOnce.Do(func() { close(w.stop) })
	if !w.started.Load() {
		return nil
	}
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Worker) stopping() bool {
	select {
	case <-w.stop:
		return true
	default:
		return false
	}
}

func (w *Worker) declare(ch *amqp.Channel) error {
	if err := ch.ExchangeDeclare(w.exchange, "fanout", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare exchange %s: %w", w.exchange, err)
	}
	if err := ch.ExchangeDeclare(w.deadLetterExchange, "fanout", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare exchange %s: %w", w.deadLetterExchange, err)
	}
	deadLetterArgs := amqp.Table{"x-message-ttl": int64(deadLetterTTL / time.Millisecond)}
	if _, err := ch.QueueDeclare(w.deadLetterQueue, true, false, false, false, deadLetterArgs); err != nil {
		return fmt.Errorf("declare queue %s: %w", w.deadLetterQueue, err)
	}
	if err := ch.QueueBind(w.deadLetterQueue, "", w.deadLetterExchange, false, nil); err != nil {
		return fmt.Errorf("bind queue %s: %w", w.deadLetterQueue, err)
	}
	if _, err := ch.QueueDeclare(w.queue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare queue %s: %w", w.queue, err)
	}
	if err := ch.QueueBind(w.queue, "", w.exchange, false, nil); err != nil {
		return fmt.Errorf("bind queue %s: %w", w.queue, err)
	}
	return nil
}

// run consumes until the worker stops, and opens a new channel whenever the
// current one closes, for example while the service reconnects to the broker.
func (w *Worker) run() {
	defer close(w.done)
	backoff := time.Second
	for {
		consumed, err := w.consume()
		if w.stopping() {
			return
		}
		if consumed {
			backoff = time.Second
		}
		w.logger.Printf("OTP delivery worker: %v; consuming again in %v", err, backoff)
		select {
		case <-w.stop:
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxReconnectBackoff)
	}
}

// consume delivers jobs from one channel until it closes or the worker stops.
// consumed reports whether consuming started.
func (w *Worker) consume() (consumed bool, err error) {
	ch, err := w.broker.OpenChannel()
	if err != nil {
		return false, fmt.Errorf("open a channel: %w", err)
	}
	defer ch.Close()

	if err := w.declare(ch); err != nil {
		return false, err
	}
	// Each delivery goroutine holds at most one unacknowledged job.
	if err := ch.Qos(w.concurrency, 0, false); err != nil {
		return false, fmt.Errorf("set the prefetch count: %w", err)
	}
	tag, err := consumerTag()
	if err != nil {
		return false, err
	}
	deliveries, err := ch.Consume(w.queue, tag, false, false, false, false, nil)
	if err != nil {
		return false, fmt.Errorf("consume %s: %w", w.queue, err)
	}

	finished := make(chan struct{})
	go func() {
		select {
		case <-w.stop:
			// The deliveries channel closes once the broker confirms.
			_ = ch.Cancel(tag, false)
		case <-finished:
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < w.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range deliveries {
				w.handle(d)
			}
		}()
	}
	wg.Wait()
	close(finished)
	return true, errors.New("the consumer channel closed")
}

func consumerTag() (string, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "otp-worker-" + hex.EncodeToString(random), nil
}

// handle delivers one job and settles it. Acknowledgement errors are ignored:
// they mean that the channel closed, and the broker then redelivers the job.
func (w *Worker) handle(d amqp.Delivery) {
	if w.stopping() {
		_ = d.Nack(false, true)
		return
	}

	var job otpJob
	if err := json.Unmarshal(d.Body, &job); err != nil || job.Type != otpJobType {
		w.logger.Printf("Dropped a message that is not a sign-in code job (type %q)", d.Type)
		w.deadLetter(deadLetter{Reason: reasonMalformed})
		_ = d.Ack(false)
		return
	}

	deliverer := w.channels.For(job.ContactType)
	if deliverer == nil {
		w.giveUp(job, 0, reasonNoChannel, ErrUnsupportedContactType)
		_ = d.Ack(false)
		return
	}

	attempts, reason, err := w.deliver(deliverer, job)
	switch {
	case err == nil:
		w.logger.Printf("Delivered the sign-in code to %s %s (attempt %d)", job.ContactType, job.MaskedContact, attempts)
		_ = d.Ack(false)
	case errors.Is(err, errStopping):
		_ = d.Nack(false, true)
	default:
		w.giveUp(job, attempts, reason, err)
		_ = d.Ack(false)
	}
}

// deliver attempts delivery until it succeeds, fails permanently, runs out of
// attempts, or the code expires. It returns the number of attempts made and,
// on failure, the reason for giving up.
func (w *Worker) deliver(deliverer Deliverer, job otpJob) (attempts int, reason string, err error) {
	msg := job.message()
	backoff := w.retryBackoff
	for {
		if job.expired(w.now()) {
			if err == nil {
				err = errors.New("the code expired before it could be delivered")
			}
			return attempts, reasonExpired, err
		}

		attempts++
		ctx, cancel := job.deliveryContext()
		err = deliverer.Deliver(ctx, msg)
		cancel()
		if err == nil {
			return attempts, "", nil
		}
		if IsPermanent(err) {
			return attempts, reasonRejected, err
		}
		if attempts >= w.maxAttempts {
			return attempts, reasonRetriesExhausted, err
		}
		if job.expired(w.now().Add(backoff)) {
			return attempts, reasonExpired, err
		}

		w.logger.Printf("Attempt %d to deliver the sign-in code to %s %s failed, retrying in %v: %s",
			attempts, job.ContactType, job.MaskedContact, backoff, redact(err, msg))
		select {
		case <-w.stop:
			return attempts, "", errStopping
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// deadLetter is the record kept for a job that could not be delivered. It
// leaves out the code and the raw contact.
type deadLetter struct {
	Type          string    `json:"type"`
	ContactType   string    `json:"contact_type,omitempty"`
	MaskedContact string    `json:"masked_contact,omitempty"`
	Reason        string    `json:"reason"`
	Attempts      int       `json:"attempts"`
	Error         string    `json:"error,omitempty"`
	FailedAt      time.Time `json:"failed_at"`
}

func (w *Worker) giveUp(job otpJob, attempts int, reason string, err error) {
	errText := redact(err, job.message())
	w.logger.Printf("Gave up delivering the sign-in code to %s %s after %d attempt(s) (%s): %s",
		job.ContactType, job.MaskedContact, attempts, reason, errText)
	w.deadLetter(deadLetter{
		ContactType:   job.ContactType,
		MaskedContact: job.MaskedContact,
		Reason:        reason,
		Attempts:      attempts,
		Error:         errText,
	})
}

func (w *Worker) deadLetter(record deadLetter) {
	record.Type = otpJobType
	record.FailedAt = w.now().UTC()
	body, err := json.Marshal(record)
	if err != nil {
		w.logger.Printf("Failed to encode a dead-letter record: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = w.broker.PublishMessage(ctx, w.deadLetterExchange, amqp.Publishing{
		ContentType:  "application/json",
		Type:         "otp_failure",
		DeliveryMode: amqp.Persistent,
		Body:         body,
	})
	if err != nil {
		w.logger.Printf("Failed to record an undelivered sign-in code on %s: %v", w.deadLetterQueue, err)
	}
}
