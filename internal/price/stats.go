package price

import (
	"slices"

	"github.com/shopspring/decimal"
)

// divPlaces is the precision of the averages: VWAP and TWAP are rounded to
// it.
const divPlaces = 18

var two = decimal.NewFromInt(2)

// median returns the median of values, which must not be empty; of an even
// count, the mean of the two middle ones.
func median(values []decimal.Decimal) decimal.Decimal {
	sorted := slices.Clone(values)
	slices.SortFunc(sorted, func(a, b decimal.Decimal) int { return a.Cmp(b) })
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return sorted[n/2-1].Add(sorted[n/2]).Div(two)
}

// outliers returns, for each value, whether it is an outlier: further from
// the median than k median absolute deviations, or than floor (a fraction
// of the median) when that is larger, so that the providers that agree to
// the tick are not rejected for the noise of a tiny deviation. With fewer
// than three values none is an outlier: two values are each as far from
// their median.
func outliers(values []decimal.Decimal, k, floor decimal.Decimal) []bool {
	out := make([]bool, len(values))
	if len(values) < 3 {
		return out
	}
	m := median(values)
	deviations := make([]decimal.Decimal, len(values))
	for i, v := range values {
		deviations[i] = v.Sub(m).Abs()
	}
	limit := decimal.Max(median(deviations).Mul(k), m.Abs().Mul(floor))
	for i, d := range deviations {
		out[i] = d.GreaterThan(limit)
	}
	return out
}

// average returns num / den, or false when den is not positive.
func average(s sum) (decimal.Decimal, bool) {
	if !s.den.IsPositive() {
		return decimal.Decimal{}, false
	}
	return s.num.DivRound(s.den, divPlaces), true
}
