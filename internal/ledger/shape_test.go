package ledger

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forge writes, in one transaction outside the ledger, an entry of kind
// that credits every balance of the books with 1000 and offsets the credit
// against the account of the exchange, with the change type change. The
// entry balances. It returns the error of the commit.
func (b books) forge(kind, change, account string) error {
	b.t.Helper()
	tx, err := b.DB.Begin()
	require.NoError(b.t, err)
	defer tx.Rollback()
	var entry string
	require.NoError(b.t, tx.QueryRow(`INSERT INTO ledger_entries (kind, idempotency_key, request_hash)
		VALUES ($1, 'forged:' || gen_random_uuid(), sha256('x'::bytea)) RETURNING id`, kind).Scan(&entry))
	_, err = tx.Exec(`INSERT INTO balance_changes (entry_id, version, balance_id, user_id, currency_hk, change_type, amount,
			old_available, new_available, old_locked, new_locked, old_pending, new_pending)
		SELECT $1, version + 1, id, user_id, currency_hk, $2, 1000, available, available + 1000, locked, locked, 0, 0
		FROM balances`, entry, change)
	require.NoError(b.t, err)
	_, err = tx.Exec(`INSERT INTO ledger_postings (entry_id, account, currency_hk, amount)
		SELECT $1, $2, currency_hk, -1000 FROM balances`, entry, account)
	require.NoError(b.t, err)
	_, err = tx.Exec(`UPDATE balances SET available = available + 1000, version = version + 1`)
	require.NoError(b.t, err)
	return tx.Commit()
}

// TestLedger_RefusesAnEntryOfTheWrongShape checks that an entry that sums
// to zero but that no operation of the ledger writes does not commit
// (#404): a role that may insert into the ledger cannot credit a user and
// offset the credit against an account of the exchange.
func TestLedger_RefusesAnEntryOfTheWrongShape(t *testing.T) {
	b := newBooks(t)
	alice, usdt := b.user(), b.currency("USDT")
	b.fund(alice, usdt, "1")

	for _, forged := range []struct{ kind, change, account string }{
		{KindTransfer, "TRANSFER_IN", AccountFees},
		{KindDepositCredited, "TRANSFER_IN", AccountFees},
		{KindTrade, "TRADE_CREDIT", AccountFees},
		{KindTrade, "TRADE_CREDIT", AccountCustody},
		{KindWithdrawal, "TRANSFER_IN", AccountInventory},
		{KindConversion, "CONVERSION_CREDIT", AccountFees},
		{KindDepositCredited, "CONVERSION_CREDIT", AccountCustody},
		// A venue account (#385) never offsets a user's credit.
		{KindVenueFee, "TRANSFER_IN", VenueAccount("binance")},
		{KindVenueWithdrawal, "TRANSFER_IN", VenueAccount("binance")},
		{KindTransfer, "TRANSFER_IN", VenueAccount("binance")},
		{KindVenueFill, "TRANSFER_IN", VenueAccount("binance")},
	} {
		err := b.forge(forged.kind, forged.change, forged.account)
		require.Error(t, err, "%+v", forged)
		assert.Contains(t, err.Error(), "which no operation of the ledger writes", "%+v", forged)
	}
	assert.Equal(t, [3]string{"1", "0", "0"}, b.components(alice, usdt))
	b.reconcile(usdt)

	// Every operation of the ledger has a shape that commits.
	ctx := context.Background()
	bob, btc := b.user(), b.currency("BTC")
	b.fund(bob, btc, "1")
	_, err := b.DetectDeposit(ctx, "shape:detect", alice, amount(usdt, "5"), Ref{})
	require.NoError(t, err)
	_, err = b.FailDeposit(ctx, "shape:fail", alice, amount(usdt, "5"), Ref{})
	require.NoError(t, err)
	b.fund(alice, usdt, "100")
	_, err = b.Transfer(ctx, "shape:transfer", alice, bob, amount(usdt, "10"), Ref{})
	require.NoError(t, err)
	_, err = b.Lock(ctx, "shape:lock", alice, amount(usdt, "50"), Ref{Type: "order", ID: "shape"})
	require.NoError(t, err)
	_, err = b.Release(ctx, "shape:release", alice, amount(usdt, "10"), Ref{Type: "order", ID: "shape"})
	require.NoError(t, err)
	_, err = b.SettleTrade(ctx, "shape:trade", alice, Trade{Give: amount(usdt, "20"), Receive: amount(btc, "0.001"), Fee: amount(usdt, "1")}, Ref{Type: "order", ID: "shape"})
	require.NoError(t, err)
	_, err = b.SettleTrade(ctx, "shape:trade-free", alice, Trade{Give: amount(usdt, "5"), Receive: amount(btc, "0.0001")}, Ref{Type: "order", ID: "shape"})
	require.NoError(t, err)
	_, err = b.Lock(ctx, "shape:lock-withdrawal", bob, amount(btc, "0.5"), Ref{Type: "withdrawal", ID: "shape"})
	require.NoError(t, err)
	_, err = b.Withdraw(ctx, "shape:withdraw", bob, amount(btc, "0.4"), dec("0.01"), Ref{Type: "withdrawal", ID: "shape"})
	require.NoError(t, err)
	_, err = b.Withdraw(ctx, "shape:withdraw-free", bob, amount(btc, "0.05"), dec("0"), Ref{Type: "withdrawal", ID: "shape"})
	require.NoError(t, err)
	eth := b.currency("ETH")
	_, err = b.Convert(ctx, "shape:convert", alice, Conversion{From: amount(usdt, "5"), To: amount(eth, "0.002")}, Ref{})
	require.NoError(t, err)
	_, err = b.Convert(ctx, "shape:convert-dust", alice, Conversion{From: amount(usdt, "5"), To: amount(eth, "0.002"), Dust: dec("0.0001")}, Ref{})
	require.NoError(t, err)
	_, err = b.Convert(ctx, "shape:convert-only-dust", alice, Conversion{From: amount(usdt, "1"), To: amount(eth, "0"), Dust: dec("0.0001")}, Ref{})
	require.NoError(t, err)
	report, err := b.Reconcile(ctx, nil)
	require.NoError(t, err)
	assert.True(t, report.OK(), "%+v", report)
}

