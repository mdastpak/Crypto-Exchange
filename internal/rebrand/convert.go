package rebrand

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"

	"github.com/shopspring/decimal"
)

// RefMigration is the type of the reference of the ledger entries of a
// migration: its conversions, by the hk of the migration.
const RefMigration = "currency_migration"

// defaultPrecision is the precision of a target without networks, and the
// most that a conversion keeps: the ledger's amounts have 18 decimal
// places at most from a migration.
const defaultPrecision = 18

// The refusals of a conversion that a user asks for.
var (
	// ErrNotConverting is a conversion of a migration that is not in its
	// converting phase, or that is forced: those convert by themselves.
	ErrNotConverting = errors.New("the migration does not offer a conversion now")
	// ErrNotASource is a conversion of a currency that is not a source of
	// the migration.
	ErrNotASource = errors.New("the currency is not a source of the migration")
	// ErrNothingToConvert is a conversion without an available balance of
	// the source.
	ErrNothingToConvert = errors.New("there is no available balance of the source")
)

// holding is a balance of a source that a conversion claimed.
type holding struct {
	migration, source, target string
	ratio                     decimal.Decimal
	balanceID                 string
	account                   ledger.Account
	available                 shared.Decimal
	version                   int
}

// Converted is what a conversion did.
type Converted struct {
	EntryID string `json:"entry_id" format:"uuid"`
	// Source and Amount are what left the account; Target and Credited
	// what entered it. Dust is the part under the precision of the target,
	// which the exchange keeps.
	Source   string `json:"source_hk" format:"uuid"`
	Amount   string `json:"amount" example:"150.5"`
	Target   string `json:"target_hk" format:"uuid"`
	Credited string `json:"credited" example:"1.5"`
	Dust     string `json:"dust" example:"0.005"`
}

// ConvertHoldings converts the available balances of the sources of forced
// migrations in their converting phase, at most limit of them. Each
// balance is claimed with FOR UPDATE SKIP LOCKED and converted in its own
// transaction, so instances share the work, and a balance converts once:
// its idempotency key is its id and version, and a converted balance has
// nothing available left. Locked funds and pending deposits convert once
// they are available, and the migration completes only when no balance of
// its sources is left.
func ConvertHoldings(ctx context.Context, db *sql.DB, limit int) (int, error) {
	converted := 0
	for converted < limit {
		ok, err := convertOne(ctx, db)
		if err != nil {
			return converted, err
		}
		if !ok {
			break
		}
		converted++
	}
	return converted, nil
}

func convertOne(ctx context.Context, db *sql.DB) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("failed to start a conversion: %w", err)
	}
	defer tx.Rollback()
	// The source balance first, then the ledger takes the target's: no
	// other operation takes both, since the sources of a forced migration
	// neither trade nor convert while it converts. FOR NO KEY UPDATE, as
	// the ledger locks balances (see lockBalances).
	h, err := claim(ctx, tx, `
		SELECT m.hk, s.currency_hk, m.target_hk, s.ratio::text, b.id, b.user_id, b.sub_account_id, b.available, b.version
		FROM balances b
		JOIN currency_migration_sources s ON s.currency_hk = b.currency_hk
		JOIN currency_migrations m ON m.hk = s.migration_hk
		WHERE m.forced AND m.phase = 'converting' AND b.available::numeric > 0
		ORDER BY b.id
		FOR NO KEY UPDATE OF b SKIP LOCKED LIMIT 1`)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := convert(ctx, tx, h); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("failed to commit a conversion: %w", err)
	}
	return true, nil
}

// claim reads and locks one holding.
func claim(ctx context.Context, tx *sql.Tx, query string, args ...any) (holding, error) {
	var h holding
	var ratio string
	var user string
	var sub sql.NullString
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&h.migration, &h.source, &h.target, &ratio, &h.balanceID,
		&user, &sub, &h.available, &h.version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return h, err
		}
		return h, fmt.Errorf("failed to claim a balance: %w", err)
	}
	var err error
	if h.ratio, err = decimal.NewFromString(ratio); err != nil {
		return h, fmt.Errorf("the ratio %q of migration %s: %w", ratio, h.migration, err)
	}
	if h.account.UserHK, err = shared.UUIDFromString(user); err != nil {
		return h, fmt.Errorf("the user of balance %s: %w", h.balanceID, err)
	}
	if sub.Valid {
		id, err := shared.UUIDFromString(sub.String)
		if err != nil {
			return h, fmt.Errorf("the sub-account of balance %s: %w", h.balanceID, err)
		}
		h.account.SubAccountID = &id
	}
	return h, nil
}

