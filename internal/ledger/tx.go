package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/balance"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// RefWithdrawal is the type of the reference of the entries of a
// withdrawal: the lock of its funds at the request, their release when it
// is cancelled or fails, and the withdrawal itself. Withdrawn counts the
// requests by it.
const RefWithdrawal = "withdrawal"

// Tx is the ledger within a transaction of the caller, for an operation
// that must commit together with other writes, or after checks that must
// hold until it commits: a withdrawal request, its row, and the lock of its
// funds, after the check of the user's daily limit (#157, #70). The caller
// commits or rolls back. After an error, the transaction must be rolled
// back.
type Tx struct {
	tx *sql.Tx
}

// In returns the ledger within tx.
func (l Ledger) In(tx *sql.Tx) Tx { return Tx{tx: tx} }

// Lock is Ledger.Lock within the transaction.
func (t Tx) Lock(ctx context.Context, key string, a Account, amount Amount, ref Ref) (Result, error) {
	return t.record(ctx, entry{kind: KindLock, key: key, ref: ref,
		moves: []move{{a, amount.Currency, balance.BalanceChangeLock, amount.Value}},
	})
}

// Release is Ledger.Release within the transaction.
func (t Tx) Release(ctx context.Context, key string, a Account, amount Amount, ref Ref) (Result, error) {
	return t.record(ctx, entry{kind: KindRelease, key: key, ref: ref,
		moves: []move{{a, amount.Currency, balance.BalanceChangeUnlock, amount.Value}},
	})
}

func (t Tx) record(ctx context.Context, e entry) (Result, error) {
	start := time.Now()
	res, err := func() (Result, error) {
		if err := e.validate(); err != nil {
			return Result{}, err
		}
		return write(ctx, t.tx, e)
	}()
	observe(e.kind, start, res, err)
	return res, err
}

// Withdrawn returns how much of currency the user has requested to
// withdraw in the window before now, in every account of the user: the
// funds locked for withdrawals (RefWithdrawal) that the window holds,
// less what was released of them, since a cancelled or failed withdrawal
// does not count. The fee of a withdrawal counts, as it is locked with it.
// The daily withdrawal limits (#157) read it, in the transaction of the
// request, after a lock that keeps two requests of the user from counting
// at once.
func Withdrawn(ctx context.Context, q querier, user, currency shared.UUID, window time.Duration) (shared.Decimal, error) {
	var total shared.Decimal
	// The user's locks and releases of the currency in the window come
	// from the partial index balance_changes_user_locks (migration 21), so
	// the query reads the user's recent locks only, whatever the volume of
	// the exchange. A release comes after its lock, so it is in the window
	// too.
	err := q.QueryRowContext(ctx, `WITH changes AS (
			SELECT e.kind, e.reference_id, c.amount
			FROM balance_changes c JOIN ledger_entries e ON e.id = c.entry_id
			WHERE c.user_id = $1 AND c.currency_hk = $2 AND c.change_type IN ('LOCK', 'UNLOCK')
				AND c."timestamp" > NOW() - make_interval(secs => $4)
				AND e.reference_type = $3 AND e.kind IN ('lock', 'release')),
		locks AS (
			SELECT reference_id, SUM(amount) AS amount FROM changes WHERE kind = 'lock' GROUP BY reference_id),
		releases AS (
			SELECT reference_id, SUM(amount) AS amount FROM changes WHERE kind = 'release' GROUP BY reference_id)
		SELECT COALESCE(SUM(GREATEST(l.amount - COALESCE(r.amount, 0), 0)), 0)
		FROM locks l LEFT JOIN releases r USING (reference_id)`,
		user, currency, RefWithdrawal, window.Seconds()).Scan(&total)
	if err != nil {
		return shared.Decimal{}, fmt.Errorf("sum the withdrawals of %s in %s: %w", user, currency, err)
	}
	return total, nil
}
