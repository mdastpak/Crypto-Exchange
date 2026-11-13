package config

import (
	"fmt"
	"maps"
	"math/big"
	"regexp"
	"slices"
)

// The verification levels of a user (#157): 0 has a verified contact
// only; 1 has the national ID matched with the owner of a bank card or
// account; 2 adds a reviewed video identity check and a commitment letter.
const (
	MinVerificationLevel = 0
	MaxVerificationLevel = 2
)

// The actions that a verification level may allow, by the names that
// verification.min_level uses. internal/verification defines one Action
// for each.
const (
	ActionDepositFiat    = "deposit_fiat"
	ActionDepositCrypto  = "deposit_crypto"
	ActionTradeOTC       = "trade_otc"
	ActionTradeMarket    = "trade_market"
	ActionWithdrawFiat   = "withdraw_fiat"
	ActionWithdrawCrypto = "withdraw_crypto"
)

// VerificationActions are the actions that a verification level may
// allow.
var VerificationActions = []string{
	ActionDepositFiat, ActionDepositCrypto, ActionTradeOTC, ActionTradeMarket, ActionWithdrawFiat, ActionWithdrawCrypto,
}

// VerificationConfig sets what each verification level of a user may do
// (#157). See docs/configuration.md#verification.
type VerificationConfig struct {
	// MinLevel is the lowest level that may do each action, by the names
	// of VerificationActions. An action it leaves out takes its default,
	// DefaultVerificationMinLevel.
	MinLevel map[string]int `yaml:"min_level"`
	// WithdrawalLimits are the withdrawal limits per currency symbol and
	// level, in units of the currency. Unset takes
	// DefaultWithdrawalLimits. A currency without an entry, or a level
	// without one, has no limit.
	WithdrawalLimits map[string]map[int]WithdrawalLimit `yaml:"withdrawal_limits"`
}

// WithdrawalLimit bounds the withdrawals of a currency at one level. An
// empty bound is no bound.
type WithdrawalLimit struct {
	// Daily bounds what a user may request to withdraw in any 24 hours, in
	// all their accounts together, the fee included.
	Daily string `yaml:"daily"`
	// PerTransaction bounds one request, the fee included.
	PerTransaction string `yaml:"per_transaction"`
}

// DefaultVerificationMinLevel returns the levels that the product policy of
// #157 sets: nothing without level 1, and crypto withdrawals only at level
// 2, until the policy says otherwise.
func DefaultVerificationMinLevel() map[string]int {
	return map[string]int{
		ActionDepositFiat:    1,
		ActionDepositCrypto:  1,
		ActionTradeOTC:       1,
		ActionTradeMarket:    1,
		ActionWithdrawFiat:   1,
		ActionWithdrawCrypto: 2,
	}
}

// DefaultWithdrawalLimits returns the daily limits of rial withdrawals: 100
// million tomans at level 1, and 300 million at level 2. They stand in
// for the limits of the Central Bank of Iran, which each deployment sets.
func DefaultWithdrawalLimits() map[string]map[int]WithdrawalLimit {
	return map[string]map[int]WithdrawalLimit{
		"IRR": {
			1: {Daily: "1000000000"},
			2: {Daily: "3000000000"},
		},
	}
}

// applyVerificationDefaults gives each action that min_level leaves out
// its default, and the limits their defaults when they are unset.
func (c *Configuration) applyVerificationDefaults() {
	v := &c.Verification
	if v.MinLevel == nil {
		v.MinLevel = map[string]int{}
	}
	for action, level := range DefaultVerificationMinLevel() {
		if _, ok := v.MinLevel[action]; !ok {
			v.MinLevel[action] = level
		}
	}
	if v.WithdrawalLimits == nil {
		v.WithdrawalLimits = DefaultWithdrawalLimits()
	}
}

var currencySymbol = regexp.MustCompile(`^[A-Z0-9]{1,20}$`)

// verificationProblems reports unknown actions, levels out of range, and
// limits that are not positive amounts, or whose bound per transaction is
// above the daily one.
func (c *Configuration) verificationProblems(p *problems) {
	v := c.Verification
	for _, action := range slices.Sorted(maps.Keys(v.MinLevel)) {
		key := "verification.min_level." + action
		if !slices.Contains(VerificationActions, action) {
			p.add(key, "is not an action; the actions are %v", VerificationActions)
			continue
		}
		if level := v.MinLevel[action]; level < MinVerificationLevel || level > MaxVerificationLevel {
			p.add(key, "is %d; it must be from %d to %d", level, MinVerificationLevel, MaxVerificationLevel)
		}
	}
	for _, symbol := range slices.Sorted(maps.Keys(v.WithdrawalLimits)) {
		if !currencySymbol.MatchString(symbol) {
			p.add("verification.withdrawal_limits."+symbol, "is not a currency symbol: upper-case letters and digits")
			continue
		}
		levels := v.WithdrawalLimits[symbol]
		for _, level := range slices.Sorted(maps.Keys(levels)) {
			key := fmt.Sprintf("verification.withdrawal_limits.%s.%d", symbol, level)
			if level < MinVerificationLevel || level > MaxVerificationLevel {
				p.add(key, "names level %d; the levels are %d to %d", level, MinVerificationLevel, MaxVerificationLevel)
				continue
			}
			limit := levels[level]
			daily, okDaily := positiveAmount(p, key+".daily", limit.Daily)
			perTx, okPerTx := positiveAmount(p, key+".per_transaction", limit.PerTransaction)
			if okDaily && okPerTx && daily != nil && perTx != nil && perTx.Cmp(daily) > 0 {
				p.add(key+".per_transaction", "is %s, above the daily limit %s", limit.PerTransaction, limit.Daily)
			}
		}
	}
}

// positiveAmount parses an optional amount: empty is no bound (nil, true),
// and anything else must be a positive decimal number.
func positiveAmount(p *problems, key, s string) (*big.Rat, bool) {
	if s == "" {
		return nil, true
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() <= 0 || !plainDecimal.MatchString(s) {
		p.add(key, "is %q; it must be a positive decimal amount, such as \"1000000000\", or empty for no limit", s)
		return nil, false
	}
	return r, true
}

var plainDecimal = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
