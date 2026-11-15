package verification

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/google/uuid"
)

// Querier runs queries: a *sql.DB, or a *sql.Tx for a check in the
// transaction of an operation.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Errors of the checks.
var (
	ErrUserNotFound    = errors.New("user not found")
	ErrUnknownCurrency = errors.New("unknown currency")
)

// Store reads the levels of users and checks their actions against
// Policy.
type Store struct {
	DB     *sql.DB
	Policy Policy
}

// Status is the verification of a user: their level, and the tier and
// status it comes from.
type Status struct {
	Level  Level
	Tier   shared.KYCTier
	Status shared.KYCStatus
}

// Level reads the verification of the user. The tier and its status are
// columns of the user's sealed row (#139): a row that does not match its
// seal in enforce mode, such as a tier raised in SQL, counts as level 0,
// and the finding is recorded.
func (s Store) Level(ctx context.Context, q Querier, userHK string) (Status, error) {
	var st Status
	row := rowseal.Users.NewRow()
	err := q.QueryRowContext(ctx, `SELECT kyc_tier, kyc_status, `+rowseal.Users.Fields("")+`
		FROM users WHERE hk = $1 AND deleted_at IS NULL`, userHK).
		Scan(append([]any{&st.Tier, &st.Status}, row.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Status{}, ErrUserNotFound
	}
	if err != nil {
		return Status{}, fmt.Errorf("read the verification of the user: %w", err)
	}
	if err := rowseal.Check(ctx, rowseal.Users, row); err != nil {
		if errors.Is(err, rowseal.ErrUnverified) {
			return Status{Level: Level0, Tier: st.Tier, Status: st.Status}, nil
		}
		return Status{}, err
	}
	st.Level = LevelOf(st.Tier, st.Status)
	return st, nil
}

// Check returns a *Refusal when the user's level does not allow a. A
// check that guards a change of money runs in its transaction, after
// restrictions.Lock: an administrator who lowers the level waits for the
// operations in flight, and the operations after see the new level.
func (s Store) Check(ctx context.Context, q Querier, userHK string, a Action) error {
	st, err := s.Level(ctx, q, userHK)
	if err != nil {
		return err
	}
	if !s.Policy.Allows(st.Level, a) {
		return &Refusal{Code: CodeVerificationRequired, Action: a, Level: st.Level, Required: s.Policy.Required(a)}
	}
	return nil
}

// currency is what a check needs of a currency.
type currency struct {
	// Symbols are its current symbol, and then the ones it had before a
	// rename, by which a limit may be configured (#406).
	Symbols []string
	Fiat    bool
}

// symbolsOf is the expression of every symbol of the currency c.hk, its
// current one first and then its previous ones, the latest first, joined
// with commas, which no symbol holds: upper-case letters and digits.
// currency_symbols keeps each symbol a currency had, reserved to it until
// an administrator releases it; a released symbol may name another
// currency, so it no longer counts.
const symbolsOf = `concat_ws(',', c.info->>'symbol', (SELECT string_agg(p.symbol, ',' ORDER BY p.created_at DESC, p.symbol)
		FROM currency_symbols p WHERE p.currency_hk = c.hk AND NOT p.current AND p.released_at IS NULL
		  AND p.symbol <> c.info->>'symbol'))`

func readCurrency(ctx context.Context, q Querier, currencyHK string) (currency, error) {
	var c currency
	var symbols string
	err := q.QueryRowContext(ctx, `SELECT `+symbolsOf+`, COALESCE((c.info->>'if_fiat')::boolean, false)
		FROM currencies c WHERE c.hk = $1 AND c.deleted_at IS NULL`, currencyHK).Scan(&symbols, &c.Fiat)
	c.Symbols = strings.Split(symbols, ",")
	if errors.Is(err, sql.ErrNoRows) {
		return currency{}, fmt.Errorf("%w: %s", ErrUnknownCurrency, currencyHK)
	}
	if err != nil {
		return currency{}, fmt.Errorf("read the currency %s: %w", currencyHK, err)
	}
	return c, nil
}

// CheckWithdrawal returns a *Refusal when the user's level does not allow
// a withdrawal of amount of the currency, or the amount is above a limit
// of the level: per transaction, or in the window together with what the
// user has requested already, in all their accounts.
//
// It runs in tx, the transaction of the withdrawal request, before the
// ledger locks its funds in the same transaction (ledger.Ledger.In), and
// after restrictions.Lock. It takes a lock of the user's withdrawals of
// the currency that the transaction holds until it ends, so that two
// requests never both count what the window held before either: the
// second waits, and then counts the first.
func (s Store) CheckWithdrawal(ctx context.Context, tx *sql.Tx, userHK, currencyHK string, amount shared.Decimal) error {
	if !amount.IsPositive() {
		return fmt.Errorf("a withdrawal of %s", amount)
	}
	c, err := readCurrency(ctx, tx, currencyHK)
	if err != nil {
		return err
	}
	st, err := s.Level(ctx, tx, userHK)
	if err != nil {
		return err
	}
	used := shared.NewDecimalFromInt64(0)
	if limit, _ := s.Policy.Limit(st.Level, c.Symbols...); limit.Daily != nil && s.Policy.Allows(st.Level, WithdrawAction(c.Fiat)) {
		if used, err = withdrawn(ctx, tx, userHK, currencyHK, true); err != nil {
			return err
		}
	}
	return s.Policy.CheckWithdrawal(st.Level, c.Fiat, c.Symbols, currencyHK, amount, used)
}

// withdrawn returns what the user has requested to withdraw of the
// currency in the window. With lock, it first takes the transaction's
// lock of the user's withdrawals of the currency.
func withdrawn(ctx context.Context, q interface {
	Querier
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}, userHK, currencyHK string, lock bool) (shared.Decimal, error) {
	user, err := uuid.Parse(userHK)
	if err != nil {
		return shared.Decimal{}, fmt.Errorf("user %q: %w", userHK, err)
	}
	cur, err := uuid.Parse(currencyHK)
	if err != nil {
		return shared.Decimal{}, fmt.Errorf("currency %q: %w", currencyHK, err)
	}
	if lock {
		if _, err := q.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			"withdrawal_limit:"+user.String()+":"+cur.String()); err != nil {
			return shared.Decimal{}, fmt.Errorf("lock the withdrawals of the user: %w", err)
		}
	}
	return ledger.Withdrawn(ctx, q, shared.UUID(user), shared.UUID(cur), Window)
}

