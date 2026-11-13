package ledger

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshot_RecordsEachVersionOnce(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	alice, bob, usdt := b.user(), b.user(), b.currency("USDT")
	b.fund(alice, usdt, "10")
	b.fund(bob, usdt, "20")

	n, err := b.Snapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	n, err = b.Snapshot(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "no balance changed since")

	_, err = b.Lock(ctx, "lock:1", alice, amount(usdt, "4"), Ref{})
	require.NoError(t, err)
	n, err = b.Snapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "only the balance that changed")

	var available, locked, total string
	var version int
	require.NoError(t, b.DB.QueryRow(`SELECT available::text, locked::text, total::text, version FROM balance_snapshots
		WHERE user_id = $1 ORDER BY version DESC LIMIT 1`, alice.UserHK).Scan(&available, &locked, &total, &version))
	assert.Equal(t, []string{"6", "4", "10"}, []string{available, locked, total})
	assert.Equal(t, 4, version, "opened, detected, credited, locked")

	_, err = b.DB.Exec(`UPDATE balance_snapshots SET available = 0`)
	require.Error(t, err, "snapshots are append-only")
}

func TestAudit_RunsOnOneInstanceAtATime(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	alice, usdt := b.user(), b.currency("USDT")
	b.fund(alice, usdt, "10")

	other, err := b.DB.Conn(ctx)
	require.NoError(t, err)
	defer other.Close()
	var held bool
	require.NoError(t, other.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, auditLockID).Scan(&held))
	require.True(t, held)
	_, _, ran, err := b.Audit(ctx, nil)
	require.NoError(t, err)
	assert.False(t, ran, "another instance audits")

	_, err = other.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, auditLockID)
	require.NoError(t, err)
	report, snapshots, ran, err := b.Audit(ctx, nil)
	require.NoError(t, err)
	assert.True(t, ran)
	assert.True(t, report.OK())
	assert.Equal(t, 1, report.Balances)
	assert.Equal(t, int64(1), snapshots)
}

// TestReconcile_DuringOperationsFindsNothing reconciles while transfers
// and locks go on: one snapshot of the books never shows an operation half
// done, so no reconciliation reports a difference.
func TestReconcile_DuringOperationsFindsNothing(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	alice, bob, usdt := b.user(), b.user(), b.currency("USDT")
	b.fund(alice, usdt, "1000")
	b.fund(bob, usdt, "1000")

	done := make(chan struct{})
	operations := make(chan error, 1)
	go func() {
		defer close(operations)
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			from, to := alice, bob
			if i%2 == 1 {
				from, to = bob, alice
			}
			if _, err := b.Transfer(ctx, fmt.Sprintf("during:%d", i), from, to, amount(usdt, "0.01"), Ref{}); err != nil {
				operations <- err
				return
			}
			if _, err := b.Lock(ctx, fmt.Sprintf("during-lock:%d", i), from, amount(usdt, "0.01"), Ref{}); err != nil {
				operations <- err
				return
			}
		}
	}()
	for range 20 {
		report, err := b.Reconcile(ctx, nil)
		require.NoError(t, err)
		assert.True(t, report.OK(), "%+v", report)
		_, _, ran, err := b.Audit(ctx, nil)
		require.NoError(t, err)
		assert.True(t, ran)
	}
	close(done)
	require.NoError(t, <-operations)
}
