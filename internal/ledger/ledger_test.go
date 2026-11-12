package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDBName = "testdb_ledger"

// books is a ledger on an empty test database, with users and currencies.
type books struct {
	Ledger
	t *testing.T
}

func newBooks(t *testing.T) books {
	t.Helper()
	conn := pgtest.Migrated(t, testDBName)
	// Enough connections for the concurrency tests, and few enough for
	// the server's limit.
	conn.SetMaxOpenConns(20)
	return books{Ledger: Ledger{DB: conn}, t: t}
}

func (b books) user() Account {
	b.t.Helper()
	var hk shared.UUID
	require.NoError(b.t, b.DB.QueryRow(`INSERT INTO users (status) VALUES ('ACTIVE') RETURNING hk`).Scan(&hk))
	return Account{UserHK: hk}
}

func (b books) currency(symbol string) shared.UUID {
	b.t.Helper()
	var hk shared.UUID
	require.NoError(b.t, b.DB.QueryRow(`INSERT INTO currencies (status, info)
		VALUES ('approved', jsonb_build_object('symbol', $1::text, 'en_name', $1::text, 'fa_name', $1::text,
			'if_fiat', false, 'is_token', false, 'contract', '')) RETURNING hk`, symbol).Scan(&hk))
	return hk
}

func dec(s string) shared.Decimal { return shared.MustNewDecimalFromString(s) }

func amount(currency shared.UUID, s string) Amount { return Amount{Currency: currency, Value: dec(s)} }

// components returns available, locked, and pending of a balance.
func (b books) components(a Account, currency shared.UUID) [3]string {
	b.t.Helper()
	bal, err := b.Balance(context.Background(), a, currency)
	require.NoError(b.t, err)
	return [3]string{bal.Available.String(), bal.Locked.String(), bal.PendingDeposit.String()}
}

// fund credits a deposit of value to a.
func (b books) fund(a Account, currency shared.UUID, value string) {
	b.t.Helper()
	ctx := context.Background()
	key := fmt.Sprintf("fund:%s:%s:%s", a, currency, shared.NewUUID())
	_, err := b.DetectDeposit(ctx, key+":detect", a, amount(currency, value), Ref{Type: "deposit", ID: key})
	require.NoError(b.t, err)
	_, err = b.CreditDeposit(ctx, key+":credit", a, amount(currency, value), Ref{Type: "deposit", ID: key})
	require.NoError(b.t, err)
}

// reconcile checks the books and returns the report of currency.
func (b books) reconcile(currency shared.UUID) CurrencyReport {
	b.t.Helper()
	report, err := b.Reconcile(context.Background(), nil)
	require.NoError(b.t, err)
	assert.Empty(b.t, report.Mismatches, "every balance is the replay of its changes")
	assert.True(b.t, report.OK(), "%+v", report)
	for _, c := range report.Currencies {
		if c.Currency == currency {
			return c
		}
	}
	return CurrencyReport{}
}

func TestLedger_TheLifeOfADeposit(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	alice, usdt := b.user(), b.currency("USDT")
	ref := Ref{Type: "deposit", ID: "tron:abc:0"}

	_, err := b.DetectDeposit(ctx, "deposit:tron:abc:0:detect", alice, amount(usdt, "100"), ref)
	require.NoError(t, err)
	assert.Equal(t, [3]string{"0", "0", "100"}, b.components(alice, usdt), "pending, not owned yet")
	_, err = b.CreditDeposit(ctx, "deposit:tron:abc:0:credit", alice, amount(usdt, "100"), ref)
	require.NoError(t, err)
	assert.Equal(t, [3]string{"100", "0", "0"}, b.components(alice, usdt))

	// A withdrawal: its amount and fee are locked when it is requested,
	// and leave when it completes.
	_, err = b.Lock(ctx, "withdrawal:w1:lock", alice, amount(usdt, "31"), Ref{Type: "withdrawal", ID: "w1"})
	require.NoError(t, err)
	assert.Equal(t, [3]string{"69", "31", "0"}, b.components(alice, usdt))
	_, err = b.Withdraw(ctx, "withdrawal:w1:complete", alice, amount(usdt, "30"), dec("1"), Ref{Type: "withdrawal", ID: "w1"})
	require.NoError(t, err)
	assert.Equal(t, [3]string{"69", "0", "0"}, b.components(alice, usdt))

	// A deposit that will not be credited.
	_, err = b.DetectDeposit(ctx, "deposit:tron:def:0:detect", alice, amount(usdt, "5"), Ref{Type: "deposit", ID: "tron:def:0"})
	require.NoError(t, err)
	_, err = b.FailDeposit(ctx, "deposit:tron:def:0:fail", alice, amount(usdt, "5"), Ref{Type: "deposit", ID: "tron:def:0"})
	require.NoError(t, err)
	assert.Equal(t, [3]string{"69", "0", "0"}, b.components(alice, usdt))

	books := b.reconcile(usdt)
	assert.Equal(t, "69", books.Users.String())
	assert.Equal(t, "1", books.Accounts[AccountFees].String())
	assert.Equal(t, "0", books.Accounts[AccountDepositsPending].String())
	assert.Equal(t, "70", books.Expected.String(), "custody must hold the deposit less the withdrawal: what users own and the fee")
	assert.Equal(t, "USDT", books.Symbol)
}

