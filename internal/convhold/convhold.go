// Package convhold holds the balances whose forced conversion failed
// (#452), so that one balance that cannot convert does not stop the
// conversion of the others: a migration of currencies (#215) or the
// settlement of a delisting (#216).
//
// A conversion claims one balance at a time, the lowest id first. When the
// conversion of a balance fails, Hold records it in its own transaction,
// with a stable code of the error and a time to retry, which backs off from
// a minute to an hour. The claim skips a held balance until then (Skip), so
// the next run converts the others. A conversion that succeeds releases its
// hold in its transaction (Release). The completion of a migration and the
// delisted step still wait for every balance, the held ones included.
//
// The table is shared by every instance, and its times are the database's,
// so the instances agree on when to retry.
package convhold

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/metrics"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// The kinds of conversions, as the reference types of their ledger
// entries.
const (
	KindMigration = "currency_migration"
	KindDelisting = "currency_delisting"
)

// The codes of the errors of a held balance.
const (
	// CodeRowUnverified is a balance, or a row that the conversion relies
	// on, that fails its seal (#139).
	CodeRowUnverified = "row_unverified"
	// CodeInvalidRequest is a conversion that the ledger refuses as
	// malformed, such as of a sub-account that is deleted or not the
	// user's.
	CodeInvalidRequest = "invalid_request"
	// CodeInsufficientBalance is a balance spent meanwhile.
	CodeInsufficientBalance = "insufficient_balance"
	// CodeIdempotencyConflict is a key used for another entry.
	CodeIdempotencyConflict = "idempotency_conflict"
	// CodeFailed is any other error.
	CodeFailed = "conversion_failed"
)

// The backoff of a held balance: a minute after the first failure,
// doubling to an hour.
const (
	FirstRetry = time.Minute
	LastRetry  = time.Hour
)

// Code returns the code of err.
func Code(err error) string {
	switch {
	case errors.Is(err, rowseal.ErrUnverified):
		return CodeRowUnverified
	case errors.Is(err, ledger.ErrInvalidRequest):
		return CodeInvalidRequest
	case errors.Is(err, ledger.ErrInsufficientBalance), errors.Is(err, ledger.ErrInsufficientLocked):
		return CodeInsufficientBalance
	case errors.Is(err, ledger.ErrIdempotencyConflict):
		return CodeIdempotencyConflict
	}
	return CodeFailed
}

// Backoff returns the wait after attempts failures.
func Backoff(attempts int) time.Duration {
	wait := FirstRetry
	for i := 1; i < attempts && wait < LastRetry; i++ {
		wait *= 2
	}
	return min(wait, LastRetry)
}

// Hold records that the conversion of a balance of currencyHK by the
// migration or delisting ref of kind failed with cause, in a transaction of
// its own, and returns the attempts so far. The failure is counted and
// logged once: the claim skips the balance until its retry.
func Hold(ctx context.Context, db *sql.DB, kind, ref, balanceID, currencyHK string, cause error) (int, error) {
	code := Code(cause)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var attempts int
	// The wait is computed in SQL from the attempts, as Backoff does, so
	// that concurrent writes agree.
	err := db.QueryRowContext(ctx, `
		INSERT INTO currency_conversion_holds (reference_type, reference_id, balance_id, currency_hk, last_error_code, retry_after)
		VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6))
		ON CONFLICT (reference_type, reference_id, balance_id) DO UPDATE SET
			attempts = CASE WHEN currency_conversion_holds.released_at IS NULL THEN currency_conversion_holds.attempts + 1 ELSE 1 END,
			last_error_code = EXCLUDED.last_error_code, updated_at = now(), released_at = NULL,
			retry_after = now() + make_interval(secs => LEAST($7::float8,
				$6::float8 * power(2, CASE WHEN currency_conversion_holds.released_at IS NULL THEN currency_conversion_holds.attempts ELSE 0 END)))
		RETURNING attempts`,
		kind, ref, balanceID, currencyHK, code, FirstRetry.Seconds(), LastRetry.Seconds()).Scan(&attempts)
	if err != nil {
		return 0, fmt.Errorf("failed to hold balance %s: %w", balanceID, err)
	}
	metrics.ConversionFailures.WithLabelValues(kind, code).Inc()
	log.Printf("The conversion of balance %s by %s %s failed (%s, attempt %d); it is retried in %s: %v",
		balanceID, kind, ref, code, attempts, Backoff(attempts), cause)
	return attempts, nil
}

// Release releases the hold of a balance that converted, in tx, the
// transaction of its conversion.
func Release(ctx context.Context, tx *sql.Tx, kind, ref, balanceID string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE currency_conversion_holds SET released_at = now(), updated_at = now()
		WHERE reference_type = $1 AND reference_id = $2 AND balance_id = $3 AND released_at IS NULL`,
		kind, ref, balanceID); err != nil {
		return fmt.Errorf("failed to release the hold of balance %s: %w", balanceID, err)
	}
	return nil
}

// Skip is the condition of a claim that skips the balances held until
// later. %[1]s is the kind, as a literal, %[2]s the column of the
// reference, and %[3]s the column of the balance.
const Skip = `NOT EXISTS (SELECT 1 FROM currency_conversion_holds hold
	WHERE hold.reference_type = '%[1]s' AND hold.reference_id = %[2]s AND hold.balance_id = %[3]s
	AND hold.released_at IS NULL AND hold.retry_after > now())`

// Blocked is the balances held of a conversion, or of one currency of it.
type Blocked struct {
	// Balances is how many balances are held, and Codes how many of them
	// failed with each code.
	Balances int            `json:"balances"`
	Codes    map[string]int `json:"codes,omitempty"`
}

// Summary returns the balances held of the migration or delisting ref of
// kind, by currency.
func Summary(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, kind, ref string) (map[string]Blocked, error) {
	rows, err := q.QueryContext(ctx, `SELECT currency_hk::text, last_error_code, count(*) FROM currency_conversion_holds
		WHERE reference_type = $1 AND reference_id = $2 AND released_at IS NULL
		GROUP BY currency_hk, last_error_code`, kind, ref)
	if err != nil {
		return nil, fmt.Errorf("failed to read the held balances: %w", err)
	}
	defer rows.Close()
	out := map[string]Blocked{}
	for rows.Next() {
		var currency, code string
		var n int
		if err := rows.Scan(&currency, &code, &n); err != nil {
			return nil, fmt.Errorf("failed to read the held balances: %w", err)
		}
		b := out[currency]
		if b.Codes == nil {
			b.Codes = map[string]int{}
		}
		b.Balances += n
		b.Codes[code] += n
		out[currency] = b
	}
	return out, rows.Err()
}

// Retry makes the held balances of the migration or delisting ref of kind
// due now, for the next run of the scheduler, such as after a re-seal, and
// returns how many. An administrator's retry is audited on target, the
// currency of the route.
func Retry(ctx context.Context, db *sql.DB, kind, ref, target string, origin audit.Event) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to start the retry: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE currency_conversion_holds SET retry_after = now(), updated_at = now()
		WHERE reference_type = $1 AND reference_id = $2 AND released_at IS NULL`, kind, ref)
	if err != nil {
		return 0, fmt.Errorf("failed to retry the held balances: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	e := audit.Of(origin, audit.ActionCurrencyConversionRetry, audit.TargetCurrency, target)
	e.Changes = &audit.Changes{After: map[string]any{"reference_type": kind, "reference_id": ref, "balances": n}}
	if err := audit.Record(ctx, tx, e); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to retry the held balances: %w", err)
	}
	return int(n), nil
}
