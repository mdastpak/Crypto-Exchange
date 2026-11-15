package delisting

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/price"

	"github.com/shopspring/decimal"
)

// The settlement of the balances that are left once a currency is frozen.
// A delisting that converts them records its price once, when the
// settlement starts: the fixed price of the plan, or the reference price of
// the price engine (price.Reader, which an administrator's override comes
// first in) at that time. Then every available balance of the currency is
// converted at that price into the settlement currency, each balance in a
// transaction of its own that claims it with FOR UPDATE SKIP LOCKED, by the
// scheduler of every instance. The entry is a ledger conversion that refers
// to the delisting, whose key is the balance and its version, so a balance
// converts once. Locked funds and pending deposits convert once they are
// available. The delisted step waits until no balance of the currency is
// left, so a delisting that holds the balances for a claim stays frozen
// until they are claimed.

// RefDelisting is the type of the reference of the ledger entries of a
// delisting: its conversions, by the hk of the delisting.
const RefDelisting = "currency_delisting"

// Prices are the reference prices (price.Reader), which fail closed.
type Prices interface {
	Get(ctx context.Context, market string) (price.Price, error)
}

// Settler runs the delistings with their settlement. Prices may be nil: a
// conversion at the reference price then waits for one.
type Settler struct {
	Prices Prices
}

// Advance applies the steps that are due (Advance), records the prices of
// the settlements that start, and converts at most limit balances. When it
// converted some, it applies the steps again, so that a delisting whose
// last balance it converted reaches delisted in the same run. It is a
// currencies.Advancer.
func (s Settler) Advance(ctx context.Context, db *sql.DB, now time.Time, limit int) ([]Change, error) {
	changes, err := Advance(ctx, db, now, limit)
	if err != nil {
		return changes, err
	}
	if err := s.RecordPrices(ctx, db, now); err != nil {
		return changes, err
	}
	converted, err := SettleHoldings(ctx, db, limit)
	if err != nil || converted == 0 {
		return changes, err
	}
	more, err := Advance(ctx, db, now, limit)
	return append(changes, more...), err
}

// startingSettlements are the conversions of the delistings of frozen
// currencies whose price is not recorded yet.
const startingSettlements = `
	SELECT d.hk, d.currency_hk, d.price_source, COALESCE(trim_scale(d.fixed_price)::text, ''),
		COALESCE(c.info->>'symbol', ''), COALESCE(t.info->>'symbol', '')
	FROM currency_delistings d
	JOIN currencies c ON c.hk = d.currency_hk
	JOIN currencies t ON t.hk = d.settlement_currency_hk
	WHERE d.status = 'scheduled' AND d.settlement = 'convert' AND d.settlement_price IS NULL
	  AND c.lifecycle IN ('frozen', 'delisted')
	ORDER BY d.hk LIMIT 100`

type starting struct {
	hk, currencyHK      string
	source              PriceSource
	fixed, symbol, into string
}

// RecordPrices records the price of each conversion that starts, once on
// every instance. A reference price that is not live is not recorded: the
// settlement waits, and an administrator can set the price of the market
// (price overrides, #59).
func (s Settler) RecordPrices(ctx context.Context, db *sql.DB, now time.Time) error {
	rows, err := db.QueryContext(ctx, startingSettlements)
	if err != nil {
		return fmt.Errorf("failed to read the settlements that start: %w", err)
	}
	var due []starting
	for rows.Next() {
		var st starting
		if err := rows.Scan(&st.hk, &st.currencyHK, &st.source, &st.fixed, &st.symbol, &st.into); err != nil {
			rows.Close()
			return fmt.Errorf("failed to read the settlements that start: %w", err)
		}
		due = append(due, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read the settlements that start: %w", err)
	}
	for _, st := range due {
		value, detail, err := s.price(ctx, st)
		if err != nil {
			log.Printf("The settlement of delisting %s waits for a price: %v", st.hk, err)
			continue
		}
		if err := recordPrice(ctx, db, st, value, detail, now); err != nil {
			return err
		}
	}
	return nil
}

// price returns the price of a settlement and where it came from.
func (s Settler) price(ctx context.Context, st starting) (decimal.Decimal, string, error) {
	if st.source == PriceFixed {
		d, err := decimal.NewFromString(st.fixed)
		if err != nil || !d.IsPositive() {
			return decimal.Zero, "", fmt.Errorf("the fixed price %q: %w", st.fixed, err)
		}
		return d, "fixed", nil
	}
	if s.Prices == nil {
		return decimal.Zero, "", errors.New("no reference prices on this instance")
	}
	market := strings.ToUpper(st.symbol) + "/" + strings.ToUpper(st.into)
	p, err := s.Prices.Get(ctx, market)
	if err != nil {
		return decimal.Zero, "", fmt.Errorf("the price of %s: %w", market, err)
	}
	value, how := p.Mid.Decimal(), "mid"
	if st.source == PriceTWAP {
		// The longest window, the steadiest price.
		var longest time.Duration
		for w, v := range p.TWAP {
			if d, err := time.ParseDuration(w); err == nil && d > longest && v.Decimal().IsPositive() {
				longest, value, how = d, v.Decimal(), "twap "+w
			}
		}
		if longest == 0 {
			return decimal.Zero, "", fmt.Errorf("the price of %s has no TWAP", market)
		}
	}
	// There are no trades of the exchange yet (#62): the last trade is the
	// reference mid, the median of the venues.
	if !value.IsPositive() {
		return decimal.Zero, "", fmt.Errorf("the price of %s is not positive", market)
	}
	return value, fmt.Sprintf("%s of %s (%s), %s", how, market, p.Status, p.At.UTC().Format(time.RFC3339)), nil
}

func recordPrice(ctx context.Context, db *sql.DB, st starting, value decimal.Decimal, detail string, now time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to record a settlement price: %w", err)
	}
	defer tx.Rollback()
	if len(detail) > 200 {
		detail = detail[:200]
	}
	res, err := tx.ExecContext(ctx, `UPDATE currency_delistings SET settlement_price = $2, settlement_price_detail = $3,
		settlement_priced_at = $4 WHERE hk = $1 AND settlement_price IS NULL AND status = 'scheduled'`,
		st.hk, value.String(), detail, now)
	if err != nil {
		return fmt.Errorf("failed to record a settlement price: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		// Another instance recorded it first.
		return err
	}
	e := audit.Of(audit.Event{}, audit.ActionCurrencyDelistingPrice, audit.TargetCurrency, st.currencyHK)
	e.Changes = &audit.Changes{After: map[string]any{"delisting": st.hk, "price": value.String(), "price_source": st.source,
		"detail": detail}}
	if err := audit.Record(ctx, tx, e); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to record a settlement price: %w", err)
	}
	return nil
}

