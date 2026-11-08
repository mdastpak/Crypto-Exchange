package source

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// ErrBudgetExhausted is returned when the plan of a source has no request
// left this month. The run fails, and the job backs off until the month
// turns or the limit is raised.
var ErrBudgetExhausted = errors.New("source: the monthly requests of the plan are used up")

// UsageLimiter keeps the requests of a source within the limits of its
// plan across every worker, by counting them in market_source_usage. The
// counts use the database's clock and its windows: the calendar minute and
// month in UTC.
type UsageLimiter struct {
	db     *sql.DB
	source string
	// PerMinute and PerMonth are the limits; 0 is no limit of that kind.
	perMinute, perMonth int
	sleep               func(ctx context.Context, d time.Duration) error
}

// NewUsageLimiter returns the limiter of a source. It returns a nil
// Limiter when the plan has no limit, so that the client makes no count.
func NewUsageLimiter(db *sql.DB, source string, perMinute, perMonth int) Limiter {
	if perMinute <= 0 && perMonth <= 0 {
		return nil
	}
	return &UsageLimiter{db: db, source: source, perMinute: perMinute, perMonth: perMonth, sleep: sleepCtx}
}

// Acquire counts a request in the minute, waiting for the next minute when
// this one is full, and then in the month.
func (l *UsageLimiter) Acquire(ctx context.Context) error {
	if l.perMinute > 0 {
		for {
			n, left, err := l.count(ctx, "minute")
			if err != nil {
				return err
			}
			if n <= l.perMinute {
				break
			}
			// The workers that wait for the next minute start a little
			// apart.
			wait := left + time.Duration(rand.Int64N(int64(time.Second))) //nolint:gosec // G404: jitter needs no secret randomness.
			if err := l.sleep(ctx, wait); err != nil {
				return err
			}
		}
	}
	if l.perMonth > 0 {
		n, _, err := l.count(ctx, "month")
		if err != nil {
			return err
		}
		if n > l.perMonth {
			return ErrBudgetExhausted
		}
	}
	return nil
}

// count adds a request to the current window of kind and returns the
// requests of the window, with this one, and the time left in it. A
// request in a new window starts its count again.
func (l *UsageLimiter) count(ctx context.Context, kind string) (int, time.Duration, error) {
	var n int
	var left float64
	err := l.db.QueryRowContext(ctx, `
		INSERT INTO market_source_usage AS u (source, window_kind, window_start, requests)
		VALUES ($1, $2, date_trunc($2, NOW() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', 1)
		ON CONFLICT (source, window_kind) DO UPDATE SET
			requests = CASE WHEN u.window_start = excluded.window_start THEN u.requests + 1 ELSE 1 END,
			window_start = excluded.window_start
		RETURNING u.requests, EXTRACT(EPOCH FROM u.window_start + ('1 ' || $2)::interval - NOW())::float8`,
		l.source, kind).Scan(&n, &left)
	if err != nil {
		return 0, 0, fmt.Errorf("source %s: counting the request: %w", l.source, err)
	}
	return n, time.Duration(left * float64(time.Second)), nil
}
