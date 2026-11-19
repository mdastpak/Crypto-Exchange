package aml

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mdastpak/crypto-exchange/internal/price"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// Event is an operation that the rules evaluate.
type Event struct {
	Kind Kind
	// Ref names the operation among those of its kind, such as the id of
	// a withdrawal.
	Ref        string
	UserHK     string
	CurrencyHK string
	// Symbol is the symbol of the currency, which its price is read by.
	Symbol string
	Amount decimal.Decimal
	// IPCountry is the country of the request, and CounterpartyCountry
	// that of the other side, such as the bank of a fiat destination; ""
	// when not known.
	IPCountry           string
	CounterpartyCountry string
	// Counterparty names the other side, such as a destination of the
	// user's book, and CounterpartySince is when the user added it.
	Counterparty      string
	CounterpartySince *time.Time
	// CounterpartyAddress is the address on chain of the other side, such
	// as that of a crypto destination, which the sanctions lists screen;
	// "" for a fiat one.
	CounterpartyAddress string
	// CounterpartyNetwork is the network of that address.
	CounterpartyNetwork string
}

// Hit is a rule that hit an operation, and what it asks.
type Hit struct {
	Rule   Stored
	Action Action
	// Data is what the rule saw, for the alert.
	Data map[string]any
}

// Decision is the evaluation of an operation: the hits of the rules, and
// the strongest of their actions.
type Decision struct {
	Event   Event
	At      time.Time
	Value   *decimal.Decimal
	EventID string
	Hits    []Hit
	Action  Action
}

// Prices reads the current prices: *price.Reader.
type Prices interface {
	Get(ctx context.Context, market string) (price.Price, error)
}

// Screener asks a screening provider about an operation for a rule of a
// screening type (#73): the action to take, ActionNone when nothing
// matched, and what it matched. It may take less than the rule's action,
// such as a review for a match that a person must confirm. A rule that it
// cannot screen, as no provider of the rule is configured, is evaluated by
// none: Screens reports it.
type Screener interface {
	Screen(ctx context.Context, q Querier, r Stored, e Event) (Action, map[string]any, error)
	Screens(r Rule) bool
}

