package aml

import (
	"context"
	"slices"
	"time"

	"github.com/shopspring/decimal"
)

// reasonNoPrice is the reason of a hit that needed a value that no current
// price gave: it fails closed, to a review.
const reasonNoPrice = "no_price"

// evaluate evaluates a rule that is not a screening rule. cur is the
// operation evaluated, and h what came before it.
func evaluate(_ context.Context, _ Querier, r Stored, ev Event, cur past, h history, at time.Time) (*Hit, error) {
	window := within(h.events, r, at)
	if r.Applies(cur.kind) {
		window = append(window, cur)
	}
	if r.Type == TypeVelocity && r.Config.Metric == MetricCycles {
		// A cycle is a deposit, whatever the rule applies to, and a
		// withdrawal after it.
		window = append(inWindow(h.events, r, at), cur)
	}
	var data map[string]any
	var noPrice bool
	switch r.Type {
	case TypeVelocity:
		data, noPrice = velocity(r, cur, window)
	case TypeStructuring:
		data, noPrice = structuring(r, cur, window)
	case TypeHighRiskCountry:
		data, noPrice = country(r, cur, window)
	case TypeUnusualPattern:
		switch r.Config.Pattern {
		case PatternWithdrawalProfile:
			data, noPrice = profile(r, cur, h, at, ev)
		case PatternDormantAccount:
			data = dormant(r, h, at)
		}
	}
	switch {
	case noPrice:
		return &Hit{Rule: r, Action: ActionReview, Data: map[string]any{"reason": reasonNoPrice}}, nil
	case data == nil:
		return nil, nil
	}
	return &Hit{Rule: r, Action: r.Action, Data: data}, nil
}

// within returns the operations of h of the rule's window that it applies
// to.
func within(events []past, r Stored, at time.Time) []past {
	from := at.Add(-time.Duration(r.WindowHours) * time.Hour)
	var out []past
	for _, p := range events {
		if p.at.After(from) && r.Applies(p.kind) {
			out = append(out, p)
		}
	}
	return out
}

// inWindow returns every operation of h of the rule's window.
func inWindow(events []past, r Stored, at time.Time) []past {
	from := at.Add(-time.Duration(r.WindowHours) * time.Hour)
	var out []past
	for _, p := range events {
		if p.at.After(from) {
			out = append(out, p)
		}
	}
	return out
}

// sum adds the values of events, or reports one without a value.
func sum(events []past) (decimal.Decimal, bool) {
	total := decimal.Zero
	for _, p := range events {
		if p.value == nil {
			return total, false
		}
		total = total.Add(*p.value)
	}
	return total, true
}

// velocity counts or sums the window: the data of a hit, or nil, and
// whether a value was missing.
func velocity(r Stored, cur past, window []past) (map[string]any, bool) {
	switch r.Config.Metric {
	case MetricCount:
		if len(window) >= r.ThresholdCount {
			return map[string]any{"metric": MetricCount, "count": len(window)}, false
		}
	case MetricVolume:
		total, ok := sum(window)
		if !ok {
			return nil, true
		}
		if total.GreaterThanOrEqual(r.ThresholdAmount) {
			return map[string]any{"metric": MetricVolume, "value_usdt": total.String()}, false
		}
	case MetricSingle:
		if cur.value == nil {
			return nil, true
		}
		if cur.value.GreaterThanOrEqual(r.ThresholdAmount) {
			return map[string]any{"metric": MetricSingle, "value_usdt": cur.value.String()}, false
		}
	case MetricCycles:
		if n := cycles(window); n >= r.ThresholdCount {
			return map[string]any{"metric": MetricCycles, "cycles": n}, false
		}
	}
	return nil, false
}

// cycles counts the withdrawals of the window that follow a deposit of the
// window. The window of a cycles rule holds the withdrawals that the rule
// applies to; the deposits are counted whatever it applies to.
func cycles(window []past) int {
	n, deposited := 0, false
	for _, p := range window {
		switch p.kind {
		case KindDeposit:
			deposited = true
		case KindWithdrawal:
			if deposited {
				n++
			}
		}
	}
	return n
}

// structuring counts the operations of the window in the band, when the
// operation evaluated is in it.
func structuring(r Stored, cur past, window []past) (map[string]any, bool) {
	ceiling := *r.Config.Ceiling
	in := func(v decimal.Decimal) bool {
		return v.GreaterThanOrEqual(r.ThresholdAmount) && v.LessThan(ceiling)
	}
	if cur.value == nil {
		return nil, true
	}
	if !in(*cur.value) {
		return nil, false
	}
	n, unknown := 0, 0
	for _, p := range window {
		switch {
		case p.value == nil:
			unknown++
		case in(*p.value):
			n++
		}
	}
	if n >= r.ThresholdCount {
		return map[string]any{"count": n, "band_usdt": []string{r.ThresholdAmount.String(), ceiling.String()}}, false
	}
	return nil, n+unknown >= r.ThresholdCount
}

