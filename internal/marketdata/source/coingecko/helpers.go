package coingecko

import (
	"slices"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// nonNegative drops a negative figure, which no supply can be.
func nonNegative(d *shared.Decimal) *shared.Decimal {
	if d == nil || d.IsNegative() {
		return nil
	}
	return d
}

// sortedContracts orders contracts by network, so that a record gives the
// same order every time.
func sortedContracts(c []source.Contract) []source.Contract {
	slices.SortFunc(c, func(a, b source.Contract) int { return strings.Compare(a.Network+a.Address, b.Network+b.Address) })
	return c
}
