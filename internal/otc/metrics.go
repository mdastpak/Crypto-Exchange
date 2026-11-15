package otc

import (
	"errors"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/metrics"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/verification"
)

// Metrics records the quotes on the process metrics. Only the quoted
// markets become labels.
type Metrics struct {
	markets map[string]bool
}

// NewMetrics returns the metrics of the quotes of markets.
func NewMetrics(markets []string) *Metrics {
	m := &Metrics{markets: map[string]bool{}}
	for _, s := range markets {
		m.markets[s] = true
	}
	return m
}

func (m *Metrics) label(market string) string {
	if m.markets[market] {
		return market
	}
	return "other"
}

func (m *Metrics) observe(market string, start time.Time, err error) {
	if m == nil {
		return
	}
	metrics.OTCQuoteDuration.Observe(time.Since(start).Seconds())
	metrics.OTCQuotes.WithLabelValues(m.label(market), outcome(err)).Inc()
}

func (m *Metrics) quoted(market string, side Side, parts Components) {
	if m == nil {
		return
	}
	margin, _ := parts.MarginBPS.Float64()
	metrics.OTCQuotedMargin.WithLabelValues(m.label(market), string(side)).Observe(margin)
}

// outcome is the label of a result: quoted, the code of a refusal, or
// error.
func outcome(err error) string {
	var r *Refusal
	var v *verification.Refusal
	var x *restrictions.Refusal
	var c *currencies.Refusal
	switch {
	case err == nil:
		return "quoted"
	case errors.As(err, &r):
		return r.Code
	case errors.As(err, &v):
		return v.Code
	case errors.As(err, &x):
		return x.Code
	case errors.As(err, &c):
		return currencies.CodeCurrencyOperationDisabled
	default:
		return "error"
	}
}
