package ledger

import (
	"context"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// subAccount creates a sub-account of the user of a and returns the
// account of it.
func (b books) subAccount(a Account, label string) Account {
	b.t.Helper()
	var id shared.UUID
	require.NoError(b.t, b.DB.QueryRow(`INSERT INTO sub_accounts (user_id, label) VALUES ($1, $2) RETURNING id`, a.UserHK, label).Scan(&id))
	return Account{UserHK: a.UserHK, SubAccountID: &id}
}

func TestLedger_SubAccountsOfTheirUser(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	alice, bob, usdt := b.user(), b.user(), b.currency("USDT")
	trading := b.subAccount(alice, "trading")
	b.fund(alice, usdt, "100")

	// A sub-account is a balance of its own, beside the main one.
	_, err := b.Transfer(ctx, "to-trading", alice, trading, amount(usdt, "40"), Ref{})
	require.NoError(t, err)
	assert.Equal(t, [3]string{"60", "0", "0"}, b.components(alice, usdt))
	assert.Equal(t, [3]string{"40", "0", "0"}, b.components(trading, usdt))

	t.Run("a trade in a sub-account", func(t *testing.T) {
		// Three balances of one sub-account: it is checked once, and the
		// trade settles in it, apart from the main account.
		btc := b.currency("BTC")
		_, err := b.Lock(ctx, "trading:lock", trading, amount(usdt, "31"), Ref{})
		require.NoError(t, err)
		_, err = b.SettleTrade(ctx, "trading:trade", trading, Trade{
			Give: amount(usdt, "30"), Receive: amount(btc, "0.001"), Fee: amount(usdt, "1"),
		}, Ref{})
		require.NoError(t, err)
		assert.Equal(t, [3]string{"9", "0", "0"}, b.components(trading, usdt))
		assert.Equal(t, [3]string{"0.001", "0", "0"}, b.components(trading, btc))
		assert.Equal(t, [3]string{"60", "0", "0"}, b.components(alice, usdt), "the main account is untouched")
		b.reconcile(btc)
	})

	t.Run("a sub-account of another user", func(t *testing.T) {
		stolen := Account{UserHK: bob.UserHK, SubAccountID: trading.SubAccountID}
		_, err := b.Transfer(ctx, "steal", stolen, bob, amount(usdt, "40"), Ref{})
		require.ErrorIs(t, err, ErrInvalidRequest)
		assert.Contains(t, err.Error(), "is not one of user")
		_, err = b.DetectDeposit(ctx, "misfiled", stolen, amount(usdt, "1"), Ref{})
		require.ErrorIs(t, err, ErrInvalidRequest, "no balance of bob opens in alice's sub-account")
	})

	t.Run("an unknown sub-account", func(t *testing.T) {
		unknown := shared.NewUUID()
		_, err := b.DetectDeposit(ctx, "unknown", Account{UserHK: alice.UserHK, SubAccountID: &unknown}, amount(usdt, "1"), Ref{})
		require.ErrorIs(t, err, ErrInvalidRequest)
	})

	t.Run("a deleted sub-account", func(t *testing.T) {
		_, err := b.DB.Exec(`UPDATE sub_accounts SET deleted_at = NOW() WHERE id = $1`, trading.SubAccountID)
		require.NoError(t, err)
		_, err = b.Lock(ctx, "deleted", trading, amount(usdt, "1"), Ref{})
		require.ErrorIs(t, err, ErrInvalidRequest)
		assert.Contains(t, err.Error(), "is deleted")
	})

	b.reconcile(usdt)
}