// Split returns what amount of a source becomes at ratio: the credit,
// rounded down to precision decimal places, and the dust under it. The
// product is exact to 18 places; what lies beyond is under any currency's
// unit and is not booked.
func Split(amount, ratio decimal.Decimal, precision int32) (credit, dust decimal.Decimal) {
	precision = min(max(precision, 0), defaultPrecision)
	exact := amount.Mul(ratio).Truncate(defaultPrecision)
	credit = exact.Truncate(precision)
	return credit, exact.Sub(credit)
}

// precisionOf returns the precision of a currency: the most decimal places
// of its networks, as many as a withdrawal can carry.
func precisionOf(ctx context.Context, tx *sql.Tx, currencyHK string) (int32, error) {
	var p sql.NullInt32
	if err := tx.QueryRowContext(ctx, `SELECT MAX(decimals) FROM currency_networks WHERE currency_hk = $1`, currencyHK).Scan(&p); err != nil {
		return 0, fmt.Errorf("failed to read the precision of %s: %w", currencyHK, err)
	}
	if !p.Valid {
		return defaultPrecision, nil
	}
	return p.Int32, nil
}

// convert converts the available balance of a claimed holding.
func convert(ctx context.Context, tx *sql.Tx, h holding) (Converted, error) {
	precision, err := precisionOf(ctx, tx, h.target)
	if err != nil {
		return Converted{}, err
	}
	credit, dust := Split(h.available.Decimal(), h.ratio, precision)
	source, err := shared.UUIDFromString(h.source)
	if err != nil {
		return Converted{}, err
	}
	target, err := shared.UUIDFromString(h.target)
	if err != nil {
		return Converted{}, err
	}
	key := fmt.Sprintf("%s:%s:%s:v%d", RefMigration, h.migration, h.balanceID, h.version)
	res, err := ledger.Ledger{}.In(tx).Convert(ctx, key, h.account, ledger.Conversion{
		From: ledger.Amount{Currency: source, Value: h.available},
		To:   ledger.Amount{Currency: target, Value: shared.Decimal(credit)},
		Dust: shared.Decimal(dust),
	}, ledger.Ref{Type: RefMigration, ID: h.migration})
	if err != nil {
		return Converted{}, fmt.Errorf("failed to convert balance %s: %w", h.balanceID, err)
	}
	return Converted{EntryID: res.EntryID, Source: h.source, Amount: h.available.String(), Target: h.target,
		Credited: credit.String(), Dust: dust.String()}, nil
}

// ConvertOwn converts what the account holds available of the source of
// an optional migration, while the migration is converting: the
// conversion that it offers, which the user asks for. The source must
// allow converting from it and the target converting into it, and no
// restriction of the user may block the conversion of either.
func ConvertOwn(ctx context.Context, db *sql.DB, migrationHK, sourceHK string, a ledger.Account) (Converted, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Converted{}, fmt.Errorf("failed to start a conversion: %w", err)
	}
	defer tx.Rollback()
	var target string
	err = tx.QueryRowContext(ctx, `SELECT target_hk FROM currency_migrations WHERE hk = $1`, migrationHK).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return Converted{}, ErrNotFound
	}
	if err != nil {
		return Converted{}, fmt.Errorf("failed to read the migration: %w", err)
	}
	user := a.UserHK.String()
	if err := restrictions.Lock(ctx, tx, user); err != nil {
		return Converted{}, err
	}
	// The currencies are share-locked in hk order, as a phase locks them
	// for update, and before the migration, which a phase locks after
	// them: a phase waits for the conversion, or the conversion for it.
	checks := []struct {
		hk string
		k  currencies.Capability
	}{{sourceHK, currencies.CapabilityConvertFrom}, {target, currencies.CapabilityConvertInto}}
	if target < sourceHK {
		checks[0], checks[1] = checks[1], checks[0]
	}
	for _, c := range checks {
		if err := restrictions.Check(ctx, tx, user, restrictions.OpConvert, c.hk); err != nil {
			return Converted{}, err
		}
		if err := currencies.Check(ctx, tx, c.hk, c.k, true); err != nil {
			return Converted{}, err
		}
	}
	var forced bool
	var phase Phase
	if err := tx.QueryRowContext(ctx, `SELECT forced, phase FROM currency_migrations WHERE hk = $1 FOR SHARE`, migrationHK).
		Scan(&forced, &phase); err != nil {
		return Converted{}, fmt.Errorf("failed to read the migration: %w", err)
	}
	if forced || phase != PhaseConverting {
		return Converted{}, ErrNotConverting
	}
	// The balance is read, not locked: the ledger locks the source and the
	// target in its own order, as a trade of the two would, and refuses the
	// conversion if a concurrent operation spent the funds in between.
	h := holding{migration: migrationHK, source: sourceHK, target: target, account: a}
	err = tx.QueryRowContext(ctx, `SELECT ratio FROM currency_migration_sources WHERE migration_hk = $1 AND currency_hk = $2`,
		migrationHK, sourceHK).Scan((*shared.Decimal)(&h.ratio))
	if errors.Is(err, sql.ErrNoRows) {
		return Converted{}, ErrNotASource
	}
	if err != nil {
		return Converted{}, fmt.Errorf("failed to read the source: %w", err)
	}
	err = tx.QueryRowContext(ctx, `SELECT id, available, version FROM balances
		WHERE user_id = $1 AND sub_account_id IS NOT DISTINCT FROM $2 AND currency_hk = $3`, a.UserHK, a.SubAccountID, sourceHK).
		Scan(&h.balanceID, &h.available, &h.version)
	if errors.Is(err, sql.ErrNoRows) {
		return Converted{}, ErrNothingToConvert
	}
	if err != nil {
		return Converted{}, fmt.Errorf("failed to read the balance: %w", err)
	}
	if !h.available.IsPositive() {
		return Converted{}, ErrNothingToConvert
	}
	out, err := convert(ctx, tx, h)
	if err != nil {
		return Converted{}, err
	}
	if err := tx.Commit(); err != nil {
		return Converted{}, fmt.Errorf("failed to commit the conversion: %w", err)
	}
	return out, nil
}

