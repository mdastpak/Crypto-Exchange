package rebrand

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/convhold"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/shopspring/decimal"
)

// RefMigration is the type of the reference of the ledger entries of a
// migration: its conversions, by the hk of the migration.
const RefMigration = "currency_migration"

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
	// The migrations that fail their seal in this run (#451): their
	// holdings wait, and the others convert. A balance that fails to
	// convert is held (#452), and counts toward the limit.
	skip := []string{}
	for tried := 0; tried < limit; {
		out, refused, err := convertOne(ctx, db, skip)
		if err != nil {
			return converted, err
		}
		switch out {
		case outcomeNone:
			return converted, nil
		case outcomeRefused:
			skip = append(skip, refused)
			continue
		case outcomeConverted:
			converted++
		}
		tried++
	}
	return converted, nil
}

// The outcomes of the conversion of one balance.
type outcome int

const (
	// outcomeNone is no balance left to claim.
	outcomeNone outcome = iota
	// outcomeConverted is a balance converted.
	outcomeConverted
	// outcomeHeld is a balance whose conversion failed, held for a retry.
	outcomeHeld
	// outcomeRefused is a migration that fails its seal.
	outcomeRefused
)

// convertOne converts one holding of a migration that is not in skip, the
// lowest balance that is not held (#452). A migration that fails its seal
// converts nothing: convertOne returns it, and its finding is recorded. A
// balance whose conversion fails is held, so that the next claim takes the
// next one.
func convertOne(ctx context.Context, db *sql.DB, skip []string) (outcome, string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return outcomeNone, "", fmt.Errorf("failed to start a conversion: %w", err)
	}
	defer tx.Rollback()
	// The source balance first, then the ledger takes the target's: no
	// other operation takes both, since the sources of a forced migration
	// neither trade nor convert while it converts. FOR NO KEY UPDATE, as
	// the ledger locks balances (see lockBalances). The sealed row of the
	// migration is read in the same statement as the ratio and the target,
	// so the seal is checked against the values that convert.
	sealed := rowseal.CurrencyMigrations.NewRow()
	h, err := claim(ctx, tx, `
		SELECT m.hk, s.currency_hk, m.target_hk, s.ratio::text, b.id, b.user_id, b.sub_account_id, b.available, b.version,
			`+rowseal.CurrencyMigrations.Fields("m")+`
		FROM balances b
		JOIN currency_migration_sources s ON s.currency_hk = b.currency_hk
		JOIN currency_migrations m ON m.hk = s.migration_hk
		WHERE m.forced AND m.phase = 'converting' AND b.available::numeric > 0
		  AND NOT (m.hk::text = ANY(string_to_array($1, ',')))
		  AND `+fmt.Sprintf(convhold.Skip, convhold.KindMigration, "m.hk", "b.id")+`
		ORDER BY b.id
		FOR NO KEY UPDATE OF b SKIP LOCKED LIMIT 1`, []any{joinHKs(skip)}, sealed.Dest()...)
	if errors.Is(err, sql.ErrNoRows) {
		return outcomeNone, "", nil
	}
	if err != nil {
		if h.balanceID == "" {
			return outcomeNone, "", err
		}
		return hold(ctx, db, tx, h, err)
	}
	if err := rowseal.Require(ctx, rowseal.CurrencyMigrations, sealed); err != nil {
		log.Printf("SECURITY: migration %s converts nothing until its finding is resolved: %v", h.migration, err)
		return outcomeRefused, h.migration, nil
	}
	if _, err := convert(ctx, tx, h); err != nil {
		return hold(ctx, db, tx, h, err)
	}
	if err := convhold.Release(ctx, tx, convhold.KindMigration, h.migration, h.balanceID); err != nil {
		return outcomeNone, "", err
	}
	if err := tx.Commit(); err != nil {
		return outcomeNone, "", fmt.Errorf("failed to commit a conversion: %w", err)
	}
	return outcomeConverted, "", nil
}

// hold rolls back the conversion of h that failed with cause, and holds
// its balance for a retry. A cancelled context is not the balance's
// failure: it returns the error instead.
func hold(ctx context.Context, db *sql.DB, tx *sql.Tx, h holding, cause error) (outcome, string, error) {
	_ = tx.Rollback()
	if ctx.Err() != nil {
		return outcomeNone, "", cause
	}
	if _, err := convhold.Hold(ctx, db, convhold.KindMigration, h.migration, h.balanceID, h.source, cause); err != nil {
		return outcomeNone, "", fmt.Errorf("%w (while converting: %w)", err, cause)
	}
	return outcomeHeld, "", nil
}

