package aml

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	files "github.com/mdastpak/crypto-exchange/config"
)

// The rules file of the release loads, lists no rule twice, and does not
// list Iran as high-risk, which would refuse nearly every user of an
// Iranian deployment (#73).
func TestBuiltInRules_Load(t *testing.T) {
	f, err := Parse(files.AMLRules)
	require.NoError(t, err)
	assert.NotEmpty(t, f.Rules)
	assert.NoError(t, f.CheckHome("IR"))
	for _, r := range f.Rules {
		assert.NotContains(t, r.Config.Countries, "IR", r.Code)
	}
}

const minimal = `
aml_rules:
  - id: "big"
    name: "Big"
    rule_type: "VELOCITY"
    enabled: true
    applies_to: ["withdrawal"]
    config:
      metric: "single_amount"
    threshold_amount: "10000"
    time_window_hours: 1
    action: "ALERT"
    severity: "MEDIUM"
engine:
  evaluation_interval_seconds: 60
  alert_escalation_hours: 24
`

func TestParse_RefusesWhatItDoesNotKnow(t *testing.T) {
	_, err := Parse([]byte(minimal))
	require.NoError(t, err)

	for name, c := range map[string]struct{ from, to, problem string }{
		"unknown key":    {`    severity: "MEDIUM"`, "    severity: \"MEDIUM\"\n    group_by: \"user_id\"", "group_by"},
		"unused config":  {`      metric: "single_amount"`, "      metric: \"single_amount\"\n      ceiling: \"5\"", "config.ceiling is not used by VELOCITY"},
		"no threshold":   {`    threshold_amount: "10000"`, `    threshold_amount: "0"`, "a single_amount needs threshold_amount"},
		"bad action":     {`    action: "ALERT"`, `    action: "LOG"`, "action must be"},
		"bad kind":       {`["withdrawal"]`, `["payout"]`, `"payout" is not deposit, withdrawal, or trade`},
		"bad type":       {`"VELOCITY"`, `"ML"`, `rule_type "ML" is not known`},
		"bad window":     {`time_window_hours: 1`, `time_window_hours: 0`, "time_window_hours must be 1 to 2160"},
		"bad escalation": {`alert_escalation_hours: 24`, `alert_escalation_hours: 0`, "alert_escalation_hours"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(strings.Replace(minimal, c.from, c.to, 1)))
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.problem)
		})
	}
}

// A file that lists the country of the deployment as high-risk is refused
// at startup (#73).
func TestCheckHome(t *testing.T) {
	f := File{Rules: []Rule{{Code: "c", Type: TypeHighRiskCountry, Config: RuleConfig{Countries: []string{"KP", "IR"}}}}}
	assert.ErrorIs(t, f.CheckHome("IR"), ErrHomeCountryListed)
	assert.NoError(t, f.CheckHome(""))
	assert.NoError(t, f.CheckHome("AE"))
}

func TestAction_Stronger(t *testing.T) {
	assert.Equal(t, ActionReview, ActionAlert.Stronger(ActionReview))
	assert.Equal(t, ActionFreeze, ActionFreeze.Stronger(ActionBlock))
	assert.Equal(t, ActionAlert, ActionNone.Stronger(ActionAlert))
	assert.True(t, ActionBlock.Refuses())
	assert.False(t, ActionReview.Refuses())
}

