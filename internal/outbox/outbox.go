// Package outbox delivers the messages that a database transaction writes:
// a message is published if and only if its transaction commits, also when
// the broker is unavailable at the time.
//
// A change writes its messages with Enqueue in its own transaction. A Relay
// publishes them afterwards, in id order within each topic, and retries a
// failed message with backoff until it succeeds, holding back the later
// messages of its topic meanwhile. Every server instance runs a relay, but
// one at a time relays, under a PostgreSQL advisory lock, so that two
// instances never publish the same message or two messages of a topic out
// of order. Delivery is at least once: a message whose publish succeeded is
// published again if the relay cannot record that.
package outbox

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Enqueue writes a message of topic in tx. It is published after tx
// commits, and never if tx rolls back. payload must be JSON.
func Enqueue(ctx context.Context, tx *sql.Tx, topic string, payload []byte) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO outbox_events (topic, payload) VALUES ($1, $2::jsonb)`, topic, payload)
	return err
}

// Publisher publishes a message to an exchange. *rabbitmq.RabbitMQService
// implements it.
type Publisher interface {
	Publish(ctx context.Context, exchange, message string) error
}

// Route says how the messages of a topic are delivered.
type Route struct {
	// Exchange is the exchange the messages are published to.
	Exchange string
	// Before runs before each message is published, for example to drop a
	// cache that the change made out of date. A failure is retried like a
	// failed publish. It may be nil.
	Before func(ctx context.Context) error
}

// relayLockID is the advisory lock that one instance at a time holds while
// it relays. It must differ from the migration lock of internal/db.
const relayLockID int64 = 0x6f7574626f78 // "outbox"

const (
	defaultBatch     = 100
	defaultInterval  = time.Second
	defaultRetention = 7 * 24 * time.Hour
	// sendTimeout bounds Before and Publish for one message.
	sendTimeout = 10 * time.Second
	// maxBackoff bounds the wait between the attempts of a message.
	maxBackoff = 5 * time.Minute
	// cleanupEvery is how often published messages past the retention are
	// deleted, and cleanupBatch how many at a time.
	cleanupEvery = time.Hour
	cleanupBatch = 1000
	// maxErrorLength bounds the last error stored with a message.
	maxErrorLength = 500
)

// Options tunes a relay. Zero values take the defaults.
type Options struct {
	// Batch is how many messages one transaction relays at most (100).
	Batch int
	// Interval is how often the relay looks for messages (1 s). Nudge
	// makes it look at once.
	Interval time.Duration
	// Retention is how long published messages are kept (7 days).
	Retention time.Duration
	Logger    *log.Logger
}

// Relay publishes the messages of the outbox.
type Relay struct {
	db        *sql.DB
	publisher Publisher
	routes    map[string]Route
	batch     int
	interval  time.Duration
	retention time.Duration
	logger    *log.Logger

	nudge    chan struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	started  atomic.Bool
}

// NewRelay returns a relay that publishes the messages of the topics in
// routes. Messages of other topics, which a newer version of the server may
// write during a rolling deploy, are left for a relay that knows them.
func NewRelay(db *sql.DB, publisher Publisher, routes map[string]Route, opts Options) *Relay {
	r := &Relay{
		db: db, publisher: publisher, routes: routes,
		batch: opts.Batch, interval: opts.Interval, retention: opts.Retention, logger: opts.Logger,
		nudge: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	if r.batch <= 0 {
		r.batch = defaultBatch
	}
	if r.interval <= 0 {
		r.interval = defaultInterval
	}
	if r.retention <= 0 {
		r.retention = defaultRetention
	}
	if r.logger == nil {
		r.logger = log.Default()
	}
	return r
}

// Start relays in the background until Stop.
func (r *Relay) Start() {
	if r.started.CompareAndSwap(false, true) {
		go r.run()
	}
}

// Stop ends the relay after the message it is sending, and waits for it
// until ctx ends. Messages it did not send stay in the outbox, for the
// relay of another instance or for the next start. It may be called more
// than once.
func (r *Relay) Stop(ctx context.Context) error {
	r.stopOnce.Do(func() { close(r.stop) })
	if !r.started.Load() {
		return nil
	}
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Nudge makes the relay look for messages at once, for example after a
// transaction that wrote some has committed. It never blocks.
func (r *Relay) Nudge() {
	select {
	case r.nudge <- struct{}{}:
	default:
	}
}

func (r *Relay) stopping() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

func (r *Relay) run() {
	defer close(r.done)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	var lastCleanup time.Time
	for {
		if _, err := r.Flush(context.Background()); err != nil {
			r.logger.Printf("Outbox relay: %v", err)
		}
		if time.Since(lastCleanup) >= cleanupEvery {
			if err := r.cleanup(context.Background()); err != nil {
				r.logger.Printf("Outbox relay: deleting published messages: %v", err)
			} else {
				lastCleanup = time.Now()
			}
		}
		select {
		case <-r.stop:
			return
		case <-ticker.C:
		case <-r.nudge:
		}
	}
}

// Flush relays batches until no message is ready, another instance is
// relaying, or the relay stops. It returns how many messages it published.
func (r *Relay) Flush(ctx context.Context) (int, error) {
	total := 0
	for !r.stopping() {
		published, more, err := r.relayBatch(ctx)
		total += published
		if err != nil || !more {
			return total, err
		}
	}
	return total, nil
}

type message struct {
	id       int64
	topic    string
	payload  string
	attempts int
	// ready is false while the message waits for its next attempt. The
	// database's clock decides, as it set the time.
	ready bool
}

// relayBatch relays up to batch pending messages in one transaction, under
// the advisory lock. more reports whether a full batch was published, so
// that more messages may be ready.
func (r *Relay) relayBatch(ctx context.Context) (published int, more bool, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }()

	var locked bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, relayLockID).Scan(&locked); err != nil {
		return 0, false, err
	}
	if !locked {
		// Another instance is relaying.
		return 0, false, nil
	}

	pending, err := r.pending(ctx, tx)
	if err != nil {
		return 0, false, err
	}
	// The first message of a topic that cannot be sent now holds back the
	// later ones, so that a topic's messages are published in order.
	blocked := make(map[string]bool)
	for _, msg := range pending {
		if r.stopping() {
			break
		}
		if blocked[msg.topic] {
			continue
		}
		route, known := r.routes[msg.topic]
		if !known || !msg.ready {
			blocked[msg.topic] = true
			continue
		}
		if err := r.send(ctx, route, msg); err != nil {
			blocked[msg.topic] = true
			if err := r.recordFailure(ctx, tx, msg, err); err != nil {
				return 0, false, err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE outbox_events SET published_at = NOW() WHERE id = $1`, msg.id); err != nil {
			return 0, false, err
		}
		published++
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("record the published messages, which are published again: %w", err)
	}
	return published, len(pending) == r.batch && published > 0, nil
}

