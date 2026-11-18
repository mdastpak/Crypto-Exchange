package withdrawals

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/aml"
	"github.com/mdastpak/crypto-exchange/internal/confirm"
)

const amlRules = `
aml_rules:
  - id: "hold_large"
    name: "Hold Large"
    rule_type: "VELOCITY"
    enabled: true
    applies_to: ["withdrawal"]
    config:
      metric: "single_amount"
    threshold_amount: "150"
    time_window_hours: 1
    action: "REVIEW"
    severity: "HIGH"
  - id: "sanctioned_ip"
    name: "Sanctioned IP"
    rule_type: "HIGH_RISK_COUNTRY"
    enabled: true
    applies_to: ["withdrawal"]
    config:
      countries: ["KP"]
      check_fields: ["ip_country"]
    time_window_hours: 1
    action: "BLOCK"
    severity: "CRITICAL"
engine:
  evaluation_interval_seconds: 60
  alert_escalation_hours: 24
`

// withAML gives the fixture the AML rules above, valued in USDT.
func withAML(t *testing.T, f fixture) fixture {
	t.Helper()
	file, err := aml.Parse([]byte(amlRules))
	require.NoError(t, err)
	_, err = aml.Sync(context.Background(), f.DB, file)
	require.NoError(t, err)
	f.AML = &aml.Engine{}
	return f
}

func (f fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.DB.QueryRow(query, args...).Scan(&n))
	return n
}

// A withdrawal that an AML rule holds waits for an administrator, with
// its alert, which names it (#73).
func TestAML_HoldsForReview(t *testing.T) {
	f := withAML(t, newFixture(t, ""))
	w, err := f.withdraw(f.request("200", "large"))
	require.NoError(t, err)
	assert.Equal(t, StatusReview, w.Status)
	assert.Equal(t, ReviewAML, w.ReviewReason)

	alerts, err := aml.Alerts(context.Background(), f.DB, aml.StatusOpen, f.user, 10)
	require.NoError(t, err)
	require.Len(t, alerts, 1)
	assert.Equal(t, "hold_large", alerts[0].RuleCode)
	assert.Equal(t, w.ID, alerts[0].EventRef)

	w, err = f.withdraw(f.request("100", "small"))
	require.NoError(t, err)
	assert.Equal(t, StatusQueued, w.Status, "below the rule")
}

// A refusal at step one sends no code; one at step two locks nothing. The
// refusal and its alert are kept, though the request rolled back (#73).
func TestAML_Refuses(t *testing.T) {
	ctx := context.Background()
	f := withAML(t, newFixture(t, ""))

	r := f.request("100", "from-kp")
	r.Country = "KP"
	_, err := f.Request(ctx, f.user, r, "client", origin)
	require.ErrorIs(t, err, ErrRefused)
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM operation_confirmations`), "no code was sent")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM aml_events WHERE refused`))
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM aml_alerts WHERE action = 'BLOCK'`))

	// Step one from home, step two from the sanctioned country.
	r = f.request("100", "then-kp")
	out, err := f.Request(ctx, f.user, r, "client", origin)
	require.NoError(t, err)
	require.NotNil(t, out.Challenge)
	require.NoError(t, f.confirm.Confirm(ctx, f.user, out.Challenge.ID, confirm.Proof{Code: "123456"}, origin))
	r.ConfirmationID, r.Country = out.Challenge.ID, "KP"
	_, err = f.Request(ctx, f.user, r, "client", origin)
	require.ErrorIs(t, err, ErrRefused)
	available, locked := f.balance()
	assert.Equal(t, "1000", available)
	assert.Equal(t, "0", locked)
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM withdrawals`))
	assert.Equal(t, 2, f.count(`SELECT COUNT(*) FROM aml_alerts WHERE action = 'BLOCK'`))
}

// While a rule fails its seal, no withdrawal is evaluated, so none runs
// (#73).
func TestAML_RuleChangedInSQL_RefusesEveryWithdrawal(t *testing.T) {
	f := withAML(t, newFixture(t, ""))
	_, err := f.DB.Exec(`UPDATE aml_rules SET enabled = false WHERE code = 'sanctioned_ip'`)
	require.NoError(t, err)
	_, err = f.Request(context.Background(), f.user, f.request("100", "any"), "client", origin)
	require.ErrorIs(t, err, aml.ErrUnavailable)
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM operation_confirmations`))
}
