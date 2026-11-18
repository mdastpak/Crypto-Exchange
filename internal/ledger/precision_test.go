package ledger

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPrecision_AMoveFinerThanItsCurrencyIsRefused shows #422: a move of a
// balance with more decimals than the networks of its currency carry is
// refused as invalid, and so is one beyond the 36 decimals of the domain,
// never with a raw error of the database. A currency without networks
// keeps 18.
func TestPrecision_AMoveFinerThanItsCurrencyIsRefused(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t)
	alice, usdt, irr, bare := b.user(), b.currency("USDT"), b.currency("IRR"), b.currency("BARE")
	for _, n := range []struct {
		currency any
		network  string
		decimals int
	}{{usdt, "tron", 6}, {usdt, "ethereum", 18}, {irr, "shetab", 0}} {
		_, err := b.DB.Exec(`INSERT INTO currency_networks (currency_hk, network, decimals, custody_mode) VALUES ($1, $2, $3, 'OFF_CHAIN')`,
			n.currency, n.network, n.decimals)
		require.NoError(t, err)
	}
	b.fund(alice, usdt, "1")
	b.fund(alice, irr, "100")

	for _, c := range []struct {
		name string
		a    Amount
		ok   bool
	}{
		{"the finest network of USDT", amount(usdt, "0.000000000000000001"), true},
		{"finer than every network of USDT", amount(usdt, "0.0000000000000000001"), false},
		{"beyond the domain", amount(usdt, "0.0000000000000000000000000000000000001"), false},
		{"a whole rial", amount(irr, "1"), true},
		{"trailing zeros are not finer", amount(irr, "1.000"), true},
		{"a fraction of a rial", amount(irr, "0.5"), false},
		{"finer than 18 decimals without networks", amount(bare, "0.0000000000000000001"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := b.Lock(ctx, "precision:"+c.name, alice, c.a, Ref{Type: "order", ID: c.name})
			if c.ok {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrInvalidRequest)
		})
	}
	assert.Equal(t, [3]string{"98", "2", "0"}, b.components(alice, irr), "only the two whole rials are locked")
}
