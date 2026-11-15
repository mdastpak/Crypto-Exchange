package config

import (
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/shopspring/decimal"
)

// The payout providers of withdrawals.payouts.provider (#70). The APIs of
// the banks and payment providers, such as Jibit's and Finnotech's, are
// added as their contracts arrive.
const (
	// PayoutProviderManual is an administrator who pays each transfer, by
	// bank or on chain, and records it with its tracking number.
	PayoutProviderManual = "manual"
)

// The bank rails that a rial withdrawal is paid by (#70).
const (
	// RailCard is a card to card transfer.
	RailCard = "card"
	// RailPaya, RailSatna, and RailPol are transfers to an IBAN.
	RailPaya  = "paya"
	RailSatna = "satna"
	RailPol   = "pol"
	// RailAccount is a transfer between two accounts of the same bank.
	RailAccount = "account"
)

// BankRails are the bank rails, in their default order of preference.
var BankRails = []string{RailAccount, RailSatna, RailPaya, RailPol, RailCard}

// PayoutsConfig configures the payouts of withdrawals (#70): how a
// withdrawal is paid, in one or more transfers.
type PayoutsConfig struct {
	// Provider pays the transfers: manual by default, the one there is.
	Provider string `yaml:"provider"`
	// IntervalSeconds is how often each instance plans, sends, and checks
	// the payouts; 15 by default.
	IntervalSeconds int `yaml:"interval_seconds"`
	// Timezone is the zone of the hours of the rails and of the days of
	// their daily limits: Asia/Tehran by default.
	Timezone string `yaml:"timezone"`
	// RailOrder is the order in which the bank rails are tried, of
	// BankRails; BankRails by default.
	RailOrder []string `yaml:"rail_order"`
	// Rails sets the limits of each bank rail, from the documents of the
	// provider. A rail left out is not used, so without rails no rial
	// withdrawal is paid.
	Rails map[string]RailConfig `yaml:"rails"`
}

// RailConfig is the limits of a bank rail, in rials. An empty bound is
// none.
type RailConfig struct {
	// Min and Max bound one transfer.
	Min string `yaml:"min"`
	Max string `yaml:"max"`
	// DailyPerDestination bounds what one destination receives on the
	// rail in a day.
	DailyPerDestination string `yaml:"daily_per_destination"`
	// Banks, for the account rail, are the banks where the exchange holds
	// an account, which a destination of the same bank is paid from.
	Banks []string `yaml:"banks"`
	// Hours, such as "08:00-17:30", is when the rail takes transfers; empty
	// is always.
	Hours string `yaml:"hours"`
}

// The defaults of the payouts.
const (
	DefaultPayoutIntervalSeconds = 15
	DefaultPayoutTimezone        = "Asia/Tehran"
)

// Interval returns IntervalSeconds, or its default.
func (p PayoutsConfig) Interval() time.Duration {
	if p.IntervalSeconds <= 0 {
		return DefaultPayoutIntervalSeconds * time.Second
	}
	return time.Duration(p.IntervalSeconds) * time.Second
}

// Location returns the zone of the rails, or its default.
func (p PayoutsConfig) Location() (*time.Location, error) {
	name := p.Timezone
	if name == "" {
		name = DefaultPayoutTimezone
	}
	return time.LoadLocation(name)
}

// Order returns RailOrder, or BankRails.
func (p PayoutsConfig) Order() []string {
	if len(p.RailOrder) == 0 {
		return BankRails
	}
	return p.RailOrder
}

// hoursPattern is a window of a day, such as 08:00-17:30.
var hoursPattern = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d-([01]\d|2[0-3]):[0-5]\d$`)

// bankPattern is a bank, as the destinations name them, such as parsian.
var bankPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,40}$`)

// payoutProblems checks withdrawals.payouts.
func (c *Configuration) payoutProblems(p *problems) {
	w := c.Withdrawals.Payouts
	switch w.Provider {
	case "", PayoutProviderManual:
	default:
		p.add("withdrawals.payouts.provider", "is %q; it must be manual until a provider's API is added", w.Provider)
	}
	if w.IntervalSeconds < 0 || w.IntervalSeconds > 3600 {
		p.add("withdrawals.payouts.interval_seconds", "is %d; it must be from 1 to 3600", w.IntervalSeconds)
	}
	if _, err := w.Location(); err != nil {
		p.add("withdrawals.payouts.timezone", "is %q: %v", w.Timezone, err)
	}
	seen := map[string]bool{}
	for _, rail := range w.RailOrder {
		if !slices.Contains(BankRails, rail) || seen[rail] {
			p.add("withdrawals.payouts.rail_order", "has %q; it lists each of %v at most once", rail, BankRails)
		}
		seen[rail] = true
	}
	for rail, r := range w.Rails {
		key := "withdrawals.payouts.rails." + rail
		if !slices.Contains(BankRails, rail) {
			p.add("withdrawals.payouts.rails", "has %q; the rails are %v", rail, BankRails)
			continue
		}
		bounds := map[string]decimal.Decimal{}
		for name, value := range map[string]string{"min": r.Min, "max": r.Max, "daily_per_destination": r.DailyPerDestination} {
			if value == "" {
				continue
			}
			d, err := decimal.NewFromString(value)
			if err != nil || !d.IsPositive() {
				p.add(key+"."+name, "is %q; it must be a positive decimal, or empty for none", value)
				continue
			}
			bounds[name] = d
		}
		if lo, ok := bounds["min"]; ok {
			if hi, ok := bounds["max"]; ok && lo.GreaterThan(hi) {
				p.add(key, "has a min above its max")
			}
		}
		if r.Hours != "" {
			if open, closing, _ := r.Window(); !hoursPattern.MatchString(r.Hours) || open >= closing {
				p.add(key+".hours", "is %q; it must be like 08:00-17:30, closing after it opens", r.Hours)
			}
		}
		for _, bank := range r.Banks {
			if !bankPattern.MatchString(bank) {
				p.add(key+".banks", "has %q; a bank is named in lower case, as the destinations name it", bank)
			}
		}
		if rail == RailAccount && len(r.Banks) == 0 {
			p.add(key+".banks", "is empty; the account rail pays only the banks listed here")
		}
	}
}

// Window returns the opening and closing times of a day of r.Hours, in
// minutes from midnight, and whether it has hours.
func (r RailConfig) Window() (open, closing int, ok bool) {
	if r.Hours == "" {
		return 0, 0, false
	}
	var oh, om, ch, cm int
	if _, err := fmt.Sscanf(r.Hours, "%d:%d-%d:%d", &oh, &om, &ch, &cm); err != nil {
		return 0, 0, false
	}
	return oh*60 + om, ch*60 + cm, true
}
