package currency

import (
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"

	"github.com/stretchr/testify/assert"
)

func TestNetwork_RoundQuantity(t *testing.T) {
	for _, tc := range []struct {
		decimals int
		qty      string
		want     string
	}{
		{8, "1.00000000", "1"},
		{8, "1.123456789", "1.12345679"},
		{0, "1234.6", "1235"}, // zero-decimal currencies such as IRR round to whole units
		{0, "1234.4", "1234"},
		{2, "1.005", "1.01"},
		{-1, "1.5", "1.5"},     // invalid: unchanged
		{40000, "1.5", "1.5"},  // above what the database stores: unchanged
		{2, "-1.005", "-1.01"}, // rounds half away from zero
	} {
		n := &Network{Decimals: tc.decimals}
		assert.Equal(t, tc.want, n.RoundQuantity(shared.MustNewDecimalFromString(tc.qty)).String(), "%d decimals, %s", tc.decimals, tc.qty)
	}
}

func TestNetwork_ValidateWithdrawal(t *testing.T) {
	base := Network{WithdrawalEnabled: true, MinWithdrawal: shared.MustNewDecimalFromString("0.01"), MaxWithdrawal: shared.MustNewDecimalFromString("100")}
	for name, tc := range map[string]struct {
		change func(n *Network)
		amount string
		want   error
	}{
		"within the limits":       {nil, "1", nil},
		"at the minimum":          {nil, "0.01", nil},
		"at the maximum":          {nil, "100", nil},
		"under the minimum":       {nil, "0.009", ErrWithdrawalBelowMinimum},
		"over the maximum":        {nil, "100.0001", ErrWithdrawalAboveMaximum},
		"no maximum (0)":          {func(n *Network) { n.MaxWithdrawal = shared.MustNewDecimalFromString("0") }, "1000000", nil},
		"zero amount":             {nil, "0", ErrInvalidWithdrawalAmount},
		"negative amount":         {nil, "-1", ErrInvalidWithdrawalAmount},
		"zero amount, no minimum": {func(n *Network) { n.MinWithdrawal = shared.MustNewDecimalFromString("0") }, "0", ErrInvalidWithdrawalAmount},
		"withdrawals disabled":    {func(n *Network) { n.WithdrawalEnabled = false }, "1", ErrWithdrawalDisabled},
	} {
		n := base
		if tc.change != nil {
			tc.change(&n)
		}
		assert.Equal(t, tc.want, n.ValidateWithdrawal(shared.MustNewDecimalFromString(tc.amount)), name)
	}
}

func TestNetwork_ValidateDeposit(t *testing.T) {
	base := Network{DepositEnabled: true, MinDeposit: shared.MustNewDecimalFromString("1")}
	for name, tc := range map[string]struct {
		change func(n *Network)
		amount string
		want   error
	}{
		"at the minimum":    {nil, "1", nil},
		"under the minimum": {nil, "0.99", ErrDepositBelowMinimum},
		"zero amount":       {func(n *Network) { n.MinDeposit = shared.MustNewDecimalFromString("0") }, "0", ErrInvalidDepositAmount},
		"negative amount":   {nil, "-1", ErrInvalidDepositAmount},
		"deposits disabled": {func(n *Network) { n.DepositEnabled = false }, "5", ErrDepositDisabled},
	} {
		n := base
		if tc.change != nil {
			tc.change(&n)
		}
		assert.Equal(t, tc.want, n.ValidateDeposit(shared.MustNewDecimalFromString(tc.amount)), name)
	}
}