// claim reads and locks one holding.
// extra receives the columns that the query selects after the holding.
func claim(ctx context.Context, tx *sql.Tx, query string, args []any, extra ...any) (holding, error) {
	var h holding
	var ratio string
	var user string
	var sub sql.NullString
	dest := append([]any{&h.migration, &h.source, &h.target, &ratio, &h.balanceID, &user, &sub, &h.available, &h.version}, extra...)
	if err := tx.QueryRowContext(ctx, query, args...).Scan(dest...); err != nil {
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

// convert converts the available balance of a claimed holding.
func convert(ctx context.Context, tx *sql.Tx, h holding) (Converted, error) {
	precision, err := ledger.Precision(ctx, tx, h.target)
	if err != nil {
		return Converted{}, err
	}
	credit, dust := ledger.Split(h.available.Decimal(), h.ratio, precision)
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
	// The migration, its source, and its sealed row in one statement, so
	// that the seal is checked against the ratio and the target that
	// convert (#451).
	var forced bool
	var phase Phase
	var current string
	var ratio sql.NullString
	sealed := rowseal.CurrencyMigrations.NewRow()
	dest := append([]any{&forced, &phase, &current, &ratio}, sealed.Dest()...)
	if err := tx.QueryRowContext(ctx, `SELECT m.forced, m.phase, m.target_hk, s.ratio::text, `+rowseal.CurrencyMigrations.Fields("m")+`
		FROM currency_migrations m
		LEFT JOIN currency_migration_sources s ON s.migration_hk = m.hk AND s.currency_hk = $2
		WHERE m.hk = $1 FOR SHARE OF m`, migrationHK, sourceHK).Scan(dest...); err != nil {
		return Converted{}, fmt.Errorf("failed to read the migration: %w", err)
	}
	if err := rowseal.Require(ctx, rowseal.CurrencyMigrations, sealed); err != nil {
		return Converted{}, err
	}
	if current != target {
		return Converted{}, fmt.Errorf("the target of migration %s changed during the conversion", migrationHK)
	}
	if forced || phase != PhaseConverting {
		return Converted{}, ErrNotConverting
	}
	if !ratio.Valid {
		return Converted{}, ErrNotASource
	}
	// The balance is read, not locked: the ledger locks the source and the
	// target in its own order, as a trade of the two would, and refuses the
	// conversion if a concurrent operation spent the funds in between.
	h := holding{migration: migrationHK, source: sourceHK, target: target, account: a}
	if h.ratio, err = decimal.NewFromString(ratio.String); err != nil {
		return Converted{}, fmt.Errorf("the ratio %q of migration %s: %w", ratio.String, migrationHK, err)
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
	// Unverified is a migration that fails its seal (#451): nothing
	// converts until an administrator reviews its integrity finding and
	// re-seals it.
	Unverified bool `json:"unverified,omitempty"`
	// Blocked are the balances of the source whose conversion failed, held
	// for a retry (#452), and BlockedCodes how many failed with each code,
	// such as row_unverified. The migration completes only once they too
	// are converted.
	Blocked      int            `json:"blocked"`
	BlockedCodes map[string]int `json:"blocked_codes,omitempty"`
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
	sealed := rowseal.CurrencyMigrations.NewRow()
	if err := db.QueryRowContext(ctx, `SELECT `+rowseal.CurrencyMigrations.Fields("")+` FROM currency_migrations WHERE hk = $1::uuid`,
		migrationHK).Scan(sealed.Dest()...); err != nil {
		return nil, fmt.Errorf("failed to read the progress: %w", err)
	}
	unverified := rowseal.Require(ctx, rowseal.CurrencyMigrations, sealed) != nil
	blocked, err := convhold.Summary(ctx, db, convhold.KindMigration, migrationHK)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Unverified = unverified
		b := blocked[out[i].CurrencyHK]
		out[i].Blocked, out[i].BlockedCodes = b.Balances, b.Codes
	}
	return out, nil
}

// Retry makes the balances of a migration into targetHK whose conversion
// failed due now (#452), such as after a re-seal, and returns how many, or
// ErrNotFound. The next run of the scheduler converts them.
func Retry(ctx context.Context, db *sql.DB, targetHK, migrationHK string, origin audit.Event) (int, error) {
	var ok bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM currency_migrations WHERE hk = $1::uuid AND target_hk = $2::uuid)`,
		migrationHK, targetHK).Scan(&ok); err != nil {
		return 0, fmt.Errorf("failed to read the migration: %w", err)
	}
	if !ok {
		return 0, ErrNotFound
	}
	return convhold.Retry(ctx, db, convhold.KindMigration, migrationHK, targetHK, origin)
}