func TestLedger_TransferAndTrade(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	alice, bob := b.user(), b.user()
	usdt, btc := b.currency("USDT"), b.currency("BTC")
	b.fund(alice, usdt, "1000")

	_, err := b.Transfer(ctx, "transfer:1", alice, bob, amount(usdt, "250"), Ref{})
	require.NoError(t, err)
	assert.Equal(t, [3]string{"750", "0", "0"}, b.components(alice, usdt))
	assert.Equal(t, [3]string{"250", "0", "0"}, b.components(bob, usdt))
	_, err = b.Transfer(ctx, "transfer:2", alice, alice, amount(usdt, "1"), Ref{})
	require.ErrorIs(t, err, ErrInvalidRequest)

	// Alice buys 0.01 BTC for 600 USDT and a fee of 3 USDT, from the
	// exchange's inventory: the price and the fee were locked with the
	// order.
	_, err = b.Lock(ctx, "order:o1:lock", alice, amount(usdt, "603"), Ref{Type: "order", ID: "o1"})
	require.NoError(t, err)
	_, err = b.SettleTrade(ctx, "trade:t1", alice, Trade{
		Give: amount(usdt, "600"), Receive: amount(btc, "0.01"), Fee: amount(usdt, "3"),
	}, Ref{Type: "trade", ID: "t1"})
	require.NoError(t, err)
	assert.Equal(t, [3]string{"147", "0", "0"}, b.components(alice, usdt))
	assert.Equal(t, [3]string{"0.01", "0", "0"}, b.components(alice, btc))

	usdtBooks, btcBooks := b.reconcile(usdt), b.reconcile(btc)
	assert.Equal(t, "600", usdtBooks.Accounts[AccountInventory].String())
	assert.Equal(t, "3", usdtBooks.Accounts[AccountFees].String())
	assert.Equal(t, "-0.01", btcBooks.Accounts[AccountInventory].String(), "the BTC left the exchange's inventory")
	assert.Equal(t, "1000", usdtBooks.Expected.String())
}

func TestLedger_RefusesWhatWouldGoBelowZero(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	alice, usdt, btc := b.user(), b.currency("USDT"), b.currency("BTC")
	b.fund(alice, usdt, "10")

	_, err := b.Lock(ctx, "lock:1", alice, amount(usdt, "10.000001"), Ref{})
	require.ErrorIs(t, err, ErrInsufficientBalance)
	_, err = b.Withdraw(ctx, "withdraw:1", alice, amount(usdt, "1"), dec("0"), Ref{})
	require.ErrorIs(t, err, ErrInsufficientLocked, "a withdrawal leaves from locked funds")
	_, err = b.SettleTrade(ctx, "trade:1", alice, Trade{Give: amount(usdt, "5"), Receive: amount(btc, "1")}, Ref{})
	require.ErrorIs(t, err, ErrInsufficientLocked, "a trade gives locked funds")
	assert.Equal(t, [3]string{"10", "0", "0"}, b.components(alice, usdt), "a refused operation changes nothing")

	// The key of a refused operation is free: nothing was recorded.
	res, err := b.Lock(ctx, "lock:1", alice, amount(usdt, "10"), Ref{})
	require.NoError(t, err)
	assert.False(t, res.Replayed)

	for name, op := range map[string]func() error{
		"a zero amount": func() error {
			_, err := b.Lock(ctx, "lock:zero", alice, amount(usdt, "0"), Ref{})
			return err
		},
		"a negative amount": func() error {
			_, err := b.Release(ctx, "release:negative", alice, amount(usdt, "-1"), Ref{})
			return err
		},
		"an unknown currency": func() error {
			_, err := b.DetectDeposit(ctx, "deposit:unknown", alice, amount(shared.NewUUID(), "1"), Ref{})
			return err
		},
		"no key": func() error {
			_, err := b.Lock(ctx, "", alice, amount(usdt, "1"), Ref{})
			return err
		},
		"a reference without its ID": func() error {
			_, err := b.Lock(ctx, "lock:ref", alice, amount(usdt, "1"), Ref{Type: "order"})
			return err
		},
	} {
		assert.ErrorIs(t, op(), ErrInvalidRequest, name)
	}
}