// SettleHoldings converts the available balances of the frozen currencies
// whose settlement price is recorded, at most limit of them.
func SettleHoldings(ctx context.Context, db *sql.DB, limit int) (int, error) {
	settled := 0
	for settled < limit {
		ok, err := settleOne(ctx, db)
		if err != nil {
			return settled, err
		}
		if !ok {
			break
		}
		settled++
	}
	return settled, nil
}

func settleOne(ctx context.Context, db *sql.DB) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("failed to start a settlement: %w", err)
	}
	defer tx.Rollback()
	// The balance of the frozen currency first, then the ledger takes the
	// settlement currency's: no other operation takes both, since a frozen
	// currency neither trades nor moves. FOR NO KEY UPDATE, as the ledger
	// locks balances.
	var delistingHK, source, target, rate, balanceID, user string
	var sub sql.NullString
	var available shared.Decimal
	var version int
	err = tx.QueryRowContext(ctx, `
		SELECT d.hk, d.currency_hk, d.settlement_currency_hk, d.settlement_price::text, b.id, b.user_id, b.sub_account_id,
			b.available, b.version
		FROM balances b
		JOIN currency_delistings d ON d.currency_hk = b.currency_hk
		WHERE d.status = 'scheduled' AND d.settlement = 'convert' AND d.settlement_price IS NOT NULL AND b.available::numeric > 0
		ORDER BY b.id
		FOR NO KEY UPDATE OF b SKIP LOCKED LIMIT 1`).
		Scan(&delistingHK, &source, &target, &rate, &balanceID, &user, &sub, &available, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to claim a balance: %w", err)
	}
	r, err := decimal.NewFromString(rate)
	if err != nil {
		return false, fmt.Errorf("the settlement price of delisting %s: %w", delistingHK, err)
	}
	var a ledger.Account
	if a.UserHK, err = shared.UUIDFromString(user); err != nil {
		return false, err
	}
	if sub.Valid {
		id, err := shared.UUIDFromString(sub.String)
		if err != nil {
			return false, err
		}
		a.SubAccountID = &id
	}
	from, err := shared.UUIDFromString(source)
	if err != nil {
		return false, err
	}
	into, err := shared.UUIDFromString(target)
	if err != nil {
		return false, err
	}
	precision, err := ledger.Precision(ctx, tx, target)
	if err != nil {
		return false, err
	}
	credit, dust := ledger.Split(available.Decimal(), r, precision)
	key := fmt.Sprintf("%s:%s:%s:v%d", RefDelisting, delistingHK, balanceID, version)
	if _, err := (ledger.Ledger{}).In(tx).Convert(ctx, key, a, ledger.Conversion{
		From: ledger.Amount{Currency: from, Value: available},
		To:   ledger.Amount{Currency: into, Value: shared.Decimal(credit)},
		Dust: shared.Decimal(dust),
	}, ledger.Ref{Type: RefDelisting, ID: delistingHK}); err != nil {
		return false, fmt.Errorf("failed to settle balance %s: %w", balanceID, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("failed to commit a settlement: %w", err)
	}
	return true, nil
}

