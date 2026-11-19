package otc

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/aml"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
)

// rulesOf is a rule of the trades that hits from the threshold-th trade of
// the hour on, with action.
func rulesOf(threshold int, action aml.Action) string {
	return fmt.Sprintf(`
aml_rules:
  - id: "trades_per_hour"
    name: "Trades per hour"
    rule_type: "VELOCITY"
    enabled: true
    applies_to: ["trade"]
    config:
      metric: "count"
    threshold_count: %d
    time_window_hours: 1
    action: %q
    severity: "HIGH"
engine:
  evaluation_interval_seconds: 60
  alert_escalation_hours: 24
`, threshold, action)
}

// withRules gives the executor the AML rules of the trades.
func withRules(t *testing.T, f execFixture, rules string) execFixture {
	t.Helper()
	file, err := aml.Parse([]byte(rules))
	require.NoError(t, err)
	_, err = aml.Sync(context.Background(), f.DB, file)
	require.NoError(t, err)
	f.exec.AML = &aml.Engine{}
	return f
}

func (f execFixture) alerts(t *testing.T) []aml.Alert {
	t.Helper()
	list, err := aml.Alerts(context.Background(), f.DB, aml.StatusOpen, f.user, 20)
	require.NoError(t, err)
	return list
}

// Every OTC trade is evaluated by the rules of the trades, in its
// transaction (#552): an alert lets it settle; a review or a block refuses
// it, with its own code, and leaves no trace of it but the quote it took
// and the refusal recorded; a freeze also freezes the account.
func TestExecutor_AMLRulesOfTheTrades(t *testing.T) {
	for _, c := range []struct {
		action aml.Action
		code   string
	}{
		{aml.ActionAlert, ""},
		{aml.ActionReview, CodeTradeUnderReview},
		{aml.ActionBlock, CodeTradeRefusedCompliance},
		{aml.ActionFreeze, CodeTradeRefusedCompliance},
	} {
		t.Run(string(c.action), func(t *testing.T) {
			ctx := context.Background()
			f := withRules(t, newExecFixture(t), rulesOf(1, c.action))
			before := f.balance(t, f.market.QuoteHK)
			q := f.quote(t, SideBuy, "0.5", "60000", "60")
			out, err := f.execute(q, "k1")
			alerts := f.alerts(t)
			require.Len(t, alerts, 1, "the rule's alert")
			assert.Equal(t, "trades_per_hour", alerts[0].RuleCode)

			if c.code == "" {
				require.NoError(t, err)
				assert.Equal(t, out.Trade.ID, alerts[0].EventRef, "the alert names the trade")
				assert.Equal(t, 1, f.counts(t)[0], "the trade settled")
				return
			}
			var refusal *Refusal
			require.ErrorAs(t, err, &refusal)
			assert.Equal(t, c.code, refusal.Code)
			assert.Equal(t, [3]int{}, f.counts(t), "no trade, no events, no ledger entry")
			assert.Equal(t, before, f.balance(t, f.market.QuoteHK), "the balance is untouched")
			_, err = f.execute(q, "k2")
			require.ErrorAs(t, err, &refusal)
			assert.Equal(t, CodeQuoteNotFound, refusal.Code, "the quote was consumed")

			frozen := restrictions.Check(ctx, f.DB, f.user, restrictions.OpBuy, f.market.BaseHK)
			if c.action == aml.ActionFreeze {
				assert.Error(t, frozen, "the account is frozen")
			} else {
				assert.NoError(t, frozen)
			}
		})
	}
}

// A velocity rule of three trades an hour refuses the fourth (#552).
func TestExecutor_AVelocityRuleOfTheTrades(t *testing.T) {
	f := withRules(t, newExecFixture(t), rulesOf(4, aml.ActionBlock))
	for i := range 3 {
		_, err := f.execute(f.quote(t, SideBuy, "0.01", "60000", "1"), fmt.Sprintf("v%d", i))
		require.NoError(t, err, "trade %d", i+1)
	}
	_, err := f.execute(f.quote(t, SideBuy, "0.01", "60000", "1"), "v3")
	var refusal *Refusal
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, CodeTradeRefusedCompliance, refusal.Code)
	assert.Equal(t, 3, f.counts(t)[0])
}

// While the rules cannot be evaluated, as for a rule changed in SQL, the
// trade is refused: the rules of the trades fail closed (#552).
func TestExecutor_RulesThatCannotBeEvaluatedRefuseTheTrade(t *testing.T) {
	f := withRules(t, newExecFixture(t), rulesOf(1, aml.ActionAlert))
	_, err := f.DB.Exec(`UPDATE aml_rules SET threshold_count = 99`)
	require.NoError(t, err)
	_, err = f.execute(f.quote(t, SideBuy, "0.5", "60000", "60"), "k1")
	var refusal *Refusal
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, CodeTradeScreeningUnavailable, refusal.Code)
	assert.Equal(t, [3]int{}, f.counts(t))
}
