package delisting

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"
)

// batch bounds the steps that one run applies, so that a backlog is worked
// off in several runs and the run stops soon after Stop.
const batch = 100

// Scheduler applies the steps of delistings that are due. Every instance
// runs one; Advance makes each step apply exactly once.
type Scheduler struct {
	db       *sql.DB
	interval time.Duration
	// after runs after each run that committed changes: it drops the cached
	// currency list and nudges the outbox relay.
	after func(ctx context.Context, changes []Change)
	now   func() time.Time

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// NewScheduler returns a scheduler that runs every interval.
func NewScheduler(db *sql.DB, interval time.Duration, after func(ctx context.Context, changes []Change)) *Scheduler {
	return &Scheduler{db: db, interval: interval, after: after, now: time.Now,
		stop: make(chan struct{}), done: make(chan struct{})}
}

// Start runs the scheduler until Stop.
func (s *Scheduler) Start() {
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			s.run()
			select {
			case <-s.stop:
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Scheduler) run() {
	ctx, cancel := context.WithTimeout(context.Background(), s.interval+30*time.Second)
	defer cancel()
	for {
		changes, err := Advance(ctx, s.db, s.now(), batch)
		if len(changes) > 0 && s.after != nil {
			s.after(ctx, changes)
		}
		if err != nil {
			log.Printf("Error applying the delisting steps that are due; the next run retries: %v", err)
			return
		}
		select {
		case <-s.stop:
			return
		default:
		}
		if len(changes) < batch {
			return
		}
	}
}

// Stop stops the scheduler and waits for its run to end, or for ctx.
func (s *Scheduler) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() { close(s.stop) })
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