// SettlementReport is how far the settlement of a delisting has gone, and
// what it would credit.
type SettlementReport struct {
	Settlement Settlement `json:"settlement" enums:"convert,claim"`
	// Holders are the balances of the currency that are left, available,
	// locked, or pending, and Remaining their sum, of which Available
	// converts now.
	Holders   int    `json:"holders"`
	Remaining string `json:"remaining" example:"1250.5"`
	Available string `json:"available" example:"1200"`
	// Price is the recorded price of a conversion, with where it came from
	// and when; before it is recorded, the fixed price of the plan, if any.
	Price         *string    `json:"price,omitempty" example:"0.0125"`
	PriceDetail   string     `json:"price_detail,omitempty" example:"twap 300s of XYZ/USDT (live), 2026-12-08T00:00:00Z"`
	PricedAt      *time.Time `json:"priced_at,omitempty"`
	PriceRecorded bool       `json:"price_recorded"`
	// Estimated is what Remaining would credit at Price, before the
	// rounding of each balance.
	Estimated *string `json:"estimated,omitempty" example:"15.63125"`
	// Conversions are the balances converted, Converted what left them,
	// Credited what the settlement currency received, and Dust what the
	// exchange kept.
	Conversions int    `json:"conversions"`
	Converted   string `json:"converted" example:"50.5"`
	Credited    string `json:"credited" example:"0.63"`
	Dust        string `json:"dust" example:"0.00125"`
}

// Report returns the settlement of a delisting of a currency, or
// ErrNotFound: a preview while it is scheduled, and its progress from
// frozen on.
func Report(ctx context.Context, db *sql.DB, currencyHK, hk string) (SettlementReport, error) {
	var r SettlementReport
	var recorded, fixed sql.NullString
	err := db.QueryRowContext(ctx, `
		WITH conv AS (
			SELECT e.id, d.amount::numeric AS debit
			FROM ledger_entries e JOIN balance_changes d ON d.entry_id = e.id AND d.change_type = 'CONVERSION_DEBIT'
			WHERE e.kind = 'conversion' AND e.reference_type = $3 AND e.reference_id = $2::text)
		SELECT x.settlement, trim_scale(x.settlement_price)::text, COALESCE(x.settlement_price_detail, ''), x.settlement_priced_at,
			trim_scale(x.fixed_price)::text,
			(SELECT count(*) FROM balances b WHERE b.currency_hk = x.currency_hk
				AND b.available::numeric + b.locked::numeric + b.pending_deposit::numeric <> 0),
			(SELECT COALESCE(sum(b.available::numeric + b.locked::numeric + b.pending_deposit::numeric), 0)::text
				FROM balances b WHERE b.currency_hk = x.currency_hk),
			(SELECT COALESCE(sum(b.available::numeric), 0)::text FROM balances b WHERE b.currency_hk = x.currency_hk),
			(SELECT count(*) FROM conv),
			(SELECT COALESCE(sum(c.debit), 0)::text FROM conv c),
			(SELECT COALESCE(sum(abs(y.amount::numeric)), 0)::text FROM conv c
				JOIN balance_changes y ON y.entry_id = c.id AND y.change_type = 'CONVERSION_CREDIT'),
			(SELECT COALESCE(sum(p.amount::numeric), 0)::text FROM conv c
				JOIN ledger_postings p ON p.entry_id = c.id AND p.account = 'fees')
		FROM currency_delistings x WHERE x.currency_hk = $1::uuid AND x.hk = $2::uuid`, currencyHK, hk, RefDelisting).
		Scan(&r.Settlement, &recorded, &r.PriceDetail, &r.PricedAt, &fixed, &r.Holders, &r.Remaining, &r.Available,
			&r.Conversions, &r.Converted, &r.Credited, &r.Dust)
	if errors.Is(err, sql.ErrNoRows) {
		return SettlementReport{}, ErrNotFound
	}
	if err != nil {
		return SettlementReport{}, fmt.Errorf("failed to read the settlement: %w", err)
	}
	for _, f := range []*string{&r.Remaining, &r.Available, &r.Converted, &r.Credited, &r.Dust} {
		if d, err := decimal.NewFromString(*f); err == nil {
			*f = d.String()
		}
	}
	switch {
	case recorded.Valid:
		r.Price, r.PriceRecorded = &recorded.String, true
	case fixed.Valid:
		r.Price = &fixed.String
	}
	if r.Price != nil {
		if p, err := decimal.NewFromString(*r.Price); err == nil {
			if left, err := decimal.NewFromString(r.Remaining); err == nil {
				est := left.Mul(p).Truncate(ledger.MaxConversionPlaces).String()
				r.Estimated = &est
			}
		}
	}
	return r, nil
}
