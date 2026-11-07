package outbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
)

// Stats is the state of the outbox.
type Stats struct {
	// Pending counts the messages that wait to be published, and
	// OldestPending is the age of the oldest of them, zero without one.
	Pending       int
	OldestPending time.Duration
	// Failed counts the dead letters.
	Failed int
}

// ReadStats reads the state of the outbox.
func ReadStats(ctx context.Context, db *sql.DB) (Stats, error) {
	var s Stats
	var oldest float64
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FILTER (WHERE failed_at IS NULL),
			COALESCE(EXTRACT(EPOCH FROM NOW() - MIN(created_at) FILTER (WHERE failed_at IS NULL)), 0),
			COUNT(*) FILTER (WHERE failed_at IS NOT NULL)
		FROM outbox_events WHERE published_at IS NULL`).Scan(&s.Pending, &oldest, &s.Failed)
	s.OldestPending = time.Duration(oldest * float64(time.Second))
	return s, err
}

// report logs the state of the outbox when a message has waited long or a
// dead letter exists. Only the instance that holds the relay lock reports,
// so that the instances do not all log the same.
func (r *Relay) report(ctx context.Context) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var locked bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, relayLockID).Scan(&locked); err != nil || !locked {
		return err
	}
	s, err := ReadStats(ctx, r.db)
	if err != nil {
		return err
	}
	if s.Failed > 0 || s.OldestPending >= reportAge {
		r.logger.Printf("WARNING: Outbox: %d messages pending, the oldest for %v; %d dead letters. Inspect them with: admin outbox list",
			s.Pending, s.OldestPending.Round(time.Second), s.Failed)
	}
	return nil
}

// DeadLetter is a message that was set aside after its attempts.
type DeadLetter struct {
	ID        int64
	Topic     string
	Payload   string
	Attempts  int
	LastError string
	CreatedAt time.Time
	FailedAt  time.Time
}

// ErrNotDeadLetter is returned for an id that is not a dead letter.
var ErrNotDeadLetter = errors.New("no dead letter has this id")

// ListDeadLetters returns up to limit dead letters, oldest first.
func ListDeadLetters(ctx context.Context, db *sql.DB, limit int) ([]DeadLetter, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, topic, payload::text, attempts, COALESCE(last_error, ''), created_at, failed_at
		FROM outbox_events WHERE failed_at IS NOT NULL AND published_at IS NULL ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var letters []DeadLetter
	for rows.Next() {
		var d DeadLetter
		if err := rows.Scan(&d.ID, &d.Topic, &d.Payload, &d.Attempts, &d.LastError, &d.CreatedAt, &d.FailedAt); err != nil {
			return nil, err
		}
		letters = append(letters, d)
	}
	return letters, rows.Err()
}

// RetryDeadLetter makes a dead letter pending again, with all its attempts.
// It is published after the messages of its topic that were published
// while it was set aside, so out of order. origin is who retries it, which
// is recorded in the audit log (#195).
func RetryDeadLetter(ctx context.Context, db *sql.DB, id int64, origin audit.Event) error {
	return changeDeadLetter(ctx, db, id, audit.ActionOutboxRetry, origin,
		`UPDATE outbox_events SET failed_at = NULL, attempts = 0, next_attempt_at = NOW()
		WHERE id = $1 AND failed_at IS NOT NULL AND published_at IS NULL RETURNING topic`)
}

// DiscardDeadLetter deletes a dead letter, which is then never published.
// The audit event keeps its topic.
func DiscardDeadLetter(ctx context.Context, db *sql.DB, id int64, origin audit.Event) error {
	return changeDeadLetter(ctx, db, id, audit.ActionOutboxDiscard, origin,
		`DELETE FROM outbox_events WHERE id = $1 AND failed_at IS NOT NULL AND published_at IS NULL RETURNING topic`)
}

// changeDeadLetter runs query, which returns the topic of the message it
// changed, and records action in the same transaction.
func changeDeadLetter(ctx context.Context, db *sql.DB, id int64, action string, origin audit.Event, query string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var topic string
	err = tx.QueryRowContext(ctx, query, id).Scan(&topic)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("message %d: %w", id, ErrNotDeadLetter)
	}
	if err != nil {
		return err
	}
	e := audit.Of(origin, action, audit.TargetOutboxMessage, strconv.FormatInt(id, 10))
	e.Changes = &audit.Changes{Before: map[string]interface{}{"topic": topic}}
	if err := audit.Record(ctx, tx, e); err != nil {
		return fmt.Errorf("recording %s of message %d: %w", action, id, err)
	}
	return tx.Commit()
}
