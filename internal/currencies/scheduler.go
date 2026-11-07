package currencies

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

// Change is a committed change of a currency by a scheduled step, which the
// caller follows by dropping the cached currency list and nudging the outbox
// relay.
type Change struct {
	CurrencyHK string
	// Public reports whether the change is in the public view, and so has
	// an event in the outbox.
	Public bool
}

// Advancer applies the scheduled steps that are due at now, each exactly
// once on all instances, at most limit of them, and returns the changes it
// committed: the steps of delistings (#216) and of migrations (#215).
type Advancer func(ctx context.Context, db *sql.DB, now time.Time, limit int) ([]Change, error)

// Scheduler applies the scheduled steps of currencies that are due. Every
// instance runs one; each Advancer makes each step apply exactly once.
type Scheduler struct {
	db       *sql.DB
	interval time.Duration
	advance  []Advancer
	// after runs after each run that committed changes: it drops the cached
	// currency list and nudges the outbox relay.
	after func(ctx context.Context, changes []Change)
	now   func() time.Time

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// NewScheduler returns a scheduler that runs the advancers every interval.
func NewScheduler(db *sql.DB, interval time.Duration, after func(ctx context.Context, changes []Change), advance ...Advancer) *Scheduler {
	return &Scheduler{db: db, interval: interval, after: after, advance: advance, now: time.Now,
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
	for _, advance := range s.advance {
		s.runOne(ctx, advance)
	}
}

func (s *Scheduler) runOne(ctx context.Context, advance Advancer) {
	for {
		changes, err := advance(ctx, s.db, s.now(), batch)
		if len(changes) > 0 && s.after != nil {
			s.after(ctx, changes)
		}
		if err != nil {
			log.Printf("Error applying the currency steps that are due; the next run retries: %v", err)
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