// TestReconcile_ReportsTheAccountsOfTheExchange checks the accounts of the
// exchange (#404): with the shape check of the database turned off, as an
// owner of the tables could, a balanced entry that offsets a credit
// against the fees or the pending deposits is a problem of the books.
func TestReconcile_ReportsTheAccountsOfTheExchange(t *testing.T) {
	b := newBooks(t)
	alice, usdt := b.user(), b.currency("USDT")
	b.fund(alice, usdt, "1")
	_, err := b.DB.Exec(`ALTER TABLE ledger_postings DISABLE TRIGGER ledger_postings_balanced;
		ALTER TABLE balance_changes DISABLE TRIGGER balance_changes_balanced;
		ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_balanced`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := b.DB.Exec(`ALTER TABLE ledger_postings ENABLE TRIGGER ledger_postings_balanced;
			ALTER TABLE balance_changes ENABLE TRIGGER balance_changes_balanced;
			ALTER TABLE ledger_entries ENABLE TRIGGER ledger_entries_balanced`)
		require.NoError(t, err)
	})

	require.NoError(t, b.forge(KindTransfer, "TRANSFER_IN", AccountFees))
	report, err := b.Reconcile(context.Background(), nil)
	require.NoError(t, err)
	assert.False(t, report.OK())
	assert.Empty(t, report.Mismatches, "the balance is the replay of its changes")
	require.Len(t, report.Currencies, 1)
	c := report.Currencies[0]
	assert.True(t, c.Imbalance.IsZero(), "the books sum to zero")
	assert.False(t, c.Balanced())
	assert.Equal(t, []string{"the fees account is -1000, below zero"}, c.Problems)

	require.NoError(t, b.forge(KindDepositCredited, "TRANSFER_IN", AccountDepositsPending))
	report, err = b.Reconcile(context.Background(), nil)
	require.NoError(t, err)
	assert.Contains(t, report.Currencies[0].Problems, "the deposits_pending account is -1000, and users hold 0 pending")

	require.NoError(t, b.forge(KindWithdrawal, "TRANSFER_IN", AccountCustody))
	_, err = b.DB.Exec(`INSERT INTO ledger_postings (entry_id, account, currency_hk, amount)
		SELECT id, 'custody', $1, 3000 FROM ledger_entries LIMIT 1`, usdt)
	require.NoError(t, err)
	report, err = b.Reconcile(context.Background(), nil)
	require.NoError(t, err)
	assert.Contains(t, report.Currencies[0].Problems, "the custody account is 1999, above zero")
}