// Summary is the verification of a user as the user sees it: their level,
// what it allows, and their withdrawal limits.
type Summary struct {
	// Level is the user's verification level, from 0 to 2.
	Level Level `json:"level" example:"1"`
	// KYCTier and KYCStatus are what the level comes from.
	KYCTier   shared.KYCTier   `json:"kyc_tier" example:"BASIC"`
	KYCStatus shared.KYCStatus `json:"kyc_status" example:"APPROVED"`
	// Actions are every action, whether the level allows it, and the level
	// that it needs.
	Actions []ActionStatus `json:"actions"`
	// WithdrawalLimits are the limits of the currencies that the user may
	// withdraw at their level, and what remains of the daily ones.
	WithdrawalLimits []LimitStatus `json:"withdrawal_limits"`
}

// ActionStatus is whether the user's level allows an action.
type ActionStatus struct {
	Action        Action `json:"action" example:"withdraw_fiat"`
	Allowed       bool   `json:"allowed"`
	RequiredLevel Level  `json:"required_level" example:"1"`
}

// LimitStatus is a withdrawal limit of the user's level in one currency.
type LimitStatus struct {
	CurrencyHK string `json:"currency_hk" format:"uuid"`
	Symbol     string `json:"symbol" example:"IRR"`
	// Daily bounds what the user may request in any 24 hours, in all
	// their accounts, the fee included; absent is no bound.
	Daily *shared.Decimal `json:"daily,omitempty" swaggertype:"string" example:"1000000000"`
	// PerTransaction bounds one request; absent is no bound.
	PerTransaction *shared.Decimal `json:"per_transaction,omitempty" swaggertype:"string"`
	// Used is what the user has requested in the last 24 hours, and
	// Remaining what the daily limit still allows.
	Used      shared.Decimal  `json:"used" swaggertype:"string" example:"0"`
	Remaining *shared.Decimal `json:"remaining,omitempty" swaggertype:"string" example:"1000000000"`
	// WindowHours is the period of the daily limit.
	WindowHours int `json:"window_hours" example:"24"`
}

