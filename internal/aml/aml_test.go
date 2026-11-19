package aml

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/price"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"
)

// prices is a price engine with one live price per market.
type prices map[string]string

func (p prices) Get(_ context.Context, market string) (price.Price, error) {
	mid, ok := p[market]
	if !ok {
		return price.Price{}, price.ErrNoPrice
	}
	return price.Price{Market: market, Status: price.StatusLive, Mid: shared.MustNewDecimalFromString(mid)}, nil
}

// notifier keeps what it is told.
type notifier struct{ notices []Notice }

func (n *notifier) Notify(_ context.Context, notices []Notice) error {
	n.notices = append(n.notices, notices...)
	return nil
}

type fixture struct {
	t      *testing.T
	db     *sql.DB
	engine *Engine
	notes  *notifier
	user   string
	admin  string
	btc    string
}

func newFixture(t *testing.T, yaml string) fixture {
	t.Helper()
	db := pgtest.Migrated(t, "testdb_aml")
	sealtest.Use(t, db, rowseal.ModeEnforce)
	f := fixture{t: t, db: db, notes: &notifier{}}
	f.engine = &Engine{Prices: prices{"BTC/USDT": "50000"}, Notifier: f.notes}
	f.user = f.newUser("alice@example.com")
	f.admin = f.newUser("compliance@example.com")
	require.NoError(t, db.QueryRow(`INSERT INTO currencies (status, info)
		VALUES ('approved', jsonb_build_object('symbol', 'BTC', 'en_name', 'BTC', 'fa_name', 'BTC',
			'if_fiat', false, 'is_token', false, 'contract', '')) RETURNING hk::text`).Scan(&f.btc))
	if yaml != "" {
		file, err := Parse([]byte(yaml))
		require.NoError(t, err)
		_, err = Sync(context.Background(), db, file)
		require.NoError(t, err)
	}
	return f
}

func (f fixture) newUser(email string) string {
	f.t.Helper()
	hk := usertest.Insert(f.t, f.db, usertest.User{Email: email})
	require.NoError(f.t, rowseal.Reseal(context.Background(), f.db, rowseal.Users, "hk = $1", hk))
	return hk
}

func (f fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.db.QueryRow(query, args...).Scan(&n))
	return n
}

// withdraw evaluates a withdrawal of btc in its own transaction, which it
// commits with its alerts, or rolls back and enforces for a refusal.
func (f fixture) withdraw(ref, btc string, ev Event) (Decision, error) {
	f.t.Helper()
	ctx := context.Background()
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(f.t, err)
	defer func() { _ = tx.Rollback() }()
	ev.Kind, ev.Ref, ev.UserHK, ev.CurrencyHK, ev.Symbol = KindWithdrawal, ref, f.user, f.btc, "BTC"
	ev.Amount = decimal.RequireFromString(btc)
	d, err := f.engine.Evaluate(ctx, tx, ev)
	if err != nil {
		return d, err
	}
	if d.Action.Refuses() {
		require.NoError(f.t, tx.Rollback())
		return d, f.engine.Enforce(ctx, f.db, d, audit.Event{IP: "203.0.113.9"})
	}
	notices, err := Record(ctx, tx, d, audit.Event{IP: "203.0.113.9"})
	require.NoError(f.t, err)
	require.NoError(f.t, tx.Commit())
	f.engine.Notify(ctx, notices)
	return d, nil
}

const engineYAML = `
engine:
  evaluation_interval_seconds: 60
  alert_escalation_hours: 24
`

const reportRule = `
  - id: "large"
    name: "Large"
    rule_type: "VELOCITY"
    enabled: true
    applies_to: ["withdrawal"]
    config:
      metric: "single_amount"
    threshold_amount: "10000"
    time_window_hours: 1
    action: "ALERT"
    severity: "MEDIUM"
`

const structuringRule = `
  - id: "structuring"
    name: "Structuring"
    rule_type: "STRUCTURING"
    enabled: true
    applies_to: ["withdrawal"]
    config:
      ceiling: "10000"
    threshold_amount: "9000"
    threshold_count: 3
    time_window_hours: 24
    action: "BLOCK"
    severity: "CRITICAL"
`

const countryRule = `
  - id: "sanctioned_ip"
    name: "Sanctioned IP"
    rule_type: "HIGH_RISK_COUNTRY"
    enabled: true
    applies_to: ["withdrawal"]
    config:
      countries: ["KP"]
      check_fields: ["ip_country"]
    time_window_hours: 1
    action: "FREEZE"
    severity: "CRITICAL"
`

func rules(rs ...string) string { return "aml_rules:" + strings.Join(rs, "") + engineYAML }

