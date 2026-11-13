package ledger

import (
	"context"
	"database/sql"
	"fmt"
)

// Snapshot records, in balance_snapshots, every balance whose current
// version has no snapshot yet, and returns how many it recorded. A snapshot
// is the state of a balance at one version, for the records and for audits:
// it is never changed, and the balance is still the replay of its changes.
// Instances may run it at once: each version is recorded once.
func (l Ledger) Snapshot(ctx context.Context) (int64, error) {
	return snapshot(ctx, l.DB)
}

// snapshot is Snapshot in q.
func snapshot(ctx context.Context, q querier) (int64, error) {
	res, err := q.ExecContext(ctx, `INSERT INTO balance_snapshots
			(balance_id, user_id, sub_account_id, currency_hk, available, locked, pending_deposit, total, version)
		SELECT b.id, b.user_id, b.sub_account_id, b.currency_hk, b.available, b.locked, b.pending_deposit, b.total, b.version
		FROM balances b
		WHERE NOT EXISTS (SELECT 1 FROM balance_snapshots s WHERE s.balance_id = b.id AND s.version = b.version)
		ON CONFLICT (balance_id, version) DO NOTHING`)
	if err != nil {
		return 0, fmt.Errorf("record the snapshots: %w", err)
	}
	return res.RowsAffected()
}

// auditLockID is the advisory lock that lets one instance at a time audit
// the books.
const auditLockID int64 = 0x60_0001

// Audit takes the snapshots and reconciles the books, on one instance at a
// time: it returns ran false, and does nothing, while another holds the
// lock. It holds one connection, and the lock of its session, for both
// steps: the snapshots, in READ COMMITTED, since a snapshot of a balance
// that changes meanwhile is simply of its next version; then the
// reconciliation, in its REPEATABLE READ transaction.
func (l Ledger) Audit(ctx context.Context, holdings Holdings) (report Report, snapshots int64, ran bool, err error) {
	conn, err := l.DB.Conn(ctx)
	if err != nil {
		return Report{}, 0, false, err
	}
	defer conn.Close()
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, auditLockID).Scan(&ran); err != nil || !ran {
		return Report{}, 0, false, err
	}
	defer func() {
		// The lock belongs to this session; it ends with the connection
		// should the unlock fail.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, auditLockID)
	}()
	if snapshots, err = snapshot(ctx, conn); err != nil {
		return Report{}, 0, true, err
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return Report{}, snapshots, true, err
	}
	defer tx.Rollback()
	report, err = reconcile(ctx, tx, holdings)
	return report, snapshots, true, err
}