// countries are the countries of an operation that a rule checks.
func countries(r Stored, p past) []string {
	var out []string
	for _, field := range r.Config.CheckFields {
		switch field {
		case FieldIPCountry:
			out = append(out, p.ipCountry)
		case FieldCounterpartyCountry:
			out = append(out, p.counterpartyCountry)
		}
	}
	return out
}

// matches reports whether an operation is of a high-risk country, or
// crosses the border, for r, and the country.
func matches(r Stored, p past) (string, bool) {
	for _, c := range countries(r, p) {
		switch {
		case c == "":
		case r.Config.CrossBorder && c != r.Config.HomeCountry:
			return c, true
		case !r.Config.CrossBorder && slices.Contains(r.Config.Countries, c):
			return c, true
		}
	}
	return "", false
}

// country hits an operation that matches, and with a threshold, once the
// operations of the window that match reach it.
func country(r Stored, cur past, window []past) (map[string]any, bool) {
	c, ok := matches(r, cur)
	if !ok {
		return nil, false
	}
	data := map[string]any{"country": c}
	if r.ThresholdAmount.IsZero() {
		return data, false
	}
	var matching []past
	for _, p := range window {
		if _, ok := matches(r, p); ok {
			matching = append(matching, p)
		}
	}
	total, known := sum(matching)
	if !known {
		return nil, true
	}
	if total.LessThan(r.ThresholdAmount) {
		return nil, false
	}
	data["value_usdt"] = total.String()
	return data, false
}

// profile runs the checks of a withdrawal profile against the user's
// withdrawals of the history, and hits when enough of them fire.
func profile(r Stored, cur past, h history, at time.Time, ev Event) (map[string]any, bool) {
	c := r.Config
	from := at.Add(-time.Duration(c.HistoryDays) * 24 * time.Hour)
	var before []past
	for _, p := range h.events {
		if p.kind == KindWithdrawal && p.at.After(from) {
			before = append(before, p)
		}
	}
	var fired []string
	for _, check := range c.Checks {
		switch check {
		case CheckNewDestination:
			if ev.CounterpartySince != nil && at.Sub(*ev.CounterpartySince) < time.Duration(c.NewDestinationDays)*24*time.Hour {
				fired = append(fired, check)
			}
		case CheckAmount:
			if len(before) == 0 {
				continue
			}
			if cur.value == nil {
				return nil, true
			}
			total, ok := sum(before)
			if !ok {
				return nil, true
			}
			average := total.Div(decimal.NewFromInt(int64(len(before))))
			if cur.value.GreaterThanOrEqual(average.Mul(*c.AmountDeviationMultiplier)) {
				fired = append(fired, check)
			}
		case CheckTime:
			if len(before) >= 3 && offHours(before, at, c.TimeDeviationHours) {
				fired = append(fired, check)
			}
		case CheckVelocity:
			if spike(before, at, r) {
				fired = append(fired, check)
			}
		}
	}
	if len(fired) < r.ThresholdCount {
		return nil, false
	}
	return map[string]any{"checks": fired}, false
}

// offHours reports whether every withdrawal of before was more than hours
// away from the hour of the day of at, in UTC.
func offHours(before []past, at time.Time, hours int) bool {
	for _, p := range before {
		d := p.at.Hour() - at.Hour()
		if d < 0 {
			d = -d
		}
		if min(d, 24-d) <= hours {
			return false
		}
	}
	return true
}

// spike reports whether the withdrawals of the window, with the one
// evaluated, are at least two and at least the multiplier times as many as
// the history has in a window on average.
func spike(before []past, at time.Time, r Stored) bool {
	if len(before) == 0 {
		return false
	}
	window := 1
	from := at.Add(-time.Duration(r.WindowHours) * time.Hour)
	for _, p := range before {
		if p.at.After(from) {
			window++
		}
	}
	windows := decimal.NewFromInt(int64(r.Config.HistoryDays * 24)).Div(decimal.NewFromInt(int64(r.WindowHours)))
	average := decimal.NewFromInt(int64(len(before))).Div(windows)
	return window >= 2 && decimal.NewFromInt(int64(window)).GreaterThanOrEqual(average.Mul(*r.Config.VelocitySpikeMultiplier))
}

// dormant hits the first operation after DormantDays without one. An
// account without an operation counts from when it joined, or from when
// the engine recorded its first operation, whichever is later: the
// accounts of before the engine are not all dormant on its first day.
func dormant(r Stored, h history, at time.Time) map[string]any {
	span := time.Duration(r.Config.DormantDays) * 24 * time.Hour
	if h.lastAt != nil {
		if at.Sub(*h.lastAt) >= span {
			return map[string]any{"last_activity": h.lastAt.Format(time.RFC3339)}
		}
		return nil
	}
	if h.started == nil {
		return nil
	}
	quiet := h.joinedAt
	if h.started.After(quiet) {
		quiet = *h.started
	}
	if at.Sub(quiet) >= span {
		return map[string]any{"last_activity": nil, "quiet_since": quiet.UTC().Format(time.RFC3339)}
	}
	return nil
}
