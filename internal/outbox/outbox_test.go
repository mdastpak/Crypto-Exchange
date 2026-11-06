package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDBName is the PostgreSQL database of this package.
const testDBName = "testdb_outbox"

// publisher records the messages and fails those that fail says to.
type publisher struct {
	mu   sync.Mutex
	sent []string
	fail func(message string) error
}

func (p *publisher) Publish(_ context.Context, exchange, message string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		if err := p.fail(message); err != nil {
			return err
		}
	}
	var payload struct {
		N string `json:"n"`
	}
	if err := json.Unmarshal([]byte(message), &payload); err != nil {
		return err
	}
	p.sent = append(p.sent, exchange+":"+payload.N)
	return nil
}

func (p *publisher) messages() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.sent...)
}

func (p *publisher) failOn(n string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fail = func(message string) error {
		if strings.Contains(message, `"`+n+`"`) {
			return err
		}
		return nil
	}
}

var quiet = log.New(io.Discard, "", 0)

func newRelay(db *sql.DB, p Publisher, routes map[string]Route) *Relay {
	return NewRelay(db, p, routes, Options{Interval: time.Hour, Logger: quiet})
}

var twoTopics = map[string]Route{"a": {Exchange: "ex-a"}, "b": {Exchange: "ex-b"}}

// enqueue writes one message per name, each in a transaction of its own.
func enqueue(t *testing.T, db *sql.DB, topic string, names ...string) {
	t.Helper()
	for _, n := range names {
		tx, err := db.Begin()
		require.NoError(t, err)
		require.NoError(t, Enqueue(context.Background(), tx, topic, []byte(fmt.Sprintf(`{"n": %q}`, n))))
		require.NoError(t, tx.Commit())
	}
}

func pendingCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&n))
	return n
}

// makeReady ends the backoff of every pending message.
func makeReady(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`UPDATE outbox_events SET next_attempt_at = NOW() WHERE published_at IS NULL`)
	require.NoError(t, err)
}

func TestEnqueue_OnlyCommittedMessagesArePublished(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	ctx := context.Background()
	enqueue(t, db, "a", "committed")
	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, Enqueue(ctx, tx, "a", []byte(`{"n": "rolled back"}`)))
	require.NoError(t, tx.Rollback())

	p := &publisher{}
	published, err := newRelay(db, p, twoTopics).Flush(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, published)
	assert.Equal(t, []string{"ex-a:committed"}, p.messages())
	assert.Zero(t, pendingCount(t, db))
}

func TestEnqueue_RejectsAPayloadThatIsNotJSON(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	assert.Error(t, Enqueue(context.Background(), tx, "a", []byte("not json")))
}

func TestFlush_PublishesInIDOrder(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	enqueue(t, db, "a", "a1", "a2")
	enqueue(t, db, "b", "b1")
	enqueue(t, db, "a", "a3")

	p := &publisher{}
	r := NewRelay(db, p, twoTopics, Options{Batch: 2, Interval: time.Hour, Logger: quiet})
	published, err := r.Flush(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 4, published, "Flush relays batch after batch")
	assert.Equal(t, []string{"ex-a:a1", "ex-a:a2", "ex-b:b1", "ex-a:a3"}, p.messages())
}

// TestFlush_AFailureHoldsBackItsTopicOnly fails the first message of a topic.
// Its later messages wait, so that the topic stays in order, while other
// topics go on.
func TestFlush_AFailureHoldsBackItsTopicOnly(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	ctx := context.Background()
	enqueue(t, db, "a", "a1", "a2")
	enqueue(t, db, "b", "b1")
	p := &publisher{}
	p.failOn("a1", errors.New("broker unavailable"))
	r := newRelay(db, p, twoTopics)

	published, err := r.Flush(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, published)
	assert.Equal(t, []string{"ex-b:b1"}, p.messages())
	var attempts int
	var lastError string
	var waiting bool
	require.NoError(t, db.QueryRow(
		`SELECT attempts, last_error, next_attempt_at > NOW() FROM outbox_events WHERE payload->>'n' = 'a1'`,
	).Scan(&attempts, &lastError, &waiting))
	assert.Equal(t, 1, attempts)
	assert.Equal(t, "broker unavailable", lastError)
	assert.True(t, waiting, "the message waits for its next attempt")

	published, err = r.Flush(ctx)
	require.NoError(t, err)
	assert.Zero(t, published, "nothing is retried before the backoff ends")

	p.failOn("none", nil)
	makeReady(t, db)
	_, err = r.Flush(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"ex-b:b1", "ex-a:a1", "ex-a:a2"}, p.messages(), "the topic is published in order once the broker is back")
	assert.Zero(t, pendingCount(t, db))
}

func TestFlush_RetriesAFailedBefore(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	ctx := context.Background()
	enqueue(t, db, "a", "a1")
	cacheDown := true
	var before []bool
	routes := map[string]Route{"a": {Exchange: "ex-a", Before: func(context.Context) error {
		before = append(before, cacheDown)
		if cacheDown {
			return errors.New("redis unavailable")
		}
		return nil
	}}}
	p := &publisher{}
	r := newRelay(db, p, routes)

	_, err := r.Flush(ctx)
	require.NoError(t, err)
	assert.Empty(t, p.messages(), "a message is not published before its Before succeeds")

	cacheDown = false
	makeReady(t, db)
	_, err = r.Flush(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"ex-a:a1"}, p.messages())
	assert.Equal(t, []bool{true, false}, before)
}

