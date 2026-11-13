// Package verification decides what a user may do at their verification
// level (#157), and up to which amounts.
//
// The product policy sets three levels:
//
//   - 0, registered: a verified contact only. Nothing that moves funds.
//   - 1, identity and bank: the national ID matches the owner of the user's
//     bank card or account (#158). Deposits from the user's own cards and
//     accounts (#159), OTC and market trading, and fiat withdrawals to the
//     user's registered destinations (#160) within a daily limit.
//   - 2, full: level 1 with a reviewed video identity check and commitment
//     letter (#162). Crypto withdrawals, and fiat withdrawals up to the limit
//     of the Central Bank of Iran.
//
// The lowest level of each action and the withdrawal limits are settings
// (verification in the configuration), so that a change of the rules needs
// no release. Every path that deposits, trades, or withdraws asks this
// package instead of reading the tier itself: Store.Check for an action,
// and Store.CheckWithdrawal, in the transaction of a withdrawal request,
// for the level and the limits together.
package verification

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/gin-gonic/gin"
)

// Level is a verification level of a user.
type Level int

// The levels.
const (
	Level0 Level = config.MinVerificationLevel
	Level1 Level = 1
	Level2 Level = config.MaxVerificationLevel
)

// LevelOf returns the level of a user with a KYC tier and the status of
// that tier. Only an approved tier counts: a verification that expired, or
// whose status is anything but approved, grants level 0 until it is
// renewed. Tiers above STANDARD are level 2, until a corporate or higher
// level is defined.
func LevelOf(tier shared.KYCTier, status shared.KYCStatus) Level {
	if status != shared.KYCStatusApproved {
		return Level0
	}
	rank, ok := tier.Rank()
	switch {
	case !ok || rank <= 0:
		return Level0
	case rank == 1:
		return Level1
	default:
		return Level2
	}
}

// Action is something that a level may allow.
type Action string

// The actions. Their names are the keys of verification.min_level.
const (
	DepositFiat    Action = config.ActionDepositFiat
	DepositCrypto  Action = config.ActionDepositCrypto
	TradeOTC       Action = config.ActionTradeOTC
	TradeMarket    Action = config.ActionTradeMarket
	WithdrawFiat   Action = config.ActionWithdrawFiat
	WithdrawCrypto Action = config.ActionWithdrawCrypto
)

// Actions are the actions, in the order that a summary lists them.
var Actions = []Action{DepositFiat, DepositCrypto, TradeOTC, TradeMarket, WithdrawFiat, WithdrawCrypto}

// Window is the period of a daily withdrawal limit: any 24 hours, not a
// calendar day, so that a limit cannot be used twice around midnight.
const Window = 24 * time.Hour

// Limit bounds the withdrawals of a currency at one level. A nil bound is
// no bound.
type Limit struct {
	Daily          *shared.Decimal
	PerTransaction *shared.Decimal
}

// Policy is the lowest level of each action and the withdrawal limits.
type Policy struct {
	minLevel map[Action]Level
	limits   map[string]map[Level]Limit
}

// NewPolicy returns the policy of the settings, which Load has validated
// and given their defaults.
func NewPolicy(c config.VerificationConfig) (Policy, error) {
	p := Policy{minLevel: map[Action]Level{}, limits: map[string]map[Level]Limit{}}
	defaults := config.DefaultVerificationMinLevel()
	for _, a := range Actions {
		level, ok := c.MinLevel[string(a)]
		if !ok {
			level = defaults[string(a)]
		}
		if level < config.MinVerificationLevel || level > config.MaxVerificationLevel {
			return Policy{}, fmt.Errorf("verification.min_level.%s is %d", a, level)
		}
		p.minLevel[a] = Level(level)
	}
	for symbol, levels := range c.WithdrawalLimits {
		p.limits[symbol] = map[Level]Limit{}
		for level, l := range levels {
			daily, err := bound(l.Daily)
			if err != nil {
				return Policy{}, fmt.Errorf("verification.withdrawal_limits.%s.%d.daily: %w", symbol, level, err)
			}
			perTx, err := bound(l.PerTransaction)
			if err != nil {
				return Policy{}, fmt.Errorf("verification.withdrawal_limits.%s.%d.per_transaction: %w", symbol, level, err)
			}
			p.limits[symbol][Level(level)] = Limit{Daily: daily, PerTransaction: perTx}
		}
	}
	return p, nil
}

func bound(s string) (*shared.Decimal, error) {
	if s == "" {
		return nil, nil
	}
	d, err := shared.NewDecimalFromString(s)
	if err != nil {
		return nil, err
	}
	if !d.IsPositive() {
		return nil, fmt.Errorf("%s is not positive", s)
	}
	return &d, nil
}

// Required returns the lowest level that may do a.
func (p Policy) Required(a Action) Level { return p.minLevel[a] }

