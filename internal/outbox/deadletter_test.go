package outbox

import (
	"bytes"
	"context"
	"errors"
	"log"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flushTimes flushes n times, and ends the backoff after each, so that
// every pending message is tried n times.
func flushTimes(t *testing.T, r *Relay, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		_, err := r.Flush(context.Background())
		require.NoError(t, err)
		makeReady(t, r.db)
	}
}

// A message that always fails becomes a dead letter after the attempts of
// its route, and the later messages of its topic are published (#192).
func TestFlush_AMessageThatAlwaysFailsBecomesADeadLetter(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	ctx := context.Background()
	enqueue(t, db, "a", "poison", "a2", "a3")
	p := &publisher{}
	p.failOn("poison", errors.New("refused by the broker"))
	var logs bytes.Buffer
	r := NewRelay(db, p, twoTopics, Options{Interval: time.Hour, MaxAttempts: 3, Logger: log.New(&logs, "", 0)})

	flushTimes(t, r, 2)
	assert.Empty(t, p.messages(), "the topic waits while the message has attempts left")

	flushTimes(t, r, 1)
	assert.Equal(t, []string{"ex-a:a2", "ex-a:a3"}, p.messages(), "the topic goes on without the dead letter, in the same flush")
	assert.Contains(t, logs.String(), "dead letter")

	letters, err := ListDeadLetters(ctx, db, 10)
	require.NoError(t, err)
	require.Len(t, letters, 1)
	assert.Equal(t, "a", letters[0].Topic)
	assert.Equal(t, 3, letters[0].Attempts)
	assert.Equal(t, "refused by the broker", letters[0].LastError)
	assert.JSONEq(t, `{"n": "poison"}`, letters[0].Payload)

	stats, err := ReadStats(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, Stats{Failed: 1}, stats)

	// A dead letter is not tried again by itself.
	flushTimes(t, r, 2)
	assert.Equal(t, []string{"ex-a:a2", "ex-a:a3"}, p.messages())

	// Retried once the cause is fixed, it is published.
	p.failOn("none", nil)
	require.NoError(t, RetryDeadLetter(ctx, db, letters[0].ID))
	flushTimes(t, r, 1)
	assert.Equal(t, []string{"ex-a:a2", "ex-a:a3", "ex-a:poison"}, p.messages())
	assert.ErrorIs(t, RetryDeadLetter(ctx, db, letters[0].ID), ErrNotDeadLetter, "a published message is no dead letter")
}

func TestDiscardDeadLetter(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	ctx := context.Background()
	enqueue(t, db, "a", "poison")
	p := &publisher{}
	p.failOn("poison", errors.New("refused"))
	r := NewRelay(db, p, twoTopics, Options{Interval: time.Hour, MaxAttempts: 1, Logger: quiet})
	flushTimes(t, r, 1)

	letters, err := ListDeadLetters(ctx, db, 10)
	require.NoError(t, err)
	require.Len(t, letters, 1)
	require.NoError(t, DiscardDeadLetter(ctx, db, letters[0].ID))
	assert.ErrorIs(t, DiscardDeadLetter(ctx, db, letters[0].ID), ErrNotDeadLetter)
	var rows int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM outbox_events`).Scan(&rows))
	assert.Zero(t, rows)
}

// A route that never gives up keeps its topic in order instead.
func TestFlush_ARouteMayNeverGiveUp(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	enqueue(t, db, "a", "poison", "a2")
	p := &publisher{}
	p.failOn("poison", errors.New("refused"))
	routes := map[string]Route{"a": {Exchange: "ex-a", MaxAttempts: -1}}
	r := NewRelay(db, p, routes, Options{Interval: time.Hour, MaxAttempts: 2, Logger: quiet})

	flushTimes(t, r, 5)
	assert.Empty(t, p.messages(), "the topic waits for its first message")
	letters, err := ListDeadLetters(context.Background(), db, 10)
	require.NoError(t, err)
	assert.Empty(t, letters)
}

// A topic with more waiting messages than a batch does not hold back the
// other topics (#192): before, the batch read the oldest messages of every
// topic together, and a blocked topic could fill it.
func TestFlush_ABlockedTopicDoesNotStarveTheOthers(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	names := make([]string, 30)
	for i := range names {
		names[i] = "a" + string(rune('A'+i))
	}
	enqueue(t, db, "a", names...)
	enqueue(t, db, "b", "b1")
	p := &publisher{}
	p.failOn(names[0], errors.New("broker refuses topic a"))
	r := NewRelay(db, p, twoTopics, Options{Batch: 10, Interval: time.Hour, Logger: quiet})

	_, err := r.Flush(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"ex-b:b1"}, p.messages())
}

func TestReadStats_ReportsTheOldestPendingMessage(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	ctx := context.Background()
	stats, err := ReadStats(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, Stats{}, stats, "an empty outbox")

	enqueue(t, db, "a", "a1", "a2")
	_, err = db.Exec(`UPDATE outbox_events SET created_at = NOW() - INTERVAL '10 minutes' WHERE payload->>'n' = 'a1'`)
	require.NoError(t, err)
	stats, err = ReadStats(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, 2, stats.Pending)
	assert.InDelta(t, (10 * time.Minute).Seconds(), stats.OldestPending.Seconds(), 5)

	var logs bytes.Buffer
	r := NewRelay(db, &publisher{}, twoTopics, Options{Interval: time.Hour, Logger: log.New(&logs, "", 0)})
	require.NoError(t, r.report(ctx))
	assert.Contains(t, logs.String(), "2 messages pending")
}