func TestLedger_AnIdempotencyKeyAppliesOnce(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	alice, usdt := b.user(), b.currency("USDT")
	ref := Ref{Type: "deposit", ID: "d1"}

	first, err := b.DetectDeposit(ctx, "deposit:d1", alice, amount(usdt, "5"), ref)
	require.NoError(t, err)
	again, err := b.DetectDeposit(ctx, "deposit:d1", alice, amount(usdt, "5.000"), ref)
	require.NoError(t, err)
	assert.True(t, again.Replayed, "the same request, written otherwise")
	assert.Equal(t, first.EntryID, again.EntryID)
	assert.Equal(t, [3]string{"0", "0", "5"}, b.components(alice, usdt), "applied once")

	_, err = b.DetectDeposit(ctx, "deposit:d1", alice, amount(usdt, "6"), ref)
	require.ErrorIs(t, err, ErrIdempotencyConflict, "the key of another request")

	t.Run("concurrent retries", func(t *testing.T) {
		const retries = 50
		var applied, replayed atomic.Int32
		var wg sync.WaitGroup
		for range retries {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := b.CreditDeposit(ctx, "deposit:d1:credit", alice, amount(usdt, "5"), ref)
				if !assert.NoError(t, err) {
					return
				}
				if res.Replayed {
					replayed.Add(1)
				} else {
					applied.Add(1)
				}
			}()
		}
		wg.Wait()
		assert.Equal(t, int32(1), applied.Load())
		assert.Equal(t, int32(retries-1), replayed.Load())
		assert.Equal(t, [3]string{"5", "0", "0"}, b.components(alice, usdt))
	})
}

// TestLedger_ConcurrentOperationsLoseNothing runs hundreds of operations
// at once on the same balances: locks that compete for the same funds, and
// transfers in both directions between two accounts, which would deadlock
// if the balances were not locked in a fixed order.
func TestLedger_ConcurrentOperationsLoseNothing(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	alice, bob, usdt := b.user(), b.user(), b.currency("USDT")
	b.fund(alice, usdt, "100")
	b.fund(bob, usdt, "100")

	t.Run("locks compete for the same funds", func(t *testing.T) {
		const attempts = 300
		var locked, refused atomic.Int32
		var wg sync.WaitGroup
		for i := range attempts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := b.Lock(ctx, fmt.Sprintf("lock:%d", i), alice, amount(usdt, "0.5"), Ref{})
				switch {
				case err == nil:
					locked.Add(1)
				case errors.Is(err, ErrInsufficientBalance):
					refused.Add(1)
				default:
					assert.NoError(t, err)
				}
			}()
		}
		wg.Wait()
		assert.Equal(t, int32(200), locked.Load(), "exactly the funds there are")
		assert.Equal(t, int32(100), refused.Load())
		assert.Equal(t, [3]string{"0", "100", "0"}, b.components(alice, usdt))

		var wg2 sync.WaitGroup
		for i := range 200 {
			wg2.Add(1)
			go func() {
				defer wg2.Done()
				_, err := b.Release(ctx, fmt.Sprintf("release:%d", i), alice, amount(usdt, "0.5"), Ref{})
				assert.NoError(t, err)
			}()
		}
		wg2.Wait()
		assert.Equal(t, [3]string{"100", "0", "0"}, b.components(alice, usdt))
	})

	t.Run("transfers both ways", func(t *testing.T) {
		const each = 150
		var wg sync.WaitGroup
		for i := range each {
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, err := b.Transfer(ctx, fmt.Sprintf("a-to-b:%d", i), alice, bob, amount(usdt, "0.25"), Ref{})
				assert.NoError(t, err)
			}()
			go func() {
				defer wg.Done()
				_, err := b.Transfer(ctx, fmt.Sprintf("b-to-a:%d", i), bob, alice, amount(usdt, "0.25"), Ref{})
				assert.NoError(t, err)
			}()
		}
		wg.Wait()
		assert.Equal(t, [3]string{"100", "0", "0"}, b.components(alice, usdt))
		assert.Equal(t, [3]string{"100", "0", "0"}, b.components(bob, usdt))
	})

	books := b.reconcile(usdt)
	assert.Equal(t, "200", books.Users.String())
	assert.Equal(t, "200", books.Expected.String())
	var changes int
	require.NoError(t, b.DB.QueryRow(`SELECT COUNT(*) FROM balance_changes`).Scan(&changes))
	assert.Equal(t, 4+200+200+3*2*150, changes, "every applied operation is recorded, and no refused one")
}

