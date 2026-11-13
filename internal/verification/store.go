package verification

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
	Symbol string
	Fiat   bool
}

func readCurrency(ctx context.Context, q Querier, currencyHK string) (currency, error) {
	var c currency
	err := q.QueryRowContext(ctx, `SELECT info->>'symbol', COALESCE((info->>'if_fiat')::boolean, false)
		FROM currencies WHERE hk = $1 AND deleted_at IS NULL`, currencyHK).Scan(&c.Symbol, &c.Fiat)
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
	if limit := s.Policy.Limit(st.Level, c.Symbol); limit.Daily != nil && s.Policy.Allows(st.Level, WithdrawAction(c.Fiat)) {
		if used, err = withdrawn(ctx, tx, userHK, currencyHK, true); err != nil {
			return err
		}
	}
	return s.Policy.CheckWithdrawal(st.Level, c.Fiat, c.Symbol, currencyHK, amount, used)
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
	for _, symbol := range s.Policy.symbols() {
		limit := s.Policy.Limit(st.Level, symbol)
		if limit.Daily == nil && limit.PerTransaction == nil {
			continue
		}
		var hk string
		var fiat bool
		err := s.DB.QueryRowContext(ctx, `SELECT hk::text, COALESCE((info->>'if_fiat')::boolean, false)
			FROM currencies WHERE info->>'symbol' = $1 AND deleted_at IS NULL`, symbol).Scan(&hk, &fiat)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return Summary{}, fmt.Errorf("read the currency %s: %w", symbol, err)
		}
		if !s.Policy.Allows(st.Level, WithdrawAction(fiat)) {
			continue
		}
		ls := LimitStatus{CurrencyHK: hk, Symbol: symbol, Daily: limit.Daily, PerTransaction: limit.PerTransaction,
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
