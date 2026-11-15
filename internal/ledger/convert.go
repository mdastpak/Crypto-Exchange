package ledger

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/shopspring/decimal"
)

// MaxConversionPlaces is the most decimal places that a conversion keeps,
// and the precision of a currency without networks.
const MaxConversionPlaces = 18

// Split returns what amount becomes at rate, a ratio or a price: the
// credit, rounded down to precision decimal places, and the dust under it,
// which the exchange keeps. The product is exact to 18 places; what lies
// beyond is under any currency's unit and is not booked.
func Split(amount, rate decimal.Decimal, precision int32) (credit, dust decimal.Decimal) {
	precision = min(max(precision, 0), MaxConversionPlaces)
	exact := amount.Mul(rate).Truncate(MaxConversionPlaces)
	credit = exact.Truncate(precision)
	return credit, exact.Sub(credit)
}

// Precision returns the precision of a currency: the most decimal places
// of its networks, as many as a withdrawal can carry, or
// MaxConversionPlaces without a network.
func Precision(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, currencyHK string) (int32, error) {
	var p sql.NullInt32
	if err := q.QueryRowContext(ctx, `SELECT MAX(decimals) FROM currency_networks WHERE currency_hk = $1`, currencyHK).Scan(&p); err != nil {
		return 0, fmt.Errorf("failed to read the precision of %s: %w", currencyHK, err)
	}
	if !p.Valid {
		return MaxConversionPlaces, nil
	}
	return p.Int32, nil
}
