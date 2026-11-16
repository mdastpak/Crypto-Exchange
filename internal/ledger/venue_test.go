package ledger

import (
	"context"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLedger_VenueMoves covers the house accounts at the venues (#385): a
// deposit to a venue moves custody to it, a fee takes from the inventory,
// a withdrawal moves back, and the books stay balanced throughout.
func TestLedger_VenueMoves(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	alice, usdt := b.user(), b.currency("USDT")
	b.fund(alice, usdt, "1000")

	_, err := b.VenueDeposit(ctx, "venue:deposit:1", "binance", amount(usdt, "400"), Ref{Type: "venue_deposit", ID: "1"})
	require.NoError(t, err)
	_, err = b.VenueFee(ctx, "venue:fee:1", "binance", amount(usdt, "1.5"), Ref{Type: "venue_fee", ID: "1"})
	require.NoError(t, err)
	_, err = b.VenueWithdrawal(ctx, "venue:withdrawal:1", "binance", amount(usdt, "100"), Ref{Type: "venue_withdrawal", ID: "1"})
	require.NoError(t, err)

	held, err := b.VenueHoldings(ctx, "binance")
	require.NoError(t, err)
	assert.Equal(t, "298.5", held[usdt.String()].String())
	none, err := b.VenueHoldings(ctx, "nobitex")
	require.NoError(t, err)
	assert.Empty(t, none)

	c := b.reconcile(usdt)
	assert.Equal(t, "-700", c.Accounts[AccountCustody].String(), "custody holds what is not at the venue")
	assert.Equal(t, "-1.5", c.Accounts[AccountInventory].String(), "the inventory paid the fee")
	assert.Equal(t, "-298.5", c.Accounts[VenueAccount("binance")].String())
	assert.Equal(t, [3]string{"1000", "0", "0"}, b.components(alice, usdt), "no user balance moves")

	// The same key applies once.
	_, err = b.VenueFee(ctx, "venue:fee:1", "binance", amount(usdt, "1.5"), Ref{Type: "venue_fee", ID: "1"})
	require.NoError(t, err)
	held, err = b.VenueHoldings(ctx, "binance")
	require.NoError(t, err)
	assert.Equal(t, "298.5", held[usdt.String()].String())
}

// TestLedger_VenueMovesRefuse covers what a venue move refuses: an amount
// that is not positive, and a name that is not a venue's.
func TestLedger_VenueMovesRefuse(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	usdt := b.currency("USDT")
	for _, a := range []string{"0", "-1"} {
		_, err := b.VenueDeposit(ctx, "venue:bad:"+a, "binance", amount(usdt, a), Ref{})
		require.ErrorIs(t, err, ErrInvalidRequest)
		_, err = b.VenueWithdrawal(ctx, "venue:bad-w:"+a, "binance", amount(usdt, a), Ref{})
		require.ErrorIs(t, err, ErrInvalidRequest)
		_, err = b.VenueFee(ctx, "venue:bad-f:"+a, "binance", amount(usdt, a), Ref{})
		require.ErrorIs(t, err, ErrInvalidRequest)
	}
	_, err := b.VenueDeposit(ctx, "venue:bad-name", "Bin ance", amount(usdt, "1"), Ref{})
	require.ErrorIs(t, err, ErrInvalidRequest)

	// The schema refuses an account that is not a venue's.
	b.fund(b.user(), usdt, "1")
	_, err = b.DB.Exec(`INSERT INTO ledger_postings (entry_id, account, currency_hk, amount)
		SELECT id, 'venue:Bad', $1, 1 FROM ledger_entries LIMIT 1`, usdt)
	require.ErrorContains(t, err, "ledger_postings_account_check")
}

// TestReconcile_FlagsAVenueAccountAboveZero covers the account check: a
// venue account above zero means more left the venue than reached it.
func TestReconcile_FlagsAVenueAccountAboveZero(t *testing.T) {
	problems := accountProblems(CurrencyReport{Accounts: map[string]shared.Decimal{
		AccountFees: dec("0"), AccountCustody: dec("0"), AccountDepositsPending: dec("0"),
		VenueAccount("binance"): dec("1"),
	}})
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "venue:binance")
}

// TestLedger_VenueFill covers the fill of a routed order (#387): the venue
// holds the base currency it bought and less of the quote it spent, the
// inventory the reverse, and a key applies once.
func TestLedger_VenueFill(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	alice, usdt, btc := b.user(), b.currency("USDT"), b.currency("BTC")
	b.fund(alice, usdt, "1000")
	_, err := b.VenueDeposit(ctx, "venue:deposit:fill", "binance", amount(usdt, "500"), Ref{Type: "venue_deposit", ID: "fill"})
	require.NoError(t, err)

	tx, err := b.DB.Begin()
	require.NoError(t, err)
	_, err = b.In(tx).VenueFill(ctx, "venue:fill:1", "binance", amount(btc, "0.004"), amount(usdt, "260"), Ref{Type: "otc_routed_order", ID: "1"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	_, err = b.VenueFill(ctx, "venue:fill:1", "binance", amount(btc, "0.004"), amount(usdt, "260"), Ref{Type: "otc_routed_order", ID: "1"})
	require.NoError(t, err)

	held, err := b.VenueHoldings(ctx, "binance")
	require.NoError(t, err)
	assert.Equal(t, "240", held[usdt.String()].String())
	assert.Equal(t, "0.004", held[btc.String()].String())
	assert.Equal(t, "-260", b.reconcile(usdt).Accounts[AccountInventory].String(), "the inventory paid the quote")
	assert.Equal(t, "0.004", b.reconcile(btc).Accounts[AccountInventory].String(), "and got the base")

	for _, bad := range [][2]string{{"0", "1"}, {"1", "-1"}} {
		_, err = b.VenueFill(ctx, "venue:fill:bad:"+bad[0]+bad[1], "binance", amount(btc, bad[0]), amount(usdt, bad[1]), Ref{})
		require.ErrorIs(t, err, ErrInvalidRequest)
	}
	_, err = b.VenueFill(ctx, "venue:fill:same", "binance", amount(btc, "1"), amount(btc, "1"), Ref{})
	require.ErrorIs(t, err, ErrInvalidRequest)
}
