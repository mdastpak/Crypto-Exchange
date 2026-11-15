package ledger

import (
	"context"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/metrics"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLedger_RefusesABalanceChangedOutsideIt(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	sealer := sealtest.Use(t, b.DB, rowseal.ModeEnforce)
	alice, usdt := b.user(), b.currency("USDT")
	b.fund(alice, usdt, "100")
	_, err := b.Lock(ctx, "lock:1", alice, amount(usdt, "10"), Ref{})
	require.NoError(t, err, "the ledger seals every balance it writes")
	balanceFindings := func() int {
		var n int
		require.NoError(t, b.DB.QueryRow(`SELECT COUNT(*) FROM integrity_findings WHERE table_name = 'balances'`).Scan(&n))
		return n
	}
	// The user rows of this test are written in SQL, without seals; the
	// balances have theirs.
	_, err = sealer.VerifyAll(ctx)
	require.NoError(t, err)
	assert.Zero(t, balanceFindings(), "the verifier checks the balances too")

	_, err = b.DB.Exec(`UPDATE balances SET available = available + 1000`)
	require.NoError(t, err)
	_, err = b.Lock(ctx, "lock:2", alice, amount(usdt, "500"), Ref{})
	require.ErrorIs(t, err, ErrUnverified, "the extra funds cannot be moved")
	assert.Equal(t, 1, balanceFindings())

	// The reconciliation finds it too, from the changes.
	books, err := b.Reconcile(ctx, nil)
	require.NoError(t, err)
	assert.Len(t, books.Mismatches, 1)
}

func TestLedger_CountsItsOperations(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	alice, usdt := b.user(), b.currency("USDT")
	b.fund(alice, usdt, "1")
	count := func(kind, outcome string) float64 {
		return testutil.ToFloat64(metrics.LedgerOperations.WithLabelValues(kind, outcome))
	}
	applied, replayed, insufficient := count(KindLock, "applied"), count(KindLock, "replayed"), count(KindLock, "insufficient")

	_, err := b.Lock(ctx, "count:1", alice, amount(usdt, "1"), Ref{})
	require.NoError(t, err)
	_, err = b.Lock(ctx, "count:1", alice, amount(usdt, "1"), Ref{})
	require.NoError(t, err)
	_, err = b.Lock(ctx, "count:2", alice, amount(usdt, "1"), Ref{})
	require.ErrorIs(t, err, ErrInsufficientBalance)

	assert.Equal(t, applied+1, count(KindLock, "applied"))
	assert.Equal(t, replayed+1, count(KindLock, "replayed"))
	assert.Equal(t, insufficient+1, count(KindLock, "insufficient"))
}

// TestLedger_ReportModeDoesNotAdoptABalance checks that a balance changed
// in SQL cannot move in report mode either, and that the verifier keeps
// its finding open instead of sealing it again (#404): balances are sealed
// from their first write, so a mismatch is never a row of before.
func TestLedger_ReportModeDoesNotAdoptABalance(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	sealer := sealtest.Use(t, b.DB, rowseal.ModeReport)
	alice, usdt := b.user(), b.currency("USDT")
	b.fund(alice, usdt, "100")
	_, err := b.DB.Exec(`UPDATE balances SET available = available + 1000`)
	require.NoError(t, err)

	report, err := sealer.VerifyAll(ctx)
	require.NoError(t, err)
	var open, closed int
	require.NoError(t, b.DB.QueryRow(`SELECT COUNT(*) FILTER (WHERE resolved_at IS NULL), COUNT(*) FILTER (WHERE resolved_at IS NOT NULL)
		FROM integrity_findings WHERE table_name = 'balances'`).Scan(&open, &closed))
	assert.Equal(t, 1, open, "the finding stays open")
	assert.Zero(t, closed)
	assert.Positive(t, report.Resealed, "the user rows of the test, written in SQL, are adopted")

	_, err = b.Lock(ctx, "report:lock", alice, amount(usdt, "500"), Ref{})
	require.ErrorIs(t, err, ErrUnverified, "the extra funds cannot be locked")
	bob := b.user()
	_, err = b.Transfer(ctx, "report:transfer", alice, bob, amount(usdt, "50"), Ref{})
	require.ErrorIs(t, err, ErrUnverified, "nor moved")
}
