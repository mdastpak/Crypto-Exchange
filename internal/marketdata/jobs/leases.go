package jobs

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// leases claims, renews, checks, and releases the rows of
// market_job_leases. Every comparison uses the database's clock, so that the
// clocks of the workers do not matter.
type leases struct {
	db *sql.DB
}

// ensure adds the row of a job, due now, if it does not exist.
func (l leases) ensure(ctx context.Context, job string) error {
	_, err := l.db.ExecContext(ctx, `INSERT INTO market_job_leases (job) VALUES ($1) ON CONFLICT (job) DO NOTHING`, job)
	return err
}

// claim takes the lease of a job that is due and that nobody holds, and
// returns its new token, or false when the job is not due or is held. Of
// several workers that claim at once, the row lock lets one win; the others
// find the lease taken.
func (l leases) claim(ctx context.Context, job, holder string, lease time.Duration) (int64, bool, error) {
	var token int64
	err := l.db.QueryRowContext(ctx, `
		UPDATE market_job_leases
		SET holder = $2, token = token + 1, lease_until = NOW() + make_interval(secs => $3),
			last_started_at = NOW()
		WHERE job = $1 AND lease_until < NOW() AND next_run_at <= NOW()
		RETURNING token`, job, holder, lease.Seconds()).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return token, err == nil, err
}

// renew extends the lease of the holder of token, and reports false when the
// lease is no longer its own.
func (l leases) renew(ctx context.Context, job string, token int64, lease time.Duration) (bool, error) {
	res, err := l.db.ExecContext(ctx, `
		UPDATE market_job_leases SET lease_until = NOW() + make_interval(secs => $3)
		WHERE job = $1 AND token = $2 AND lease_until > NOW()`, job, token, lease.Seconds())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// holds reports whether token still holds the lease of job.
func (l leases) holds(ctx context.Context, job string, token int64) (bool, error) {
	var ok bool
	err := l.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM market_job_leases WHERE job = $1 AND token = $2 AND lease_until > NOW())`,
		job, token).Scan(&ok)
	return ok, err
}

// outcome is how a run ended, which sets when the job is due again.
type outcome int

const (
	// succeeded: due again after the interval.
	succeeded outcome = iota
	// failed: due again after the backoff: the retry base after the first
	// failure in a row, doubled with each further one up to the interval,
	// with a jitter of ±20 % so that the workers do not retry a source in
	// step.
	failed
	// interrupted: the worker stopped; due at once, for another worker.
	interrupted
)

// release gives the lease up and schedules the next run. It changes nothing
// when token no longer holds the lease, since the job belongs to another
// worker then.
func (l leases) release(ctx context.Context, job string, token int64, how outcome, interval, retryBase time.Duration) error {
	var query string
	switch how {
	case succeeded:
		query = `UPDATE market_job_leases SET holder = '', lease_until = '-infinity', failures = 0,
			next_run_at = NOW() + make_interval(secs => $3), last_finished_at = NOW(), last_success_at = NOW()
			WHERE job = $1 AND token = $2`
	case failed:
		query = `UPDATE market_job_leases SET holder = '', lease_until = '-infinity', failures = failures + 1,
			next_run_at = NOW() + make_interval(secs =>
				LEAST($3, $4 * power(2, LEAST(failures, 20))) * (0.8 + random() * 0.4)),
			last_finished_at = NOW()
			WHERE job = $1 AND token = $2`
	default:
		query = `UPDATE market_job_leases SET holder = '', lease_until = '-infinity', next_run_at = NOW(),
			last_finished_at = NOW()
			WHERE job = $1 AND token = $2`
	}
	// Each query takes only the parameters it uses: PostgreSQL cannot type
	// an unused one.
	args := []any{job, token}
	switch how {
	case succeeded:
		args = append(args, interval.Seconds())
	case failed:
		args = append(args, interval.Seconds(), retryBase.Seconds())
	}
	_, err := l.db.ExecContext(ctx, query, args...)
	return err
}
