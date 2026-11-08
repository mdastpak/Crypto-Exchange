package marketdata

import (
	"slices"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// QualityRules say when a ticker is stale or an outlier (#267).
type QualityRules struct {
	// StaleAfter: a ticker observed longer ago than this is stale.
	StaleAfter time.Duration
	// MaxDeviation is the share of the median, such as 0.05, by which a
	// price may differ from the median of the venues before it is an
	// outlier.
	MaxDeviation decimal.Decimal
	// MinVenues is the fewest venues with a fresh ticker of the same quote
	// symbol that make a median; with fewer, nothing is an outlier.
	MinVenues int
}

// Flag sets the Stale, Outlier, and Median of quotes at now. The median is
// that of the fresh tickers of each quote symbol, one per venue, so that a
// venue with several pairs of the same quote counts once; a stale ticker
// is never an outlier, and does not move the median.
func Flag(quotes []Quote, now time.Time, rules QualityRules) {
	byQuote := map[string]map[string]decimal.Decimal{}
	for i := range quotes {
		q := &quotes[i]
		q.Stale = now.Sub(q.Ticker.ObservedAt) > rules.StaleAfter
		q.Outlier, q.Median = false, nil
		if q.Stale {
			continue
		}
		if byQuote[q.QuoteSymbol] == nil {
			byQuote[q.QuoteSymbol] = map[string]decimal.Decimal{}
		}
		// One price per venue: the first pair of the venue.
		if _, ok := byQuote[q.QuoteSymbol][q.VenueID]; !ok {
			byQuote[q.QuoteSymbol][q.VenueID] = q.Ticker.LastPrice.Decimal()
		}
	}
	medians := map[string]decimal.Decimal{}
	for quote, prices := range byQuote {
		if len(prices) < max(rules.MinVenues, 1) {
			continue
		}
		medians[quote] = median(prices)
	}
	for i := range quotes {
		q := &quotes[i]
		m, ok := medians[q.QuoteSymbol]
		if !ok {
			continue
		}
		d := shared.Decimal(m)
		q.Median = &d
		if q.Stale || !m.IsPositive() {
			continue
		}
		deviation := q.Ticker.LastPrice.Decimal().Sub(m).Abs().Div(m)
		q.Outlier = deviation.GreaterThan(rules.MaxDeviation)
	}
}

// median is the median of the prices, the mean of the middle two for an
// even count, exact in decimals.
func median(prices map[string]decimal.Decimal) decimal.Decimal {
	values := make([]decimal.Decimal, 0, len(prices))
	for _, p := range prices {
		values = append(values, p)
	}
	slices.SortFunc(values, func(a, b decimal.Decimal) int { return a.Cmp(b) })
	mid := len(values) / 2
	if len(values)%2 == 1 {
		return values[mid]
	}
	return values[mid-1].Add(values[mid]).Div(decimal.NewFromInt(2))
}
