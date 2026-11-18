// Package aml evaluates the anti-money-laundering rules (#73): it syncs
// config/aml_rules.yaml into the sealed aml_rules table with a version for
// each change, evaluates the rules on the operations that move money, in
// their transactions, and keeps the alerts with their lifecycle.
//
// An operation is evaluated against every enabled rule that applies to its
// kind. The strongest action of the rules that hit decides: ALERT records
// an alert, REVIEW holds the operation for an administrator, BLOCK refuses
// it, and FREEZE refuses it and freezes the account. Amounts are compared
// in USDT, at the current price of the price engine; an amount that needs a
// price that is not current fails closed, to REVIEW.
package aml

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/shopspring/decimal"
)

// Type is the type of a rule: the aml_rule_type of the schema.
type Type string

// The types of rules. The screening types need a provider (Screener) and
// are not evaluated without one.
const (
	TypeVelocity        Type = "VELOCITY"
	TypeStructuring     Type = "STRUCTURING"
	TypeHighRiskCountry Type = "HIGH_RISK_COUNTRY"
	TypeUnusualPattern  Type = "UNUSUAL_PATTERN"
	TypeMixer           Type = "MIXER"
	TypeDarknet         Type = "DARKNET"
	TypeSanctions       Type = "SANCTIONS"
	TypePEP             Type = "PEP"
)

// Screening reports whether rules of t ask a screening provider.
func (t Type) Screening() bool {
	return t == TypeMixer || t == TypeDarknet || t == TypeSanctions || t == TypePEP
}

// Action is what a rule does when it hits: the aml_action of the schema.
type Action string

// The actions, from the weakest.
const (
	ActionNone   Action = ""
	ActionAlert  Action = "ALERT"
	ActionReview Action = "REVIEW"
	ActionBlock  Action = "BLOCK"
	ActionFreeze Action = "FREEZE"
)

// rank orders the actions: a stronger one decides.
func (a Action) rank() int {
	return slices.Index([]Action{ActionNone, ActionAlert, ActionReview, ActionBlock, ActionFreeze}, a)
}