// SourceProgress is how far the conversion of a source has gone.
type SourceProgress struct {
	CurrencyHK string `json:"currency_hk" format:"uuid"`
	// Holders are the balances of the source that are left, available,
	// locked, or pending, and Remaining their sum.
	Holders   int    `json:"holders"`
	Remaining string `json:"remaining" example:"12.5"`
	// Conversions are the balances converted, Converted what left them,
	// Credited what the target received, and Dust what the exchange kept.
	Conversions int    `json:"conversions"`
	Converted   string `json:"converted" example:"150.5"`
	Credited    string `json:"credited" example:"1.5"`
	Dust        string `json:"dust" example:"0.005"`
}

// Progress returns the progress of the conversion of each source of a
// migration into targetHK, from the balances that are left and the ledger
// entries of its conversions, or ErrNotFound.
func Progress(ctx context.Context, db *sql.DB, targetHK, migrationHK string) ([]SourceProgress, error) {
	rows, err := db.QueryContext(ctx, `
		WITH conv AS (
			SELECT e.id, d.currency_hk AS source, d.amount::numeric AS debit
			FROM ledger_entries e JOIN balance_changes d ON d.entry_id = e.id AND d.change_type = 'CONVERSION_DEBIT'
			WHERE e.kind = 'conversion' AND e.reference_type = $2 AND e.reference_id = $1::text)
		SELECT s.currency_hk,
			(SELECT count(*) FROM balances b WHERE b.currency_hk = s.currency_hk
				AND b.available::numeric + b.locked::numeric + b.pending_deposit::numeric <> 0),
			(SELECT COALESCE(sum(b.available::numeric + b.locked::numeric + b.pending_deposit::numeric), 0)::text
				FROM balances b WHERE b.currency_hk = s.currency_hk),
			(SELECT count(*) FROM conv c WHERE c.source = s.currency_hk),
			(SELECT COALESCE(sum(c.debit), 0)::text FROM conv c WHERE c.source = s.currency_hk),
			(SELECT COALESCE(sum(abs(x.amount::numeric)), 0)::text FROM conv c
				JOIN balance_changes x ON x.entry_id = c.id AND x.change_type = 'CONVERSION_CREDIT' WHERE c.source = s.currency_hk),
			(SELECT COALESCE(sum(p.amount::numeric), 0)::text FROM conv c
				JOIN ledger_postings p ON p.entry_id = c.id AND p.account = 'fees' WHERE c.source = s.currency_hk)
		FROM currency_migration_sources s JOIN currency_migrations m ON m.hk = s.migration_hk AND m.target_hk = $3::uuid
		WHERE s.migration_hk = $1::uuid
		ORDER BY s.currency_hk`, migrationHK, RefMigration, targetHK)
	if err != nil {
		return nil, fmt.Errorf("failed to read the progress: %w", err)
	}
	defer rows.Close()
	out := []SourceProgress{}
	for rows.Next() {
		var p SourceProgress
		if err := rows.Scan(&p.CurrencyHK, &p.Holders, &p.Remaining, &p.Conversions, &p.Converted, &p.Credited, &p.Dust); err != nil {
			return nil, fmt.Errorf("failed to read the progress: %w", err)
		}
		for _, f := range []*string{&p.Remaining, &p.Converted, &p.Credited, &p.Dust} {
			if d, err := decimal.NewFromString(*f); err == nil {
				*f = d.String()
			}
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read the progress: %w", err)
	}
	if len(out) == 0 {
		// Every migration has a source.
		return nil, ErrNotFound
	}
	return out, nil
}