// holdings is what custody reports, in a test.
type holdings map[shared.UUID]shared.Decimal

func (h holdings) Holdings(_ context.Context, currency shared.UUID) (shared.Decimal, bool, error) {
	v, ok := h[currency]
	return v, ok, nil
}

func TestReconcile_FindsWhatChangedOutsideTheLedger(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	alice, usdt := b.user(), b.currency("USDT")
	b.fund(alice, usdt, "100")

	report, err := b.Reconcile(ctx, holdings{usdt: dec("100")})
	require.NoError(t, err)
	assert.True(t, report.OK())
	require.Len(t, report.Currencies, 1)
	assert.Equal(t, "0", report.Currencies[0].Difference.String())

	t.Run("custody holds less than it must", func(t *testing.T) {
		report, err := b.Reconcile(ctx, holdings{usdt: dec("99.5")})
		require.NoError(t, err)
		assert.False(t, report.OK())
		assert.Equal(t, "-0.5", report.Currencies[0].Difference.String())
	})

	t.Run("a balance changed in SQL", func(t *testing.T) {
		_, err := b.DB.Exec(`UPDATE balances SET available = available + 1000`)
		require.NoError(t, err)
		report, err := b.Reconcile(ctx, nil)
		require.NoError(t, err)
		assert.False(t, report.OK())
		require.Len(t, report.Mismatches, 1)
		assert.Contains(t, report.Mismatches[0].Reason, "replayed to version")
		assert.Equal(t, "1000", report.Currencies[0].Imbalance.String(), "the books no longer balance")
	})
}

func TestReconcile_ReplaysAStoredChangeThatIsNotItsTransition(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	alice, usdt := b.user(), b.currency("USDT")
	b.fund(alice, usdt, "100")
	// A change written outside the code, balanced but with the wrong type:
	// UNLOCK of 10 that moves funds from available to locked.
	tx, err := b.DB.Begin()
	require.NoError(t, err)
	var entry string
	require.NoError(t, tx.QueryRow(`INSERT INTO ledger_entries (kind, idempotency_key, request_hash)
		VALUES ('lock', 'forged', sha256('x'::bytea)) RETURNING id`).Scan(&entry))
	_, err = tx.Exec(`INSERT INTO balance_changes (entry_id, version, balance_id, user_id, currency_hk, change_type, amount,
			old_available, new_available, old_locked, new_locked, old_pending, new_pending)
		SELECT $1, version + 1, id, user_id, currency_hk, 'UNLOCK', 10, available, available - 10, locked, locked + 10, 0, 0
		FROM balances`, entry)
	require.NoError(t, err)
	_, err = tx.Exec(`UPDATE balances SET available = available - 10, locked = locked + 10, version = version + 1`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	report, err := b.Reconcile(ctx, nil)
	require.NoError(t, err)
	require.Len(t, report.Mismatches, 1)
	assert.Contains(t, report.Mismatches[0].Reason, "is not a valid transition")
}
