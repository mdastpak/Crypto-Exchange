package app

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/aml"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/withdrawals"
)

const e2eAMLRules = `
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
  - id: "refuse_larger"
    name: "Refuse Larger"
    rule_type: "VELOCITY"
    enabled: true
    applies_to: ["withdrawal"]
    config:
      metric: "single_amount"
    threshold_amount: "500"
    time_window_hours: 1
    action: "BLOCK"
    severity: "CRITICAL"
engine:
  evaluation_interval_seconds: 60
  alert_escalation_hours: 24
`

// withAMLRules turns compliance.aml on with rules from a file.
func withAMLRules(t *testing.T, rules string) func(*config.Configuration) {
	path := filepath.Join(t.TempDir(), "aml_rules.yaml")
	require.NoError(t, os.WriteFile(path, []byte(rules), 0o600))
	return func(c *config.Configuration) {
		c.Compliance.AML.Enabled, c.Compliance.AML.RulesPath = true, path
	}
}

// TestAML walks through #73 on the routes: a rule holds a withdrawal for
// review, another refuses one before any code is sent, and an
// administrator reads the rules and works the alerts.
func TestAML(t *testing.T) {
	env := newE2EEnv(t, withAMLRules(t, e2eAMLRules))
	u := newWithdrawer(t, env, "aml@example.com")

	w := u.withdraw(t, "200", "aml-held")
	assert.Equal(t, withdrawals.StatusReview, w.Status)
	assert.Equal(t, withdrawals.ReviewAML, w.ReviewReason)

	sent := env.sender.count(u.email)
	status, raw := u.send(t, u.request("600"), "aml-refused")
	require.Equal(t, http.StatusForbidden, status, string(raw))
	assert.Contains(t, string(raw), "withdrawal_refused")
	assert.NotContains(t, string(raw), "refuse_larger", "the user is not told the rule")
	assert.Equal(t, sent, env.sender.count(u.email), "no code was sent")

	enrollment, err := env.accounts.Create(t.Context(), "aml-ops@example.com")
	require.NoError(t, err)
	token := env.signInAdmin(t, enrollment)

	status, raw = env.call(t, http.MethodGet, "/api/v1/admin/aml/rules", nil, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Contains(t, string(raw), "hold_large")

	status, raw = env.call(t, http.MethodGet, "/api/v1/admin/aml/alerts?user_hk="+u.userID, nil, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	var listed struct {
		Info []aml.Alert `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &listed))
	// The held withdrawal, and the refused one, which both rules hit.
	require.Len(t, listed.Info, 3)
	var held aml.Alert
	refused := map[string]aml.Action{}
	for _, a := range listed.Info {
		if a.EventRef == w.ID {
			held = a
		} else {
			refused[a.RuleCode] = a.Action
		}
	}
	assert.Equal(t, "hold_large", held.RuleCode)
	assert.Equal(t, aml.ActionReview, held.Action)
	assert.Equal(t, map[string]aml.Action{"hold_large": aml.ActionReview, "refuse_larger": aml.ActionBlock}, refused)

	id := held.ID
	status, raw = env.call(t, http.MethodGet, "/api/v1/admin/aml/alerts/"+id, nil, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	status, raw = env.call(t, http.MethodPost, "/api/v1/admin/aml/alerts/"+id+"/transition",
		map[string]string{"status": "INVESTIGATING"}, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	status, raw = env.call(t, http.MethodPost, "/api/v1/admin/aml/alerts/"+id+"/transition",
		map[string]string{"status": "RESOLVED", "resolution": "Rent of the user's office; contract on file."}, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	status, raw = env.call(t, http.MethodPost, "/api/v1/admin/aml/alerts/"+id+"/transition",
		map[string]string{"status": "ESCALATED"}, token)
	require.Equal(t, http.StatusConflict, status, string(raw))
	assert.Contains(t, string(raw), "aml_alert_transition_invalid")
	status, raw = env.call(t, http.MethodGet, "/api/v1/admin/aml/alerts/00000000-0000-0000-0000-000000000009", nil, token)
	require.Equal(t, http.StatusNotFound, status, string(raw))
}