// Summary returns the verification of the user.
func (s Store) Summary(ctx context.Context, userHK string) (Summary, error) {
	st, err := s.Level(ctx, s.DB, userHK)
	if err != nil {
		return Summary{}, err
	}
	sum := Summary{Level: st.Level, KYCTier: st.Tier, KYCStatus: st.Status,
		Actions: make([]ActionStatus, 0, len(Actions)), WithdrawalLimits: []LimitStatus{}}
	for _, a := range Actions {
		sum.Actions = append(sum.Actions, ActionStatus{Action: a, Allowed: s.Policy.Allows(st.Level, a), RequiredLevel: s.Policy.Required(a)})
	}
	// Each configured symbol names the currency that has or had it (#406),
	// shown by its current symbol, once.
	seen := map[string]bool{}
	for _, configured := range s.Policy.symbols() {
		var hk, symbols string
		var c currency
		err := s.DB.QueryRowContext(ctx, `SELECT c.hk::text, `+symbolsOf+`, COALESCE((c.info->>'if_fiat')::boolean, false)
			FROM currencies c WHERE c.deleted_at IS NULL
			  AND (c.info->>'symbol' = $1 OR c.hk = (SELECT cs.currency_hk FROM currency_symbols cs WHERE cs.symbol = $1 AND cs.released_at IS NULL))
			LIMIT 1`, configured).Scan(&hk, &symbols, &c.Fiat)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return Summary{}, fmt.Errorf("read the currency %s: %w", configured, err)
		}
		c.Symbols = strings.Split(symbols, ",")
		if seen[hk] || !s.Policy.Allows(st.Level, WithdrawAction(c.Fiat)) {
			continue
		}
		seen[hk] = true
		limit, _ := s.Policy.Limit(st.Level, c.Symbols...)
		if limit.Daily == nil && limit.PerTransaction == nil {
			continue
		}
		ls := LimitStatus{CurrencyHK: hk, Symbol: c.Symbols[0], Daily: limit.Daily, PerTransaction: limit.PerTransaction,
			Used: shared.NewDecimalFromInt64(0), WindowHours: int(Window.Hours())}
		if limit.Daily != nil {
			if ls.Used, err = withdrawn(ctx, s.DB, userHK, hk, false); err != nil {
				return Summary{}, err
			}
			rest := limit.Daily.Sub(ls.Used)
			if rest.IsNegative() {
				rest = shared.NewDecimalFromInt64(0)
			}
			ls.Remaining = &rest
		}
		sum.WithdrawalLimits = append(sum.WithdrawalLimits, ls)
	}
	return sum, nil
}

// Withdrawable is a listed currency that a network may withdraw.
type Withdrawable struct {
	HK string
	// Symbols are its current symbol, and then its previous ones.
	Symbols []string
	Fiat    bool
}

// WithdrawableCurrencies returns the listed currencies that some network
// may withdraw, by current symbol, for the warnings of a configuration
// that leaves one without a limit or a review threshold (#406).
func WithdrawableCurrencies(ctx context.Context, q Querier) ([]Withdrawable, error) {
	rows, err := q.QueryContext(ctx, `SELECT c.hk::text, `+symbolsOf+`, COALESCE((c.info->>'if_fiat')::boolean, false)
		FROM currencies c
		WHERE c.deleted_at IS NULL AND c.status = 'approved'
		  AND EXISTS (SELECT 1 FROM currency_networks n WHERE n.currency_hk = c.hk AND n.withdrawal_enabled)
		ORDER BY c.info->>'symbol'`)
	if err != nil {
		return nil, fmt.Errorf("list the withdrawable currencies: %w", err)
	}
	defer rows.Close()
	var out []Withdrawable
	for rows.Next() {
		var w Withdrawable
		var symbols string
		if err := rows.Scan(&w.HK, &symbols, &w.Fiat); err != nil {
			return nil, fmt.Errorf("list the withdrawable currencies: %w", err)
		}
		w.Symbols = strings.Split(symbols, ",")
		out = append(out, w)
	}
	return out, rows.Err()
}

// Unlimited returns the levels that allow the withdrawals of w and refuse
// them for want of a limit (#406).
func (p Policy) Unlimited(w Withdrawable) []Level {
	var levels []Level
	for level := Level(config.MinVerificationLevel); level <= Level(config.MaxVerificationLevel); level++ {
		if !p.Allows(level, WithdrawAction(w.Fiat)) {
			continue
		}
		if _, ok := p.Limit(level, w.Symbols...); !ok {
			levels = append(levels, level)
		}
	}
	return levels
}