// Allows reports whether level may do a. An unknown action is never
// allowed.
func (p Policy) Allows(level Level, a Action) bool {
	required, ok := p.minLevel[a]
	return ok && level >= required
}

// Limit returns the withdrawal limit of the currency symbol at level.
func (p Policy) Limit(level Level, symbol string) Limit { return p.limits[symbol][level] }

// WithdrawAction returns the action of a withdrawal of a fiat currency, or
// of a crypto one.
func WithdrawAction(fiat bool) Action {
	if fiat {
		return WithdrawFiat
	}
	return WithdrawCrypto
}

// CheckWithdrawal decides a withdrawal of amount of the currency symbol
// (currencyHK) by a user at level who has requested used in the window
// already. It returns a *Refusal when the level does not allow it or the
// amount is above a limit.
func (p Policy) CheckWithdrawal(level Level, fiat bool, symbol, currencyHK string, amount, used shared.Decimal) error {
	action := WithdrawAction(fiat)
	if !p.Allows(level, action) {
		return &Refusal{Code: CodeVerificationRequired, Action: action, Level: level, Required: p.Required(action)}
	}
	limit := p.Limit(level, symbol)
	if limit.PerTransaction != nil && amount.GreaterThan(*limit.PerTransaction) {
		return &Refusal{Code: CodeWithdrawalLimitExceeded, Action: action, Level: level, CurrencyHK: currencyHK,
			Period: PeriodTransaction, Limit: *limit.PerTransaction}
	}
	if limit.Daily != nil && used.Add(amount).GreaterThan(*limit.Daily) {
		return &Refusal{Code: CodeWithdrawalLimitExceeded, Action: action, Level: level, CurrencyHK: currencyHK,
			Period: PeriodDaily, Limit: *limit.Daily, Used: used}
	}
	return nil
}

// The codes of the refusals.
const (
	// CodeVerificationRequired is an action that the user's level does not
	// allow. The refusal names the level that it needs.
	CodeVerificationRequired = "verification_required"
	// CodeWithdrawalLimitExceeded is a withdrawal above a limit of the
	// user's level.
	CodeWithdrawalLimitExceeded = "withdrawal_limit_exceeded"
)

// The periods of a limit.
const (
	PeriodTransaction = "transaction"
	PeriodDaily       = "24h"
)

// Refusal is an action that the user's level does not allow, or a
// withdrawal above a limit. It is an error.
type Refusal struct {
	Code   string
	Action Action
	// Level is the user's level, and Required the level that the action
	// needs.
	Level    Level
	Required Level
	// CurrencyHK, Period, Limit, and Used describe an exceeded limit: Used
	// is what the window holds already, for a daily limit.
	CurrencyHK string
	Period     string
	Limit      shared.Decimal
	Used       shared.Decimal
}

func (r *Refusal) Error() string {
	if r.Code == CodeVerificationRequired {
		return fmt.Sprintf("%s: %s needs level %d, the user has %d", r.Code, r.Action, r.Required, r.Level)
	}
	return fmt.Sprintf("%s: %s of %s above the %s limit %s", r.Code, r.Action, r.CurrencyHK, r.Period, r.Limit)
}

// Remaining is what the user may still withdraw under the limit of a
// refusal: the limit less what the window holds, and never below zero.
func (r *Refusal) Remaining() shared.Decimal {
	rest := r.Limit.Sub(r.Used)
	if rest.IsNegative() {
		return shared.NewDecimalFromInt64(0)
	}
	return rest
}

// Messages of the refusals.
var (
	messageVerificationRequired    = i18n.M("VERIFICATION_REQUIRED")
	messageWithdrawalLimitExceeded = i18n.M("WITHDRAWAL_LIMIT_EXCEEDED")
)

// Send answers a refusal with 403, its code, and what the client needs to
// show: the level that an action needs, or the limit and what remains of
// it. It reports false, and sends nothing, for an err that is not a
// *Refusal.
func Send(c *gin.Context, err error) bool {
	var r *Refusal
	if !errors.As(err, &r) {
		return false
	}
	info := gin.H{"action": r.Action, "level": r.Level}
	message := messageVerificationRequired
	if r.Code == CodeVerificationRequired {
		info["required_level"] = r.Required
	} else {
		message = messageWithdrawalLimitExceeded
		info["currency_hk"] = r.CurrencyHK
		info["period"] = r.Period
		info["limit"] = r.Limit
		if r.Period == PeriodDaily {
			info["used"] = r.Used
			info["remaining"] = r.Remaining()
		}
	}
	helpers.SendError(c, http.StatusForbidden, r.Code, message, info)
	return true
}

// symbols returns the currency symbols that have limits, sorted.
func (p Policy) symbols() []string {
	return slices.Sorted(maps.Keys(p.limits))
}