// The sync keeps every version of a rule, retires a rule that left the
// file, and brings it back at its next version (#73).
func TestSync_VersionsAndRetires(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, rules(reportRule, structuringRule))
	assert.Equal(t, 2, f.count(`SELECT COUNT(*) FROM aml_rule_history`))

	again, err := Parse([]byte(rules(reportRule, structuringRule)))
	require.NoError(t, err)
	out, err := Sync(ctx, f.db, again)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"large", "structuring"}, out.Unchanged)

	changed, err := Parse([]byte(rules(strings.Replace(reportRule, `"10000"`, `"20000"`, 1))))
	require.NoError(t, err)
	out, err = Sync(ctx, f.db, changed)
	require.NoError(t, err)
	assert.Equal(t, []string{"large"}, out.Changed)
	assert.Equal(t, []string{"structuring"}, out.Retired)

	all, err := All(ctx, f.db)
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, 2, all[0].Version)
	assert.Equal(t, "20000", all[0].ThresholdAmount.String())
	require.NotNil(t, all[1].Retired)
	in, err := Rules(ctx, f.db)
	require.NoError(t, err)
	require.Len(t, in, 1, "a retired rule is not in force")

	out, err = Sync(ctx, f.db, again)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"large", "structuring"}, out.Changed)
	all, err = All(ctx, f.db)
	require.NoError(t, err)
	assert.Equal(t, 3, all[0].Version)
	assert.Equal(t, 2, all[1].Version)
	assert.Nil(t, all[1].Retired)
	assert.Equal(t, 5, f.count(`SELECT COUNT(*) FROM aml_rule_history`))
	assert.Equal(t, 6, f.count(`SELECT COUNT(*) FROM audit_events WHERE action = 'aml.rule_sync'`))
}

// A rule loosened in SQL is refused with its finding, and with it every
// evaluation, until it is reviewed: the operation is refused, not let
// through (#73).
func TestRules_ChangedInSQL_Refused(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, rules(reportRule))
	_, err := f.db.Exec(`UPDATE aml_rules SET threshold_amount = 1000000000, enabled = false`)
	require.NoError(t, err)

	_, err = Rules(ctx, f.db)
	require.ErrorIs(t, err, rowseal.ErrUnverified)
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM integrity_findings WHERE table_name = 'aml_rules'`))

	_, err = f.withdraw("w1", "1", Event{})
	require.ErrorIs(t, err, ErrUnavailable)
	assert.Equal(t, 0, f.count(`SELECT COUNT(*) FROM aml_events`), "nothing of the refused evaluation is kept")

	file, err := Parse([]byte(rules(reportRule)))
	require.NoError(t, err)
	_, err = Sync(ctx, f.db, file)
	assert.ErrorIs(t, err, rowseal.ErrUnverified, "the sync does not paper over it")
}

// A withdrawal that a rule reports runs, with its alert, at the version of
// the rule, valued at the price of the engine (#73).
func TestEvaluate_RecordsTheOperationAndItsAlerts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, rules(reportRule, structuringRule))

	d, err := f.withdraw("w1", "0.3", Event{IPCountry: "IR"})
	require.NoError(t, err)
	assert.Equal(t, ActionAlert, d.Action)
	require.NotNil(t, d.Value)
	assert.Equal(t, "15000", d.Value.String())

	list, err := Alerts(ctx, f.db, StatusOpen, f.user, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
	a := list[0]
	assert.Equal(t, "large", a.RuleCode)
	assert.Equal(t, 1, a.RuleVersion)
	assert.Equal(t, ActionAlert, a.Action)
	assert.Equal(t, "withdrawal", a.EventKind)
	assert.Equal(t, "w1", a.EventRef)
	assert.Equal(t, "15000", a.Data["value_usdt"])
	assert.Empty(t, f.notes.notices, "an alert alone is not sent")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM audit_events WHERE action = 'aml.alert' AND actor_type = 'system'`))

	// Two more just below the reporting threshold complete structuring:
	// the third is refused, and recorded as refused, apart.
	_, err = f.withdraw("w2", "0.19", Event{})
	require.NoError(t, err)
	_, err = f.withdraw("w3", "0.185", Event{})
	require.NoError(t, err)
	d, err = f.withdraw("w4", "0.199", Event{})
	require.ErrorIs(t, err, ErrRefused)
	assert.Equal(t, ActionBlock, d.Action)
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM aml_events WHERE refused`))
	require.Len(t, f.notes.notices, 1)
	assert.Equal(t, "structuring", f.notes.notices[0].RuleCode)
	assert.Equal(t, 0, f.count(`SELECT COUNT(*) FROM user_restrictions`), "a block freezes nothing")

	// The refused one is not counted: one more in the band is the third
	// again, and refused again.
	_, err = f.withdraw("w5", "0.19", Event{})
	require.ErrorIs(t, err, ErrRefused)
}

// A FREEZE refuses the operation and freezes the account, with the alert,
// in one transaction (#73).
func TestEnforce_Freeze(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, rules(countryRule))

	d, err := f.withdraw("w1", "0.01", Event{IPCountry: "KP"})
	require.ErrorIs(t, err, ErrRefused)
	assert.Equal(t, ActionFreeze, d.Action)

	var scope, reason, caseRef, createdBy string
	require.NoError(t, f.db.QueryRow(`SELECT scope, reason, case_ref, created_by::text FROM user_restrictions WHERE user_hk = $1`, f.user).
		Scan(&scope, &reason, &caseRef, &createdBy))
	assert.Equal(t, "account", scope)
	assert.Equal(t, "aml_review", reason)
	assert.True(t, strings.HasPrefix(caseRef, "aml:"))
	assert.Equal(t, systemAdmin, createdBy)

	list, err := Alerts(ctx, f.db, "", f.user, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, ActionFreeze, list[0].Action)
	assert.Equal(t, caseRef, list[0].Data["case_ref"])
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM audit_events WHERE action = 'user.restriction_add' AND actor_type = 'system'`))
}

