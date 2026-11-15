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
	"github.com/mdastpak/crypto-exchange/internal/convhold"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/price"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

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
//
//nolint:gosec // G202: the sealed columns are constants of rowseal.
var startingSettlements = `
	SELECT d.hk, d.currency_hk, d.price_source, COALESCE(trim_scale(d.fixed_price)::text, ''),
		COALESCE(c.info->>'symbol', ''), COALESCE(t.info->>'symbol', ''), ` + rowseal.CurrencyDelistings.Fields("d") + `
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
	// sealed is the row of the delisting, read with the rest, whose seal
	// covers the settlement currency and the fixed price (#451).
	sealed *rowseal.Row
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
		st := starting{sealed: rowseal.CurrencyDelistings.NewRow()}
		dest := append([]any{&st.hk, &st.currencyHK, &st.source, &st.fixed, &st.symbol, &st.into}, st.sealed.Dest()...)
		if err := rows.Scan(dest...); err != nil {
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
		// A delisting that fails its seal records no price, and so
		// converts nothing, until its finding is resolved.
		if err := rowseal.Require(ctx, rowseal.CurrencyDelistings, st.sealed); err != nil {
			log.Printf("SECURITY: the settlement of delisting %s waits until its finding is resolved: %v", st.hk, err)
			continue
		}
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
	// The row is checked again under its lock: the seal made with the
	// price must not adopt a change made in SQL since it was read.
	locked := rowseal.CurrencyDelistings.NewRow()
	if err := tx.QueryRowContext(ctx, `SELECT `+rowseal.CurrencyDelistings.Fields("")+` FROM currency_delistings WHERE hk = $1 FOR UPDATE`,
		st.hk).Scan(locked.Dest()...); err != nil {
		return fmt.Errorf("failed to lock the delisting: %w", err)
	}
	if err := rowseal.Require(ctx, rowseal.CurrencyDelistings, locked); err != nil {
		log.Printf("SECURITY: the settlement of delisting %s waits until its finding is resolved: %v", st.hk, err)
		return nil
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
	if err := rowseal.Reseal(ctx, tx, rowseal.CurrencyDelistings, "hk = $1::uuid", st.hk); err != nil {
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
	// The delistings that fail their seal in this run (#451): their
	// balances wait, and the others convert. A balance that fails to
	// convert is held (#452), and counts toward the limit.
	skip := []string{}
	for tried := 0; tried < limit; {
		out, refused, err := settleOne(ctx, db, skip)
		if err != nil {
			return settled, err
		}
		switch out {
		case outcomeNone:
			return settled, nil
		case outcomeRefused:
			skip = append(skip, refused)
			continue
		case outcomeSettled:
			settled++
		}
		tried++
	}
	return settled, nil
}

// The outcomes of the settlement of one balance.
type outcome int

const (
	// outcomeNone is no balance left to claim.
	outcomeNone outcome = iota
	// outcomeSettled is a balance converted.
	outcomeSettled
	// outcomeHeld is a balance whose conversion failed, held for a retry.
	outcomeHeld
	// outcomeRefused is a delisting that fails its seal.
	outcomeRefused
)

// claimed is a balance that a settlement claimed.
type claimed struct {
	delistingHK, source, target, rate, balanceID, user string
	sub                                                sql.NullString
	available                                          shared.Decimal
	version                                            int
}

// settleOne converts one balance of a delisting that is not in skip, the
// lowest that is not held (#452). A delisting that fails its seal converts
// nothing: settleOne returns it, and its finding is recorded. A balance
// whose conversion fails is held, so that the next claim takes the next
// one.
func settleOne(ctx context.Context, db *sql.DB, skip []string) (outcome, string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return outcomeNone, "", fmt.Errorf("failed to start a settlement: %w", err)
	}
	defer tx.Rollback()
	// The balance of the frozen currency first, then the ledger takes the
	// settlement currency's: no other operation takes both, since a frozen
	// currency neither trades nor moves. FOR NO KEY UPDATE, as the ledger
	// locks balances. The sealed row of the delisting is read in the same
	// statement as the price and the settlement currency, so the seal is
	// checked against the values that convert.
	var c claimed
	sealed := rowseal.CurrencyDelistings.NewRow()
	err = tx.QueryRowContext(ctx, `
		SELECT d.hk, d.currency_hk, d.settlement_currency_hk, d.settlement_price::text, b.id, b.user_id, b.sub_account_id,
			b.available, b.version, `+rowseal.CurrencyDelistings.Fields("d")+`
		FROM balances b
		JOIN currency_delistings d ON d.currency_hk = b.currency_hk
		WHERE d.status = 'scheduled' AND d.settlement = 'convert' AND d.settlement_price IS NOT NULL AND b.available::numeric > 0
		  AND NOT (d.hk::text = ANY(string_to_array($1, ',')))
		  AND `+fmt.Sprintf(convhold.Skip, convhold.KindDelisting, "d.hk", "b.id")+`
		ORDER BY b.id
		FOR NO KEY UPDATE OF b SKIP LOCKED LIMIT 1`, strings.Join(skip, ",")).
		Scan(append([]any{&c.delistingHK, &c.source, &c.target, &c.rate, &c.balanceID, &c.user, &c.sub, &c.available, &c.version},
			sealed.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return outcomeNone, "", nil
	}
	if err != nil {
		return outcomeNone, "", fmt.Errorf("failed to claim a balance: %w", err)
	}
	if err := rowseal.Require(ctx, rowseal.CurrencyDelistings, sealed); err != nil {
		log.Printf("SECURITY: delisting %s converts nothing until its finding is resolved: %v", c.delistingHK, err)
		return outcomeRefused, c.delistingHK, nil
	}
	if err := settle(ctx, tx, c); err != nil {
		_ = tx.Rollback()
		// A cancelled context is not the balance's failure.
		if ctx.Err() != nil {
			return outcomeNone, "", err
		}
		if _, herr := convhold.Hold(ctx, db, convhold.KindDelisting, c.delistingHK, c.balanceID, c.source, err); herr != nil {
			return outcomeNone, "", fmt.Errorf("%w (while settling: %w)", herr, err)
		}
		return outcomeHeld, "", nil
	}
	if err := convhold.Release(ctx, tx, convhold.KindDelisting, c.delistingHK, c.balanceID); err != nil {
		return outcomeNone, "", err
	}
	if err := tx.Commit(); err != nil {
		return outcomeNone, "", fmt.Errorf("failed to commit a settlement: %w", err)
	}
	return outcomeSettled, "", nil
}

// settle converts the claimed balance c in tx.
func settle(ctx context.Context, tx *sql.Tx, c claimed) error {
	delistingHK, source, target, rate, balanceID, user := c.delistingHK, c.source, c.target, c.rate, c.balanceID, c.user
	sub, available, version := c.sub, c.available, c.version
	r, err := decimal.NewFromString(rate)
	if err != nil {
		return fmt.Errorf("the settlement price of delisting %s: %w", delistingHK, err)
	}
	var a ledger.Account
	if a.UserHK, err = shared.UUIDFromString(user); err != nil {
		return err
	}
	if sub.Valid {
		id, err := shared.UUIDFromString(sub.String)
		if err != nil {
			return err
		}
		a.SubAccountID = &id
	}
	from, err := shared.UUIDFromString(source)
	if err != nil {
		return err
	}
	into, err := shared.UUIDFromString(target)
	if err != nil {
		return err
	}
	precision, err := ledger.Precision(ctx, tx, target)
	if err != nil {
		return err
	}
	credit, dust := ledger.Split(available.Decimal(), r, precision)
	key := fmt.Sprintf("%s:%s:%s:v%d", RefDelisting, delistingHK, balanceID, version)
	if _, err := (ledger.Ledger{}).In(tx).Convert(ctx, key, a, ledger.Conversion{
		From: ledger.Amount{Currency: from, Value: available},
		To:   ledger.Amount{Currency: into, Value: shared.Decimal(credit)},
		Dust: shared.Decimal(dust),
	}, ledger.Ref{Type: RefDelisting, ID: delistingHK}); err != nil {
		return fmt.Errorf("failed to settle balance %s: %w", balanceID, err)
	}
	return nil
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
	// Unverified is a delisting that fails its seal (#451): it converts
	// nothing until an administrator reviews its integrity finding and
	// re-seals it.
	Unverified bool `json:"unverified,omitempty"`
	// Blocked are the balances whose conversion failed, held for a retry
	// (#452), and BlockedCodes how many failed with each code, such as
	// row_unverified. delisted waits until they too are converted.
	Blocked      int            `json:"blocked"`
	BlockedCodes map[string]int `json:"blocked_codes,omitempty"`
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
	sealed := rowseal.CurrencyDelistings.NewRow()
	if err := db.QueryRowContext(ctx, `SELECT `+rowseal.CurrencyDelistings.Fields("")+` FROM currency_delistings WHERE hk = $1::uuid`, hk).
		Scan(sealed.Dest()...); err != nil {
		return SettlementReport{}, fmt.Errorf("failed to read the settlement: %w", err)
	}
	r.Unverified = rowseal.Require(ctx, rowseal.CurrencyDelistings, sealed) != nil
	blocked, err := convhold.Summary(ctx, db, convhold.KindDelisting, hk)
	if err != nil {
		return SettlementReport{}, err
	}
	b := blocked[currencyHK]
	r.Blocked, r.BlockedCodes = b.Balances, b.Codes
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

// Retry makes the balances of a delisting of currencyHK whose conversion
// failed due now (#452), such as after a re-seal, and returns how many, or
// ErrNotFound. The next run of the scheduler converts them.
func Retry(ctx context.Context, db *sql.DB, currencyHK, hk string, origin audit.Event) (int, error) {
	var ok bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM currency_delistings WHERE hk = $1::uuid AND currency_hk = $2::uuid)`,
		hk, currencyHK).Scan(&ok); err != nil {
		return 0, fmt.Errorf("failed to read the delisting: %w", err)
	}
	if !ok {
		return 0, ErrNotFound
	}
	return convhold.Retry(ctx, db, convhold.KindDelisting, hk, currencyHK, origin)
}