func (r *Relay) pending(ctx context.Context, tx *sql.Tx) ([]message, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, topic, payload, attempts, next_attempt_at <= NOW() FROM outbox_events
		WHERE published_at IS NULL ORDER BY id LIMIT $1`, r.batch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []message
	for rows.Next() {
		var msg message
		if err := rows.Scan(&msg.id, &msg.topic, &msg.payload, &msg.attempts, &msg.ready); err != nil {
			return nil, err
		}
		pending = append(pending, msg)
	}
	return pending, rows.Err()
}

// send runs the route's Before and publishes the message.
func (r *Relay) send(ctx context.Context, route Route, msg message) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if route.Before != nil {
		if err := route.Before(ctx); err != nil {
			return fmt.Errorf("before publishing: %w", err)
		}
	}
	return r.publisher.Publish(ctx, route.Exchange, msg.payload)
}

func (r *Relay) recordFailure(ctx context.Context, tx *sql.Tx, msg message, failure error) error {
	attempts := msg.attempts + 1
	wait := backoff(attempts)
	text := failure.Error()
	if len(text) > maxErrorLength {
		text = text[:maxErrorLength]
	}
	r.logger.Printf("Outbox relay: message %d of %s failed (attempt %d), retrying in %v: %s", msg.id, msg.topic, attempts, wait, text)
	_, err := tx.ExecContext(ctx,
		`UPDATE outbox_events SET attempts = $2, next_attempt_at = NOW() + make_interval(secs => $3), last_error = $4 WHERE id = $1`,
		msg.id, attempts, wait.Seconds(), text)
	return err
}

// backoff is the wait after the given number of failed attempts: a second,
// doubling up to maxBackoff.
func backoff(attempts int) time.Duration {
	wait := time.Second
	for i := 1; i < attempts && wait < maxBackoff; i++ {
		wait *= 2
	}
	return min(wait, maxBackoff)
}

// cleanup deletes the messages published longer ago than the retention, in
// batches, under the advisory lock.
func (r *Relay) cleanup(ctx context.Context) error {
	for !r.stopping() {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		var locked bool
		err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, relayLockID).Scan(&locked)
		if err != nil || !locked {
			_ = tx.Rollback()
			return err
		}
		result, err := tx.ExecContext(ctx,
			`DELETE FROM outbox_events WHERE id IN (
				SELECT id FROM outbox_events WHERE published_at < NOW() - make_interval(secs => $1) ORDER BY id LIMIT $2)`,
			r.retention.Seconds(), cleanupBatch)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n < cleanupBatch {
			return err
		}
	}
	return nil
}
