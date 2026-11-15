package config

import (
	"time"

	"github.com/shopspring/decimal"
)

// Providers of identity.bank_ownership.provider. The providers' APIs, such
// as Jibit's and Finnotech's, are added as their contracts arrive.
const (
	BankOwnershipNone = "none"
	BankOwnershipStub = "stub"
)

// BankOwnershipConfig configures the check that a card, an IBAN, or a bank
// account belongs to the national ID of its user (#158, #160).
type BankOwnershipConfig struct {
	// Provider is none or stub. With none, no card, IBAN, or account can
	// be added to the withdrawal destinations, since its owner cannot be
	// checked. stub matches every number, for development, and is refused
	// in release mode.
	Provider string `yaml:"provider"`
	// TimeoutSeconds bounds one check, with its retries.
	TimeoutSeconds int `yaml:"timeout_seconds"`
}

// Timeout is TimeoutSeconds, or ten seconds when it is not set.
func (b BankOwnershipConfig) Timeout() time.Duration {
	if b.TimeoutSeconds <= 0 {
		return 10 * time.Second
	}
	return time.Duration(b.TimeoutSeconds) * time.Second
}

// WithdrawalsConfig configures the withdrawal destinations of users (#160).
type WithdrawalsConfig struct {
	// MaxDestinations bounds the destinations in a user's book that are
	// not removed.
	MaxDestinations int `yaml:"max_destinations"`
	// FiatDestinationWaitHours and CryptoDestinationWaitHours are how
	// long a new destination waits before a withdrawal may use it, so that
	// a hijacked session cannot add one and withdraw to it at once. Unset
	// takes the defaults: no wait for a card, an IBAN, or an account,
	// which must be the user's own, and 24 hours for a crypto address,
	// which could be anyone's.
	FiatDestinationWaitHours   *int `yaml:"fiat_destination_wait_hours"`
	CryptoDestinationWaitHours *int `yaml:"crypto_destination_wait_hours"`
	// ReviewAbove holds, by currency symbol, the amount from which a
	// withdrawal waits for an administrator before it is paid (#70). A
	// previous symbol of a currency still names it, and the key
	// AnyCurrency, "*", holds the threshold of the currencies without an
	// entry (#406). While it has any entry, a currency that has none, and
	// no default, is held for review whatever the amount, rather than
	// never; an empty map holds nothing for review.
	ReviewAbove map[string]string `yaml:"review_above"`
	// Payouts configures how a withdrawal is paid (#70).
	Payouts PayoutsConfig `yaml:"payouts"`
}

// The defaults of the withdrawal destinations.
const (
	DefaultMaxDestinations            = 20
	DefaultFiatDestinationWaitHours   = 0
	DefaultCryptoDestinationWaitHours = 24
	MaxDestinationWaitHours           = 720
)

// DestinationWait returns how long a new destination waits, for fiat or
// for crypto.
func (w WithdrawalsConfig) DestinationWait(fiat bool) time.Duration {
	hours := DefaultCryptoDestinationWaitHours
	if fiat {
		hours = DefaultFiatDestinationWaitHours
	}
	if set := w.CryptoDestinationWaitHours; !fiat && set != nil {
		hours = *set
	}
	if set := w.FiatDestinationWaitHours; fiat && set != nil {
		hours = *set
	}
	return time.Duration(hours) * time.Hour
}

// Destinations returns MaxDestinations, or its default when it is not set.
func (w WithdrawalsConfig) Destinations() int {
	if w.MaxDestinations <= 0 {
		return DefaultMaxDestinations
	}
	return w.MaxDestinations
}

// withdrawalProblems checks identity.bank_ownership and withdrawals in
// every mode.
func (c *Configuration) withdrawalProblems(p *problems) {
	switch c.Identity.BankOwnership.Provider {
	case "", BankOwnershipNone, BankOwnershipStub:
	default:
		p.add("identity.bank_ownership.provider", "is %q; it must be none or stub until a provider's API is added", c.Identity.BankOwnership.Provider)
	}
	if v := c.Identity.BankOwnership.TimeoutSeconds; v < 0 || v > 60 {
		p.add("identity.bank_ownership.timeout_seconds", "is %d; it must be from 1 to 60", v)
	}
	w := c.Withdrawals
	if v := w.MaxDestinations; v < 0 || v > 200 {
		p.add("withdrawals.max_destinations", "is %d; it must be from 1 to 200", v)
	}
	for key, hours := range map[string]*int{
		"withdrawals.fiat_destination_wait_hours":   w.FiatDestinationWaitHours,
		"withdrawals.crypto_destination_wait_hours": w.CryptoDestinationWaitHours,
	} {
		if hours != nil && (*hours < 0 || *hours > MaxDestinationWaitHours) {
			p.add(key, "is %d; it must be from 0 to %d", *hours, MaxDestinationWaitHours)
		}
	}
	c.payoutProblems(p)
	for symbol, amount := range w.ReviewAbove {
		if symbol != AnyCurrency && !currencySymbol.MatchString(symbol) {
			p.add("withdrawals.review_above", "has %q; the keys are currency symbols, upper case, or \"*\" for every other currency", symbol)
		}
		if d, err := decimal.NewFromString(amount); err != nil || !d.IsPositive() {
			p.add("withdrawals.review_above."+symbol, "is %q; it must be a positive decimal", amount)
		}
	}
}