// TestFlush_LeavesTopicsWithoutARoute writes a message of a topic that the
// relay does not know, as a newer version of the server could during a
// rolling deploy. It stays for a relay that knows it.
func TestFlush_LeavesTopicsWithoutARoute(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	enqueue(t, db, "newer", "x1")
	enqueue(t, db, "a", "a1")
	p := &publisher{}

	_, err := newRelay(db, p, twoTopics).Flush(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"ex-a:a1"}, p.messages())
	var attempts int
	require.NoError(t, db.QueryRow(`SELECT attempts FROM outbox_events WHERE topic = 'newer' AND published_at IS NULL`).Scan(&attempts))
	assert.Zero(t, attempts, "the message is left, not failed")
}

// TestFlush_OneInstanceAtATime runs two relays, as two server instances, on
// one outbox at the same time. Every message must be published once, and in
// order.
func TestFlush_OneInstanceAtATime(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	names := make([]string, 0, 200)
	want := make([]string, 0, 200)
	for i := range 200 {
		names = append(names, fmt.Sprintf("%03d", i))
		want = append(want, fmt.Sprintf("ex-a:%03d", i))
	}
	enqueue(t, db, "a", names...)
	p := &publisher{}
	relays := []*Relay{
		NewRelay(db, p, twoTopics, Options{Batch: 10, Interval: time.Hour, Logger: quiet}),
		NewRelay(db, p, twoTopics, Options{Batch: 10, Interval: time.Hour, Logger: quiet}),
	}

	var wg sync.WaitGroup
	for _, r := range relays {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deadline := time.Now().Add(30 * time.Second)
			for pendingCount(t, db) > 0 && time.Now().Before(deadline) {
				_, err := r.Flush(context.Background())
				assert.NoError(t, err)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, want, p.messages(), "each message once, in order")
}

// slowPublisher takes delay for every message, as a broker that is slow
// but not failing.
type slowPublisher struct {
	publisher
	delay time.Duration
}

func (p *slowPublisher) Publish(ctx context.Context, exchange, message string) error {
	time.Sleep(p.delay)
	return p.publisher.Publish(ctx, exchange, message)
}

// TestFlush_ASlowBrokerEndsTheBatchEarly checks that a batch stops sending
// after its time, so that its transaction does not stay open for the whole
// batch, and that Flush goes on with the next batch.
func TestFlush_ASlowBrokerEndsTheBatchEarly(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	enqueue(t, db, "a", "a1", "a2", "a3", "a4")
	p := &slowPublisher{delay: 30 * time.Millisecond}
	r := newRelay(db, p, twoTopics)
	r.batchTime = 50 * time.Millisecond

	published, more, err := r.relayBatch(context.Background())
	require.NoError(t, err)
	assert.Less(t, published, 4, "the batch ends after its time")
	assert.True(t, more)

	total, err := r.Flush(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 4, published+total)
	assert.Equal(t, []string{"ex-a:a1", "ex-a:a2", "ex-a:a3", "ex-a:a4"}, p.messages(), "in order across batches")
}

func TestBackoff_DoublesUpToFiveMinutes(t *testing.T) {
	assert.Equal(t, time.Second, backoff(1))
	assert.Equal(t, 2*time.Second, backoff(2))
	assert.Equal(t, 4*time.Second, backoff(3))
	assert.Equal(t, 256*time.Second, backoff(9))
	assert.Equal(t, 5*time.Minute, backoff(10))
	assert.Equal(t, 5*time.Minute, backoff(1000))
}

func TestCleanup_DeletesOnlyOldPublishedMessages(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	_, err := db.Exec(`INSERT INTO outbox_events (topic, payload, created_at, published_at) VALUES
		('a', '{"n": "old"}', NOW() - INTERVAL '9 days', NOW() - INTERVAL '8 days'),
		('a', '{"n": "recent"}', NOW() - INTERVAL '1 day', NOW() - INTERVAL '1 day'),
		('a', '{"n": "pending"}', NOW() - INTERVAL '9 days', NULL)`)
	require.NoError(t, err)

	require.NoError(t, newRelay(db, &publisher{}, twoTopics).cleanup(context.Background()))
	rows, err := db.Query(`SELECT payload->>'n' FROM outbox_events ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	var left []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		left = append(left, n)
	}
	assert.Equal(t, []string{"recent", "pending"}, left)
}

func TestRelay_NudgeAndStop(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	p := &publisher{}
	r := newRelay(db, p, twoTopics)
	require.NoError(t, r.Stop(context.Background()), "a relay that never started stops at once")

	r = newRelay(db, p, twoTopics)
	r.Start()
	enqueue(t, db, "a", "a1")
	r.Nudge()
	r.Nudge()
	require.Eventually(t, func() bool { return len(p.messages()) == 1 }, 5*time.Second, 10*time.Millisecond,
		"a nudge makes the relay look at once, without waiting for the hourly interval")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, r.Stop(ctx))
	require.NoError(t, r.Stop(ctx), "Stop may be called more than once")

	enqueue(t, db, "a", "a2")
	r.Nudge()
	assert.Equal(t, 1, pendingCount(t, db), "a stopped relay leaves the messages for another one")
}
