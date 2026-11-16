package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The reproductions of #407: locked funds are held per reference, and a
// release, a withdrawal, or a trade ends only the hold that it names.
func TestHolds_EachMoveEndsTheHoldItNames(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	sealtest.Use(t, b.DB, rowseal.ModeEnforce)
	alice, usdt, btc := b.user(), b.currency("USDT"), b.currency("BTC")
	b.fund(alice, usdt, "1000")
	w1, w2 := Ref{Type: RefWithdrawal, ID: "w1"}, Ref{Type: RefWithdrawal, ID: "w2"}
	o1, o2 := Ref{Type: "order", ID: "o1"}, Ref{Type: "order", ID: "o2"}

	// A release with an unrelated reference does not free a pending
	// withdrawal's funds.
	_, err := b.Lock(ctx, "w1:lock", alice, amount(usdt, "100"), w1)
	require.NoError(t, err)
	_, err = b.Release(ctx, "o-unknown:release", alice, amount(usdt, "100"), Ref{Type: "order", ID: "o-unknown"})
	require.ErrorIs(t, err, ErrInsufficientLocked)
	_, err = b.Release(ctx, "nothing:release", alice, amount(usdt, "1"), Ref{})
	require.ErrorIs(t, err, ErrInsufficientLocked, "a release without a reference ends no hold")
	assert.Equal(t, [3]string{"900", "100", "0"}, b.components(alice, usdt))

	// A withdrawal is not completed from an order's lock.
	_, err = b.Lock(ctx, "o2:lock", alice, amount(usdt, "60"), o2)
	require.NoError(t, err)
	_, err = b.Withdraw(ctx, "w2:complete", alice, amount(usdt, "60"), dec("0"), w2)
	require.ErrorIs(t, err, ErrInsufficientLocked, "w2 never locked")

	// A hold cannot give more than it has left, in parts either.
	_, err = b.Withdraw(ctx, "w1:part1", alice, amount(usdt, "60"), dec("1"), w1)
	require.NoError(t, err)
	_, err = b.Withdraw(ctx, "w1:part2", alice, amount(usdt, "40"), dec("0"), w1)
	require.ErrorIs(t, err, ErrInsufficientLocked, "39 left of w1, though the balance locks 99")
	_, err = b.Release(ctx, "w1:release", alice, amount(usdt, "39"), w1)
	require.NoError(t, err)
	_, err = b.Release(ctx, "w1:release-again", alice, amount(usdt, "0.000001"), w1)
	require.ErrorIs(t, err, ErrInsufficientLocked, "w1 is ended")
	assert.Equal(t, [3]string{"879", "60", "0"}, b.components(alice, usdt), "o2's 60 is still locked")

	// A trade takes what it gives and its fee from its order's hold.
	_, err = b.Lock(ctx, "o1:lock", alice, amount(usdt, "33"), o1)
	require.NoError(t, err)
	_, err = b.SettleTrade(ctx, "t1", alice, Trade{Give: amount(usdt, "30"), Receive: amount(btc, "0.001"), Fee: amount(usdt, "4")}, o1)
	require.ErrorIs(t, err, ErrInsufficientLocked, "34 is more than o1 holds, though o2 holds 60 more")
	_, err = b.SettleTrade(ctx, "t1", alice, Trade{Give: amount(usdt, "30"), Receive: amount(btc, "0.001"), Fee: amount(usdt, "3")}, o1)
	require.NoError(t, err)
	assert.Equal(t, [3]string{"846", "60", "0"}, b.components(alice, usdt), "o1 ended, o2 untouched")

	// A fee in the received currency is refused, and never touches another
	// hold: here, 1 BTC locked for a withdrawal.
	b.fund(alice, btc, "1")
	_, err = b.Lock(ctx, "w3:lock", alice, amount(btc, "1"), Ref{Type: RefWithdrawal, ID: "w3"})
	require.NoError(t, err)
	_, err = b.Lock(ctx, "o3:lock", alice, amount(usdt, "10"), Ref{Type: "order", ID: "o3"})
	require.NoError(t, err)
	_, err = b.SettleTrade(ctx, "t3", alice, Trade{Give: amount(usdt, "10"), Receive: amount(btc, "0.0002"), Fee: amount(btc, "0.00001")},
		Ref{Type: "order", ID: "o3"})
	require.ErrorIs(t, err, ErrInvalidRequest)
	_, err = b.Withdraw(ctx, "w3:complete", alice, amount(btc, "0.99"), dec("0.01"), Ref{Type: RefWithdrawal, ID: "w3"})
	require.NoError(t, err, "the withdrawal's hold is whole")

	b.reconcile(usdt)
	b.reconcile(btc)
}