// Stronger returns the stronger of a and b.
func (a Action) Stronger(b Action) Action {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// Weaker returns the weaker of a and b.
func (a Action) Weaker(b Action) Action {
	if b.rank() < a.rank() {
		return b
	}
	return a
}

// Refuses reports whether a refuses the operation.
func (a Action) Refuses() bool { return a == ActionBlock || a == ActionFreeze }

// Kind is the kind of an operation that rules apply to.
type Kind string

// The kinds of operations.
const (
	KindDeposit    Kind = "deposit"
	KindWithdrawal Kind = "withdrawal"
	KindTrade      Kind = "trade"
)

var kinds = []Kind{KindDeposit, KindWithdrawal, KindTrade}

// The severities of a rule, from the lowest, and the risk score of an
// alert of each.
var severities = map[string]string{"LOW": "25", "MEDIUM": "50", "HIGH": "75", "CRITICAL": "100"}

// The metrics of a velocity rule.
const (
	// MetricCount counts the operations of the window.
	MetricCount = "count"
	// MetricVolume sums their value in USDT.
	MetricVolume = "volume"
	// MetricCycles counts the withdrawals of the window that follow a
	// deposit of the window: deposits taken out at once.
	MetricCycles = "deposit_withdrawal_cycles"
	// MetricSingle is the value of the operation alone.
	MetricSingle = "single_amount"
)

// The fields of a country rule.
const (
	FieldIPCountry           = "ip_country"
	FieldCounterpartyCountry = "counterparty_country"
)

// The patterns of an unusual-pattern rule, and the checks of the
// withdrawal profile.
const (
	PatternWithdrawalProfile = "withdrawal_profile"
	PatternDormantAccount    = "dormant_account"

	CheckNewDestination = "new_destination"
	CheckAmount         = "amount_deviation"
	CheckTime           = "time_deviation"
	CheckVelocity       = "velocity_spike"
)

// Rule is a rule of the file, and of aml_rules once synced.
type Rule struct {
	Code        string     `yaml:"id" json:"code"`
	Name        string     `yaml:"name" json:"name"`
	Description string     `yaml:"description" json:"description,omitempty"`
	Type        Type       `yaml:"rule_type" json:"rule_type"`
	Enabled     bool       `yaml:"enabled" json:"enabled"`
	AppliesTo   []Kind     `yaml:"applies_to" json:"applies_to"`
	Config      RuleConfig `yaml:"config" json:"config"`
	// ThresholdCount and ThresholdAmount, in USDT, mean what the type says;
	// zero is no threshold.
	ThresholdCount  int             `yaml:"threshold_count" json:"threshold_count"`
	ThresholdAmount decimal.Decimal `yaml:"threshold_amount" json:"threshold_amount" swaggertype:"string" example:"10000"`
	WindowHours     int             `yaml:"time_window_hours" json:"time_window_hours"`
	Action          Action          `yaml:"action" json:"action"`
	Severity        string          `yaml:"severity" json:"severity"`
}

// RuleConfig is the part of a rule of its type. A field that the type does
// not use must be left out.
type RuleConfig struct {
	// Velocity.
	Metric string `yaml:"metric,omitempty" json:"metric,omitempty"`
	// Structuring: the band is ThresholdAmount up to Ceiling, excluded.
	Ceiling *decimal.Decimal `yaml:"ceiling,omitempty" json:"ceiling,omitempty" swaggertype:"string"`
	// High-risk country.
	Countries   []string `yaml:"countries,omitempty" json:"countries,omitempty"`
	CheckFields []string `yaml:"check_fields,omitempty" json:"check_fields,omitempty"`
	CrossBorder bool     `yaml:"cross_border,omitempty" json:"cross_border,omitempty"`
	HomeCountry string   `yaml:"home_country,omitempty" json:"home_country,omitempty"`
	// Unusual pattern.
	Pattern                   string           `yaml:"pattern,omitempty" json:"pattern,omitempty"`
	Checks                    []string         `yaml:"checks,omitempty" json:"checks,omitempty"`
	HistoryDays               int              `yaml:"history_days,omitempty" json:"history_days,omitempty"`
	NewDestinationDays        int              `yaml:"new_destination_days,omitempty" json:"new_destination_days,omitempty"`
	AmountDeviationMultiplier *decimal.Decimal `yaml:"amount_deviation_multiplier,omitempty" json:"amount_deviation_multiplier,omitempty" swaggertype:"string"`
	TimeDeviationHours        int              `yaml:"time_deviation_hours,omitempty" json:"time_deviation_hours,omitempty"`
	VelocitySpikeMultiplier   *decimal.Decimal `yaml:"velocity_spike_multiplier,omitempty" json:"velocity_spike_multiplier,omitempty" swaggertype:"string"`
	DormantDays               int              `yaml:"dormant_days,omitempty" json:"dormant_days,omitempty"`
	// Screening: the providers or the list a screening rule asks.
	Providers []string `yaml:"providers,omitempty" json:"providers,omitempty"`
	List      string   `yaml:"list,omitempty" json:"list,omitempty"`
	Screen    string   `yaml:"screen,omitempty" json:"screen,omitempty"`
}

// File is config/aml_rules.yaml.
type File struct {
	Rules  []Rule       `yaml:"aml_rules"`
	Engine EngineConfig `yaml:"engine"`
}

// EngineConfig is how the engine runs.
type EngineConfig struct {
	// EvaluationIntervalSeconds is the interval of the batch evaluation
	// and of the escalation of old alerts.
	EvaluationIntervalSeconds int `yaml:"evaluation_interval_seconds"`
	// AlertEscalationHours escalates an alert that stays open that long.
	AlertEscalationHours int           `yaml:"alert_escalation_hours"`
	Notifications        Notifications `yaml:"notifications"`
}

// Notifications are where the alerts that hold or refuse an operation, and
// the escalations, are sent.
type Notifications struct {
	Email struct {
		Enabled    bool     `yaml:"enabled"`
		Recipients []string `yaml:"recipients"`
	} `yaml:"email"`
}

// Load reads and checks the rules file at path.
func Load(path string) (File, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the operator's setting, compliance.aml.rules_path.
	if err != nil {
		return File{}, fmt.Errorf("aml: read the rules: %w", err)
	}
	return Parse(raw)
}

// Parse reads and checks a rules file: every key must be known, and every
// problem is reported at once.
func Parse(raw []byte) (File, error) {
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return File{}, fmt.Errorf("aml: the rules file: %w", err)
	}
	if err := f.Validate(); err != nil {
		return File{}, err
	}
	return f, nil
}

