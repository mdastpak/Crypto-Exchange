package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLedger_InTheCallersTransaction checks Ledger.In: an entry written in
// the caller's transaction commits and rolls back with it, keeps its
// idempotency key, and counts in Withdrawn while it holds funds.
func TestLedger_InTheCallersTransaction(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	alice, usdt := b.user(), b.currency("USDT")
	b.fund(alice, usdt, "100")
	withdrawn := func() string {
		t.Helper()
		w, err := Withdrawn(ctx, b.DB, alice.UserHK, usdt, 24*time.Hour)
		require.NoError(t, err)
		return w.String()
	}

	// Rolled back with the caller: nothing of the entry stays.
	tx, err := b.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = b.In(tx).Lock(ctx, "in:1", alice, amount(usdt, "30"), Ref{Type: RefWithdrawal, ID: "w1"})
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	assert.Equal(t, [3]string{"100", "0", "0"}, b.components(alice, usdt))
	var entries int
	require.NoError(t, b.DB.QueryRow(`SELECT COUNT(*) FROM ledger_entries WHERE idempotency_key = 'in:1'`).Scan(&entries))
	assert.Zero(t, entries)
	assert.Equal(t, "0", withdrawn())

	// Committed by the caller, and replayed by a retry.
	tx, err = b.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	first, err := b.In(tx).Lock(ctx, "in:2", alice, amount(usdt, "30"), Ref{Type: RefWithdrawal, ID: "w2"})
	require.NoError(t, err)
	assert.False(t, first.Replayed)
	require.NoError(t, tx.Commit())
	assert.Equal(t, [3]string{"70", "30", "0"}, b.components(alice, usdt))
	assert.Equal(t, "30", withdrawn())

	tx, err = b.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	retry, err := b.In(tx).Lock(ctx, "in:2", alice, amount(usdt, "30"), Ref{Type: RefWithdrawal, ID: "w2"})
	require.NoError(t, err)
	assert.True(t, retry.Replayed)
	assert.Equal(t, first.EntryID, retry.EntryID)
	_, err = b.In(tx).Lock(ctx, "in:2", alice, amount(usdt, "31"), Ref{Type: RefWithdrawal, ID: "w2"})
	require.ErrorIs(t, err, ErrIdempotencyConflict, "the same key with another request")
	require.NoError(t, tx.Rollback())

	// A release gives the amount back to the user and to the limit.
	tx, err = b.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = b.In(tx).Release(ctx, "in:3", alice, amount(usdt, "30"), Ref{Type: RefWithdrawal, ID: "w2"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	assert.Equal(t, [3]string{"100", "0", "0"}, b.components(alice, usdt))
	assert.Equal(t, "0", withdrawn())

	// An invalid request is refused before anything is written.
	tx, err = b.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = b.In(tx).Lock(ctx, "", alice, amount(usdt, "1"), Ref{})
	require.ErrorIs(t, err, ErrInvalidRequest)
	require.NoError(t, tx.Rollback())

	b.reconcile(usdt)
}