// Engine evaluates the rules.
type Engine struct {
	Prices   Prices
	Screener Screener
	Notifier Notifier
	// Now is the clock of the evaluations; nil is time.Now.
	Now func() time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

// ErrUnavailable is an evaluation that could not run, such as a rule
// changed in SQL: the operation is refused until it can.
var ErrUnavailable = errors.New("aml: the rules cannot be evaluated")

// Evaluate evaluates the rules in force that apply to ev, in tx, the
// transaction of the operation, and records ev there, so that the windows
// of the next evaluations count it once the operation commits. It decides
// nothing about the operation itself: the caller holds or refuses it by
// the decision, and records its alerts with Record or Enforce.
func (e *Engine) Evaluate(ctx context.Context, tx *sql.Tx, ev Event) (Decision, error) {
	d := Decision{Event: ev, At: e.now()}
	rules, err := Rules(ctx, tx)
	if err != nil {
		return d, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if v, ok := e.valueOf(ctx, ev.Symbol, ev.Amount); ok {
		d.Value = &v
	}
	d.EventID, err = insertEvent(ctx, tx, d, false)
	if err != nil {
		return d, err
	}
	h, err := e.history(ctx, tx, ev.UserHK, d.EventID, d.At, lookback(rules, ev.Kind))
	if err != nil {
		return d, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	cur := past{kind: ev.Kind, value: d.Value, at: d.At, ipCountry: ev.IPCountry,
		counterpartyCountry: ev.CounterpartyCountry, counterparty: ev.Counterparty}
	for _, r := range rules {
		if !r.Applies(ev.Kind) {
			continue
		}
		var hit *Hit
		switch {
		case r.Type.Screening():
			hit, err = e.screen(ctx, tx, r, ev)
		default:
			hit, err = evaluate(ctx, tx, r, ev, cur, h, d.At)
		}
		if err != nil {
			return d, fmt.Errorf("%w: rule %s: %w", ErrUnavailable, r.Code, err)
		}
		if hit != nil {
			d.Hits = append(d.Hits, *hit)
			d.Action = d.Action.Stronger(hit.Action)
		}
	}
	return d, nil
}

// screen asks the screener for a screening rule; without one, the rule is
// not evaluated, as the startup warned.
func (e *Engine) screen(ctx context.Context, q Querier, r Stored, ev Event) (*Hit, error) {
	if e.Screener == nil {
		return nil, nil
	}
	if !e.Screener.Screens(r.Rule) {
		return nil, nil
	}
	action, data, err := e.Screener.Screen(ctx, q, r, ev)
	if err != nil || action == ActionNone {
		return nil, err
	}
	return &Hit{Rule: r, Action: action, Data: data}, nil
}

// valueOf values amount of symbol in USDT, at the mid of a live price of
// SYMBOL/USDT, or of USDT/SYMBOL inverted. false is no current price.
func (e *Engine) valueOf(ctx context.Context, symbol string, amount decimal.Decimal) (decimal.Decimal, bool) {
	if symbol == "USDT" {
		return amount, true
	}
	if e.Prices == nil || symbol == "" {
		return decimal.Zero, false
	}
	if p, err := e.Prices.Get(ctx, symbol+"/USDT"); err == nil && p.Live() && p.Mid.Decimal().IsPositive() {
		return amount.Mul(p.Mid.Decimal()), true
	}
	if p, err := e.Prices.Get(ctx, "USDT/"+symbol); err == nil && p.Live() && p.Mid.Decimal().IsPositive() {
		return amount.Div(p.Mid.Decimal()), true
	}
	return decimal.Zero, false
}

// past is an operation of a window.
type past struct {
	kind                           Kind
	value                          *decimal.Decimal
	at                             time.Time
	ipCountry, counterpartyCountry string
	counterparty                   string
}

// history holds what the rules of an evaluation read: the user's
// operations since the start of the longest lookback, without the one
// evaluated, the last one before that start, and when the user joined.
type history struct {
	events   []past
	lastAt   *time.Time
	joinedAt time.Time
	// started is when the engine recorded its first operation of anyone.
	started *time.Time
}

// lookback is the longest span that a rule for kind reads.
func lookback(rules []Stored, kind Kind) time.Duration {
	var d time.Duration
	for _, r := range rules {
		if !r.Applies(kind) {
			continue
		}
		span := time.Duration(r.WindowHours) * time.Hour
		if h := time.Duration(r.Config.HistoryDays) * 24 * time.Hour; h > span {
			span = h
		}
		d = max(d, span)
	}
	return d
}

// history reads the user's operations from at-span, without eventID, each
// checked against its seal; an operation without a value is valued at the
// current price of its currency, or stays without one.
func (e *Engine) history(ctx context.Context, q Querier, userHK, eventID string, at time.Time, span time.Duration) (history, error) {
	var h history
	if err := q.QueryRowContext(ctx, `SELECT created_at FROM users WHERE hk = $1`, userHK).Scan(&h.joinedAt); err != nil {
		return h, fmt.Errorf("aml: read the user: %w", err)
	}
	var last sql.NullTime
	if err := q.QueryRowContext(ctx, `SELECT max(occurred_at) FROM aml_events
		WHERE user_hk = $1 AND NOT refused AND id <> $2 AND occurred_at <= $3`, userHK, eventID, at).Scan(&last); err != nil {
		return h, fmt.Errorf("aml: read the user's operations: %w", err)
	}
	if last.Valid {
		t := last.Time.UTC()
		h.lastAt = &t
	}
	var started sql.NullTime
	if err := q.QueryRowContext(ctx, `SELECT min(occurred_at) FROM aml_events WHERE id <> $1`, eventID).Scan(&started); err != nil {
		return h, fmt.Errorf("aml: read the first operation: %w", err)
	}
	if started.Valid {
		t := started.Time.UTC()
		h.started = &t
	}
	t := rowseal.AMLEvents
	rows, err := q.QueryContext(ctx, `SELECT e.amount::numeric, e.value_usdt::numeric, e.occurred_at, c.info->>'symbol', `+t.Fields("e")+`
		FROM aml_events e JOIN currencies c ON c.hk = e.currency_hk
		WHERE e.user_hk = $1 AND NOT e.refused AND e.id <> $2 AND e.occurred_at > $3 AND e.occurred_at <= $4
		ORDER BY e.occurred_at`, userHK, eventID, at.Add(-span), at)
	if err != nil {
		return h, fmt.Errorf("aml: read the user's operations: %w", err)
	}
	defer rows.Close()
	type read struct {
		amount decimal.Decimal
		value  decimal.NullDecimal
		at     time.Time
		symbol string
		row    *rowseal.Row
	}
	var reads []read
	for rows.Next() {
		x := read{row: t.NewRow()}
		if err := rows.Scan(append([]any{&x.amount, &x.value, &x.at, &x.symbol}, x.row.Dest()...)...); err != nil {
			return h, fmt.Errorf("aml: read the user's operations: %w", err)
		}
		reads = append(reads, x)
	}
	if err := rows.Err(); err != nil {
		return h, fmt.Errorf("aml: read the user's operations: %w", err)
	}
	rows.Close()
	for _, x := range reads {
		if err := rowseal.Require(ctx, t, x.row); err != nil {
			return h, err
		}
		col := func(name string) string { return x.row.Values[t.Index(name)].String }
		p := past{kind: Kind(col("kind")), at: x.at.UTC(), ipCountry: col("ip_country"),
			counterpartyCountry: col("counterparty_country"), counterparty: col("counterparty")}
		switch {
		case x.value.Valid:
			v := x.value.Decimal
			p.value = &v
		default:
			if v, ok := e.valueOf(ctx, x.symbol, x.amount); ok {
				p.value = &v
			}
		}
		h.events = append(h.events, p)
	}
	return h, nil
}

// insertEvent records the operation of d, refused or not, and returns its
// id.
func insertEvent(ctx context.Context, q Querier, d Decision, refused bool) (string, error) {
	ev := d.Event
	var value any
	if d.Value != nil {
		value = d.Value.String()
	}
	var id string
	err := q.QueryRowContext(ctx, `INSERT INTO aml_events (user_hk, kind, ref, currency_hk, amount, value_usdt, counterparty,
			ip_country, counterparty_country, occurred_at, refused)
		VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric, NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''), $10, $11) RETURNING id`,
		ev.UserHK, string(ev.Kind), ev.Ref, ev.CurrencyHK, ev.Amount.String(), value, ev.Counterparty,
		ev.IPCountry, ev.CounterpartyCountry, d.At, refused).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("aml: record the operation: %w", err)
	}
	if err := rowseal.Reseal(ctx, q, rowseal.AMLEvents, "id = $1", id); err != nil {
		return "", err
	}
	return id, nil
}