var (
	codePattern    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	countryPattern = regexp.MustCompile(`^[A-Z]{2}$`)
	emailPattern   = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// Problems lists every problem of a rules file.
type Problems []string

func (p Problems) Error() string { return "aml: the rules file: " + strings.Join(p, "; ") }

// Validate checks every rule and the engine.
func (f File) Validate() error {
	var p Problems
	seen := map[string]bool{}
	for i, r := range f.Rules {
		at := fmt.Sprintf("rule %d", i+1)
		if r.Code != "" {
			at = "rule " + r.Code
		}
		if seen[r.Code] {
			p = append(p, at+": the id is used twice")
		}
		seen[r.Code] = true
		for _, problem := range r.problems() {
			p = append(p, at+": "+problem)
		}
	}
	e := f.Engine
	if e.EvaluationIntervalSeconds < 10 || e.EvaluationIntervalSeconds > 86400 {
		p = append(p, "engine: evaluation_interval_seconds must be 10 to 86400")
	}
	if e.AlertEscalationHours < 1 || e.AlertEscalationHours > 24*30 {
		p = append(p, "engine: alert_escalation_hours must be 1 to 720")
	}
	if e.Notifications.Email.Enabled && len(e.Notifications.Email.Recipients) == 0 {
		p = append(p, "engine: notifications.email needs recipients")
	}
	for _, to := range e.Notifications.Email.Recipients {
		if !emailPattern.MatchString(to) {
			p = append(p, fmt.Sprintf("engine: notifications.email: %q is not an email address", to))
		}
	}
	if len(p) > 0 {
		return p
	}
	return nil
}

// problems lists the problems of one rule.
func (r Rule) problems() []string {
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	if !codePattern.MatchString(r.Code) {
		add("id must be 1 to 64 lower-case letters, digits, or underscores, from a letter")
	}
	if n := len(r.Name); n < 1 || n > 100 {
		add("name must be 1 to 100 characters")
	}
	if len(r.AppliesTo) == 0 {
		add("applies_to is required")
	}
	for _, k := range r.AppliesTo {
		if !slices.Contains(kinds, k) {
			add("applies_to: %q is not deposit, withdrawal, or trade", k)
		}
	}
	switch r.Action {
	case ActionAlert, ActionReview, ActionBlock, ActionFreeze:
	default:
		add("action must be ALERT, REVIEW, BLOCK, or FREEZE")
	}
	if _, ok := severities[r.Severity]; !ok {
		add("severity must be LOW, MEDIUM, HIGH, or CRITICAL")
	}
	if r.ThresholdCount < 0 {
		add("threshold_count is negative")
	}
	if r.ThresholdAmount.IsNegative() {
		add("threshold_amount is negative")
	}
	if r.WindowHours < 1 || r.WindowHours > 24*90 {
		add("time_window_hours must be 1 to 2160")
	}
	c := r.Config
	used := map[string]bool{}
	switch r.Type {
	case TypeVelocity:
		used["metric"] = true
		switch c.Metric {
		case MetricCount:
			if r.ThresholdCount < 1 {
				add("a count needs threshold_count")
			}
		case MetricVolume, MetricSingle:
			if !r.ThresholdAmount.IsPositive() {
				add("a %s needs threshold_amount", c.Metric)
			}
		case MetricCycles:
			if r.ThresholdCount < 1 {
				add("cycles need threshold_count")
			}
			if !slices.Contains(r.AppliesTo, KindWithdrawal) {
				add("cycles apply to withdrawals")
			}
		default:
			add("config.metric must be count, volume, deposit_withdrawal_cycles, or single_amount")
		}
	case TypeStructuring:
		used["ceiling"] = true
		if !r.ThresholdAmount.IsPositive() {
			add("structuring needs threshold_amount, the bottom of the band")
		}
		if c.Ceiling == nil || !c.Ceiling.GreaterThan(r.ThresholdAmount) {
			add("structuring needs config.ceiling above threshold_amount")
		}
		if r.ThresholdCount < 2 {
			add("structuring needs threshold_count of at least 2")
		}
	case TypeHighRiskCountry:
		used["countries"], used["check_fields"], used["cross_border"], used["home_country"] = true, true, true, true
		if len(c.CheckFields) == 0 {
			add("config.check_fields is required")
		}
		for _, field := range c.CheckFields {
			if field != FieldIPCountry && field != FieldCounterpartyCountry {
				add("config.check_fields: %q is not ip_country or counterparty_country", field)
			}
		}
		for _, country := range c.Countries {
			if !countryPattern.MatchString(country) {
				add("config.countries: %q is not an ISO 3166-1 alpha-2 code", country)
			}
		}
		switch {
		case c.CrossBorder && !countryPattern.MatchString(c.HomeCountry):
			add("cross_border needs config.home_country")
		case !c.CrossBorder && c.HomeCountry != "":
			add("config.home_country is for cross_border")
		case !c.CrossBorder && len(c.Countries) == 0:
			add("config.countries is required")
		}
	case TypeUnusualPattern:
		used["pattern"], used["history_days"] = true, true
		switch c.Pattern {
		case PatternWithdrawalProfile:
			used["checks"], used["new_destination_days"], used["amount_deviation_multiplier"] = true, true, true
			used["time_deviation_hours"], used["velocity_spike_multiplier"] = true, true
			if !slices.Equal(r.AppliesTo, []Kind{KindWithdrawal}) {
				add("the withdrawal profile applies to withdrawals only")
			}
			p = append(p, profileProblems(r)...)
		case PatternDormantAccount:
			used["dormant_days"] = true
			if c.DormantDays < 1 || c.DormantDays > 3650 {
				add("config.dormant_days must be 1 to 3650")
			}
		default:
			add("config.pattern must be withdrawal_profile or dormant_account")
		}
	case TypeMixer, TypeDarknet:
		used["list"] = true
		if c.List == "" {
			add("config.list is required")
		}
	case TypeSanctions:
		used["providers"], used["screen"] = true, true
		if len(c.Providers) == 0 {
			add("config.providers is required")
		}
		for _, provider := range c.Providers {
			if provider != "ofac" && provider != "chainalysis" && provider != "trm" {
				add("config.providers: %q is not ofac, chainalysis, or trm", provider)
			}
		}
		if c.Screen != "user" && c.Screen != "counterparty" {
			add("config.screen must be user or counterparty")
		}
	case TypePEP:
		used["providers"] = true
		if len(c.Providers) == 0 {
			add("config.providers is required")
		}
	default:
		add("rule_type %q is not known", r.Type)
	}
	for _, field := range c.set() {
		if !used[field] {
			add("config.%s is not used by %s", field, r.Type)
		}
	}
	return p
}

func profileProblems(r Rule) []string {
	var p []string
	c := r.Config
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	if len(c.Checks) == 0 {
		add("config.checks is required")
	}
	one := decimal.NewFromInt(1)
	for _, check := range c.Checks {
		switch check {
		case CheckNewDestination:
			if c.NewDestinationDays < 1 {
				add("new_destination needs config.new_destination_days")
			}
		case CheckAmount:
			if c.AmountDeviationMultiplier == nil || !c.AmountDeviationMultiplier.GreaterThan(one) {
				add("amount_deviation needs config.amount_deviation_multiplier above 1")
			}
		case CheckTime:
			if c.TimeDeviationHours < 1 || c.TimeDeviationHours > 11 {
				add("time_deviation needs config.time_deviation_hours of 1 to 11")
			}
		case CheckVelocity:
			if c.VelocitySpikeMultiplier == nil || !c.VelocitySpikeMultiplier.GreaterThan(one) {
				add("velocity_spike needs config.velocity_spike_multiplier above 1")
			}
		default:
			add("config.checks: %q is not known", check)
		}
	}
	if c.HistoryDays < 1 || c.HistoryDays > 365 {
		add("config.history_days must be 1 to 365")
	}
	if r.ThresholdCount < 1 || r.ThresholdCount > len(c.Checks) {
		add("threshold_count must be 1 to the number of checks")
	}
	return p
}

// set lists the fields of c that are set.
func (c RuleConfig) set() []string {
	var s []string
	for name, isSet := range map[string]bool{
		"metric": c.Metric != "", "ceiling": c.Ceiling != nil, "countries": len(c.Countries) > 0,
		"check_fields": len(c.CheckFields) > 0, "cross_border": c.CrossBorder, "home_country": c.HomeCountry != "",
		"pattern": c.Pattern != "", "checks": len(c.Checks) > 0, "history_days": c.HistoryDays != 0,
		"new_destination_days": c.NewDestinationDays != 0, "amount_deviation_multiplier": c.AmountDeviationMultiplier != nil,
		"time_deviation_hours": c.TimeDeviationHours != 0, "velocity_spike_multiplier": c.VelocitySpikeMultiplier != nil,
		"dormant_days": c.DormantDays != 0, "providers": len(c.Providers) > 0, "list": c.List != "", "screen": c.Screen != "",
	} {
		if isSet {
			s = append(s, name)
		}
	}
	sort.Strings(s)
	return s
}

// ErrHomeCountryListed is a rules file that lists the country of the
// deployment as high-risk, which would refuse nearly every user.
var ErrHomeCountryListed = errors.New("aml: a high-risk country rule lists the country of the deployment")

// CheckHome refuses a file whose enabled country rules list home, the
// country of the deployment, as high-risk. An empty home checks nothing.
func (f File) CheckHome(home string) error {
	if home == "" {
		return nil
	}
	var listed []string
	for _, r := range f.Rules {
		if r.Type == TypeHighRiskCountry && slices.Contains(r.Config.Countries, home) {
			listed = append(listed, r.Code)
		}
	}
	if len(listed) > 0 {
		return fmt.Errorf("%w: %s lists %s", ErrHomeCountryListed, strings.Join(listed, ", "), home)
	}
	return nil
}

// definition is the canonical JSON of the rule: what its version covers.
func (r Rule) definition() ([]byte, error) {
	c := r
	c.AppliesTo = slices.Clone(r.AppliesTo)
	slices.Sort(c.AppliesTo)
	return json.Marshal(c)
}

// configJSON is the config of the rule, as aml_rules stores it.
func (r Rule) configJSON() ([]byte, error) { return json.Marshal(r.Config) }

// Applies reports whether r applies to operations of kind k.
func (r Rule) Applies(k Kind) bool { return slices.Contains(r.AppliesTo, k) }

// RiskScore is the risk score of an alert of the rule.
func (r Rule) RiskScore() string { return severities[r.Severity] }