// Withdrawn, which the daily limits read, is what the withdrawal holds
// locked less what they released (#407): a hold cannot release what it
// did not lock, so the two never drift apart.
func TestHolds_WithdrawnIsTheWithdrawalHolds(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	alice, usdt := b.user(), b.currency("USDT")
	b.fund(alice, usdt, "1000")
	withdrawn := func() string {
		t.Helper()
		total, err := Withdrawn(ctx, b.DB, alice.UserHK, usdt, time.Hour)
		require.NoError(t, err)
		return total.String()
	}

	_, err := b.Lock(ctx, "w1:lock", alice, amount(usdt, "100"), Ref{Type: RefWithdrawal, ID: "w1"})
	require.NoError(t, err)
	_, err = b.Lock(ctx, "w2:lock", alice, amount(usdt, "50"), Ref{Type: RefWithdrawal, ID: "w2"})
	require.NoError(t, err)
	_, err = b.Lock(ctx, "o1:lock", alice, amount(usdt, "70"), Ref{Type: "order", ID: "o1"})
	require.NoError(t, err)
	assert.Equal(t, "150", withdrawn(), "orders do not count")

	// A release of w2 more than it holds, from the order's funds: refused.
	_, err = b.Release(ctx, "w2:release", alice, amount(usdt, "120"), Ref{Type: RefWithdrawal, ID: "w2"})
	require.ErrorIs(t, err, ErrInsufficientLocked)
	assert.Equal(t, "150", withdrawn())

	_, err = b.Release(ctx, "w2:release", alice, amount(usdt, "50"), Ref{Type: RefWithdrawal, ID: "w2"})
	require.NoError(t, err)
	_, err = b.Withdraw(ctx, "w1:complete", alice, amount(usdt, "99"), dec("1"), Ref{Type: RefWithdrawal, ID: "w1"})
	require.NoError(t, err)
	assert.Equal(t, "100", withdrawn(), "a completed withdrawal still counts; a released one does not")
	assert.Equal(t, [3]string{"830", "70", "0"}, b.components(alice, usdt), "the order still holds its 70")
}

// Two holds on one balance, ended at once from many goroutines (#407):
// each hold gives exactly what it locked, whatever the order, and the
// funds of one are never taken by the other.
func TestHolds_TwoHoldsOnOneBalanceAtOnce(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	alice, usdt := b.user(), b.currency("USDT")
	b.fund(alice, usdt, "100")
	a, w := Ref{Type: "order", ID: "a"}, Ref{Type: RefWithdrawal, ID: "w"}
	_, err := b.Lock(ctx, "a:lock", alice, amount(usdt, "30"), a)
	require.NoError(t, err)
	_, err = b.Lock(ctx, "w:lock", alice, amount(usdt, "50"), w)
	require.NoError(t, err)

	// 40 releases of 1 from a, and 40 withdrawals of 1 from w, at once.
	var released, withdrew, refused atomic.Int32
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := b.Release(ctx, fmt.Sprintf("a:release:%d", i), alice, amount(usdt, "1"), a)
			switch {
			case err == nil:
				released.Add(1)
			case errors.Is(err, ErrInsufficientLocked):
				refused.Add(1)
			default:
				assert.NoError(t, err)
			}
		}()
		go func() {
			defer wg.Done()
			_, err := b.Withdraw(ctx, fmt.Sprintf("w:withdraw:%d", i), alice, amount(usdt, "1"), dec("0"), w)
			switch {
			case err == nil:
				withdrew.Add(1)
			case errors.Is(err, ErrInsufficientLocked):
				refused.Add(1)
			default:
				assert.NoError(t, err)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(30), released.Load(), "a gives its 30 and no more")
	assert.Equal(t, int32(40), withdrew.Load(), "w has 50, enough for all 40")
	assert.Equal(t, int32(10), refused.Load())
	assert.Equal(t, [3]string{"50", "10", "0"}, b.components(alice, usdt), "w keeps its last 10")
	b.reconcile(usdt)
}

// A hold made up in SQL (#407): a lock change written for an order that
// never locked, to free another hold's funds through it. Its version comes
// after the balance's sealed version, since a version is unique per
// balance, so the ledger refuses to read the holds of that balance, in
// enforce mode, and the reconciliation reports it.
func TestHolds_AHoldWrittenInSQLIsRefused(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	sealtest.Use(t, b.DB, rowseal.ModeEnforce)
	alice, usdt := b.user(), b.currency("USDT")
	b.fund(alice, usdt, "100")
	_, err := b.Lock(ctx, "w:lock", alice, amount(usdt, "40"), Ref{Type: RefWithdrawal, ID: "w"})
	require.NoError(t, err)

	tx, err := b.DB.Begin()
	require.NoError(t, err)
	var entry string
	require.NoError(t, tx.QueryRow(`INSERT INTO ledger_entries (kind, idempotency_key, request_hash, reference_type, reference_id)
		VALUES ('lock', 'forged:lock', sha256('x'::bytea), 'order', 'forged') RETURNING id`).Scan(&entry))
	_, err = tx.Exec(`INSERT INTO balance_changes (entry_id, version, balance_id, user_id, currency_hk, change_type, amount,
			old_available, new_available, old_locked, new_locked, old_pending, new_pending)
		SELECT $1, version + 100, id, user_id, currency_hk, 'LOCK', 40, available, available - 40, locked, locked + 40, 0, 0
		FROM balances WHERE user_id = $2`, entry, alice.UserHK)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	_, err = b.Release(ctx, "forged:release", alice, amount(usdt, "40"), Ref{Type: "order", ID: "forged"})
	require.ErrorIs(t, err, ErrUnverified, "the made-up hold frees nothing")
	assert.Equal(t, [3]string{"60", "40", "0"}, b.components(alice, usdt))

	report, err := b.Reconcile(ctx, nil)
	require.NoError(t, err)
	assert.Len(t, report.Mismatches, 1, "the changes no longer replay to the balance")

	// Changing the reference of a real lock is refused by the database.
	_, err = b.DB.Exec(`UPDATE ledger_entries SET reference_type = 'order', reference_id = 'o' WHERE idempotency_key = 'w:lock'`)
	require.Error(t, err, "entries are append-only")
}