// An operation lowered or deleted in SQL would let structuring pass under
// its threshold: the next evaluation of the user is refused (#73).
func TestEvaluate_OperationChangedInSQL_Refused(t *testing.T) {
	f := newFixture(t, rules(structuringRule))
	_, err := f.withdraw("w1", "0.19", Event{})
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE aml_events SET value_usdt = 1`)
	require.NoError(t, err)

	_, err = f.withdraw("w2", "0.19", Event{})
	require.ErrorIs(t, err, ErrUnavailable)
	assert.ErrorIs(t, err, rowseal.ErrUnverified)
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM integrity_findings WHERE table_name = 'aml_events'`))
}

// The batch raises the alert of a window once, and escalates an alert left
// open (#73).
func TestBatch_RaisesOnceAndEscalates(t *testing.T) {
	ctx := context.Background()
	deposits := `
  - id: "many_deposits"
    name: "Many Deposits"
    rule_type: "VELOCITY"
    enabled: true
    applies_to: ["deposit"]
    config:
      metric: "count"
    threshold_count: 2
    time_window_hours: 24
    action: "REVIEW"
    severity: "HIGH"
`
	f := newFixture(t, rules(deposits))
	now := time.Now().UTC()
	// Deposits of a path that evaluates nothing yet.
	for _, ref := range []string{"d1", "d2"} {
		v := decimal.NewFromInt(100)
		_, err := insertEvent(ctx, f.db, Decision{Event: Event{Kind: KindDeposit, Ref: ref, UserHK: f.user, CurrencyHK: f.btc,
			Amount: decimal.NewFromInt(1)}, At: now.Add(-time.Hour), Value: &v}, false)
		require.NoError(t, err)
	}
	f.engine.Now = func() time.Time { return now }
	out, err := f.engine.Batch(ctx, f.db, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, out.Alerts)
	out, err = f.engine.Batch(ctx, f.db, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 0, out.Alerts, "once per window")

	list, err := Alerts(ctx, f.db, "", f.user, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, ActionAlert, list[0].Action, "the batch holds nothing: the deposits ran")
	assert.Equal(t, true, list[0].Data["batch"])

	f.engine.Now = func() time.Time { return now.Add(25 * time.Hour) }
	out, err = f.engine.Batch(ctx, f.db, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, out.Escalated)
	a, err := GetAlert(ctx, f.db, list[0].ID)
	require.NoError(t, err)
	assert.Equal(t, StatusEscalated, a.Status)
	require.NotEmpty(t, f.notes.notices)
	assert.True(t, f.notes.notices[len(f.notes.notices)-1].Escalated)
}

// An alert moves through its lifecycle; a closed one is final, and one
// closed in SQL is detected (#73).
func TestTransition_Lifecycle(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, rules(reportRule))
	_, err := f.withdraw("w1", "1", Event{})
	require.NoError(t, err)
	list, err := Alerts(ctx, f.db, "", f.user, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
	id := list[0].ID
	origin := audit.Event{ActorType: audit.ActorAdmin, ActorID: f.admin}

	_, err = Transition(ctx, f.db, id, StatusResolved, f.admin, "", origin)
	require.ErrorIs(t, err, ErrResolution)
	_, err = Transition(ctx, f.db, id, StatusInvestigating, f.admin, "why", origin)
	require.ErrorIs(t, err, ErrResolution)

	a, err := Transition(ctx, f.db, id, StatusInvestigating, f.admin, "", origin)
	require.NoError(t, err)
	assert.Equal(t, StatusInvestigating, a.Status)
	assert.Equal(t, f.admin, a.AssignedTo)

	a, err = Transition(ctx, f.db, id, StatusResolved, f.admin, "Salary; documents on file.", origin)
	require.NoError(t, err)
	assert.Equal(t, StatusResolved, a.Status)
	assert.Equal(t, f.admin, a.ResolvedBy)
	assert.NotNil(t, a.ResolvedAt)

	_, err = Transition(ctx, f.db, id, StatusEscalated, f.admin, "", origin)
	var bad *ErrTransition
	require.True(t, errors.As(err, &bad))
	assert.Equal(t, StatusResolved, bad.From)

	_, err = f.db.Exec(`UPDATE aml_alerts SET resolution = 'nothing to see' WHERE id = $1`, id)
	require.NoError(t, err)
	_, err = GetAlert(ctx, f.db, id)
	require.ErrorIs(t, err, rowseal.ErrUnverified)
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM integrity_findings WHERE table_name = 'aml_alerts'`))

	_, err = GetAlert(ctx, f.db, "not-a-uuid")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, 2, f.count(`SELECT COUNT(*) FROM audit_events WHERE action = 'aml.alert_status'`))
}