var at = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func usdt(s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

func dec(s string) *decimal.Decimal { return usdt(s) }

func rule(r Rule) Stored {
	if r.WindowHours == 0 {
		r.WindowHours = 24
	}
	if r.Action == "" {
		r.Action = ActionReview
	}
	r.Severity, r.Name, r.Enabled = "HIGH", "test", true
	if r.Code == "" {
		r.Code = "test"
	}
	if err := r.problems(); len(err) > 0 {
		panic(strings.Join(err, "; "))
	}
	return Stored{ID: "00000000-0000-0000-0000-000000000001", Version: 1, Rule: r}
}

func withdrawal(value *decimal.Decimal, ago time.Duration) past {
	return past{kind: KindWithdrawal, value: value, at: at.Add(-ago)}
}

func run(t *testing.T, r Stored, cur past, h history, ev Event) *Hit {
	t.Helper()
	hit, err := evaluate(context.Background(), nil, r, ev, cur, h, at)
	require.NoError(t, err)
	return hit
}

func TestVelocity(t *testing.T) {
	count := rule(Rule{Type: TypeVelocity, AppliesTo: []Kind{KindWithdrawal}, ThresholdCount: 3, Config: RuleConfig{Metric: MetricCount}})
	two := history{events: []past{withdrawal(usdt("1"), time.Hour), withdrawal(usdt("1"), 2*time.Hour)}}
	assert.NotNil(t, run(t, count, withdrawal(usdt("1"), 0), two, Event{}), "the third in the window hits")
	old := history{events: []past{withdrawal(usdt("1"), time.Hour), withdrawal(usdt("1"), 25*time.Hour)}}
	assert.Nil(t, run(t, count, withdrawal(usdt("1"), 0), old, Event{}), "one outside the window does not count")

	volume := rule(Rule{Type: TypeVelocity, AppliesTo: []Kind{KindWithdrawal}, ThresholdAmount: decimal.NewFromInt(100),
		Config: RuleConfig{Metric: MetricVolume}})
	hit := run(t, volume, withdrawal(usdt("40"), 0), history{events: []past{withdrawal(usdt("60"), time.Hour)}}, Event{})
	require.NotNil(t, hit)
	assert.Equal(t, "100", hit.Data["value_usdt"])
	assert.Nil(t, run(t, volume, withdrawal(usdt("39"), 0), history{events: []past{withdrawal(usdt("60"), time.Hour)}}, Event{}))

	single := rule(Rule{Type: TypeVelocity, AppliesTo: []Kind{KindWithdrawal}, ThresholdAmount: decimal.NewFromInt(10000),
		Action: ActionAlert, Config: RuleConfig{Metric: MetricSingle}})
	assert.Equal(t, ActionAlert, run(t, single, withdrawal(usdt("10000"), 0), history{}, Event{}).Action)
	assert.Nil(t, run(t, single, withdrawal(usdt("9999.99"), 0), history{}, Event{}))

	cycles := rule(Rule{Type: TypeVelocity, AppliesTo: []Kind{KindWithdrawal}, ThresholdCount: 2, Config: RuleConfig{Metric: MetricCycles}})
	deposit := past{kind: KindDeposit, value: usdt("1"), at: at.Add(-3 * time.Hour)}
	h := history{events: []past{withdrawal(usdt("1"), 4*time.Hour), deposit, withdrawal(usdt("1"), 2*time.Hour)}}
	hit = run(t, cycles, withdrawal(usdt("1"), 0), h, Event{})
	require.NotNil(t, hit, "two withdrawals after a deposit")
	assert.Equal(t, 2, hit.Data["cycles"])
	assert.Nil(t, run(t, cycles, withdrawal(usdt("1"), 0), history{events: []past{withdrawal(usdt("1"), 2*time.Hour)}}, Event{}),
		"no deposit, no cycle")
}

// An amount that needs a price that is not current fails closed: the
// operation is held for review, whatever the rule's own action (#73).
func TestNoPrice_FailsClosedToReview(t *testing.T) {
	single := rule(Rule{Type: TypeVelocity, AppliesTo: []Kind{KindWithdrawal}, ThresholdAmount: decimal.NewFromInt(10000),
		Action: ActionAlert, Config: RuleConfig{Metric: MetricSingle}})
	hit := run(t, single, withdrawal(nil, 0), history{}, Event{})
	require.NotNil(t, hit)
	assert.Equal(t, ActionReview, hit.Action)
	assert.Equal(t, reasonNoPrice, hit.Data["reason"])

	block := rule(Rule{Type: TypeVelocity, AppliesTo: []Kind{KindWithdrawal}, ThresholdAmount: decimal.NewFromInt(100),
		Action: ActionBlock, Config: RuleConfig{Metric: MetricVolume}})
	hit = run(t, block, withdrawal(usdt("1"), 0), history{events: []past{withdrawal(nil, time.Hour)}}, Event{})
	require.NotNil(t, hit)
	assert.Equal(t, ActionReview, hit.Action, "a rule that blocks holds instead, without a price")
}

func TestStructuring(t *testing.T) {
	r := rule(Rule{Type: TypeStructuring, AppliesTo: []Kind{KindWithdrawal}, ThresholdAmount: decimal.NewFromInt(9000),
		ThresholdCount: 3, Action: ActionBlock, Config: RuleConfig{Ceiling: dec("10000")}})
	two := history{events: []past{withdrawal(usdt("9500"), time.Hour), withdrawal(usdt("9100"), 2*time.Hour)}}
	hit := run(t, r, withdrawal(usdt("9999"), 0), two, Event{})
	require.NotNil(t, hit, "the third just below the threshold")
	assert.Equal(t, ActionBlock, hit.Action)
	assert.Equal(t, 3, hit.Data["count"])
	assert.Nil(t, run(t, r, withdrawal(usdt("10000"), 0), two, Event{}), "one at the threshold is reported, not structured")
	assert.Nil(t, run(t, r, withdrawal(usdt("8999"), 0), two, Event{}), "one below the band")
	other := history{events: []past{withdrawal(usdt("9500"), time.Hour), withdrawal(usdt("100"), 2*time.Hour)}}
	assert.Nil(t, run(t, r, withdrawal(usdt("9500"), 0), other, Event{}))
	unknown := history{events: []past{withdrawal(usdt("9500"), time.Hour), withdrawal(nil, 2*time.Hour)}}
	assert.Equal(t, ActionReview, run(t, r, withdrawal(usdt("9500"), 0), unknown, Event{}).Action,
		"an operation without a value could complete the pattern")
}

func TestHighRiskCountry(t *testing.T) {
	r := rule(Rule{Type: TypeHighRiskCountry, AppliesTo: []Kind{KindWithdrawal}, Action: ActionBlock,
		Config: RuleConfig{Countries: []string{"KP", "SY"}, CheckFields: []string{FieldCounterpartyCountry, FieldIPCountry}}})
	cur := withdrawal(usdt("1"), 0)
	cur.ipCountry = "SY"
	hit := run(t, r, cur, history{}, Event{})
	require.NotNil(t, hit)
	assert.Equal(t, "SY", hit.Data["country"])
	cur.ipCountry, cur.counterpartyCountry = "IR", "IR"
	assert.Nil(t, run(t, r, cur, history{}, Event{}))
	cur.ipCountry, cur.counterpartyCountry = "", ""
	assert.Nil(t, run(t, r, cur, history{}, Event{}), "an unknown country is no match")

	cross := rule(Rule{Type: TypeHighRiskCountry, AppliesTo: []Kind{KindWithdrawal}, ThresholdAmount: decimal.NewFromInt(50000),
		Config: RuleConfig{CrossBorder: true, HomeCountry: "IR", CheckFields: []string{FieldCounterpartyCountry}}})
	abroad := func(value string, ago time.Duration) past {
		p := withdrawal(usdt(value), ago)
		p.counterpartyCountry = "AE"
		return p
	}
	home := withdrawal(usdt("90000"), 2*time.Hour)
	home.counterpartyCountry = "IR"
	h := history{events: []past{abroad("30000", time.Hour), home}}
	hit = run(t, cross, abroad("20000", 0), h, Event{})
	require.NotNil(t, hit, "50000 abroad in the window")
	assert.Equal(t, "50000", hit.Data["value_usdt"])
	assert.Nil(t, run(t, cross, abroad("19999", 0), h, Event{}), "what stays home does not count")
}

func TestWithdrawalProfile(t *testing.T) {
	r := rule(Rule{Type: TypeUnusualPattern, AppliesTo: []Kind{KindWithdrawal}, ThresholdCount: 2, Config: RuleConfig{
		Pattern: PatternWithdrawalProfile, Checks: []string{CheckNewDestination, CheckAmount, CheckTime, CheckVelocity},
		HistoryDays: 30, NewDestinationDays: 30, AmountDeviationMultiplier: dec("5"), TimeDeviationHours: 6,
		VelocitySpikeMultiplier: dec("5")}})
	// Three withdrawals of 100 at noon over the month.
	usual := history{events: []past{withdrawal(usdt("100"), 10*24*time.Hour), withdrawal(usdt("100"), 20*24*time.Hour),
		withdrawal(usdt("100"), 25*24*time.Hour)}}
	added := at.Add(-24 * time.Hour)
	fresh := Event{CounterpartySince: &added}
	hit := run(t, r, withdrawal(usdt("500"), 0), usual, fresh)
	require.NotNil(t, hit, "a new destination and five times the average")
	assert.Equal(t, []string{CheckNewDestination, CheckAmount}, hit.Data["checks"])

	old := at.Add(-90 * 24 * time.Hour)
	assert.Nil(t, run(t, r, withdrawal(usdt("500"), 0), usual, Event{CounterpartySince: &old}), "one check is not enough")

	night := past{kind: KindWithdrawal, value: usdt("100"), at: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)}
	nightAt := night.at
	h, err := evaluate(context.Background(), nil, r, Event{CounterpartySince: &added}, night, usual, nightAt)
	require.NoError(t, err)
	require.NotNil(t, h, "a new destination at midnight, against a profile at noon")
	assert.Equal(t, []string{CheckNewDestination, CheckTime}, h.Data["checks"])

	burst := history{events: append([]past{withdrawal(usdt("100"), time.Hour), withdrawal(usdt("100"), 2*time.Hour)}, usual.events...)}
	hit = run(t, r, withdrawal(usdt("100"), 0), burst, fresh)
	require.NotNil(t, hit, "three in a day against five in a month")
	assert.Contains(t, hit.Data["checks"], CheckVelocity)

	assert.Nil(t, run(t, r, withdrawal(usdt("500"), 0), history{}, Event{}), "no history, no profile")
}

func TestDormantAccount(t *testing.T) {
	r := rule(Rule{Type: TypeUnusualPattern, AppliesTo: []Kind{KindWithdrawal}, Config: RuleConfig{Pattern: PatternDormantAccount, DormantDays: 90}})
	long := at.Add(-100 * 24 * time.Hour)
	recent := at.Add(-10 * 24 * time.Hour)
	assert.NotNil(t, run(t, r, withdrawal(usdt("1"), 0), history{lastAt: &long, started: &long}, Event{}))
	assert.Nil(t, run(t, r, withdrawal(usdt("1"), 0), history{lastAt: &recent, started: &long}, Event{}))

	joined := at.Add(-400 * 24 * time.Hour)
	assert.NotNil(t, run(t, r, withdrawal(usdt("1"), 0), history{joinedAt: joined, started: &long}, Event{}),
		"an old account without an operation since the engine started")
	assert.Nil(t, run(t, r, withdrawal(usdt("1"), 0), history{joinedAt: joined, started: &recent}, Event{}),
		"the accounts of before the engine are not all dormant on its first days")
	assert.Nil(t, run(t, r, withdrawal(usdt("1"), 0), history{joinedAt: joined}, Event{}), "the first operation the engine sees")
}
