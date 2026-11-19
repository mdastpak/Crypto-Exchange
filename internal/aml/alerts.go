package aml

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// Status is the status of an alert: the aml_alert_status of the schema.
type Status string

// The statuses of an alert. RESOLVED and DISMISSED are final.
const (
	StatusOpen          Status = "OPEN"
	StatusInvestigating Status = "INVESTIGATING"
	StatusResolved      Status = "RESOLVED"
	StatusDismissed     Status = "DISMISSED"
	StatusEscalated     Status = "ESCALATED"
)

// transitions are the statuses that each status may move to.
var transitions = map[Status][]Status{
	StatusOpen:          {StatusInvestigating, StatusResolved, StatusDismissed, StatusEscalated},
	StatusInvestigating: {StatusResolved, StatusDismissed, StatusEscalated},
	StatusEscalated:     {StatusInvestigating, StatusResolved, StatusDismissed},
}

// Final reports whether s is final.
func (s Status) Final() bool { return s == StatusResolved || s == StatusDismissed }

// Valid reports whether s is a status.
func (s Status) Valid() bool {
	_, open := transitions[s]
	return open || s.Final()
}

// Alert is an alert of a rule.
type Alert struct {
	ID          string `json:"id" format:"uuid"`
	RuleID      string `json:"rule_id" format:"uuid"`
	RuleCode    string `json:"rule_code" example:"structuring_withdrawal"`
	RuleVersion int    `json:"rule_version"`
	UserHK      string `json:"user_hk" format:"uuid"`
	EventID     string `json:"event_id,omitempty" format:"uuid"`
	EventKind   string `json:"event_kind,omitempty" enums:"deposit,withdrawal,trade"`
	EventRef    string `json:"event_ref,omitempty"`
	Action      Action `json:"action" enums:"ALERT,REVIEW,BLOCK,FREEZE"`
	// Data is what the rule saw, by rule type.
	Data        map[string]any `json:"trigger_data"`
	RiskScore   string         `json:"risk_score" example:"75"`
	Status      Status         `json:"status" enums:"OPEN,INVESTIGATING,RESOLVED,DISMISSED,ESCALATED"`
	AssignedTo  string         `json:"assigned_to,omitempty" format:"uuid"`
	Resolution  string         `json:"resolution,omitempty"`
	ResolvedAt  *time.Time     `json:"resolved_at,omitempty"`
	ResolvedBy  string         `json:"resolved_by,omitempty" format:"uuid"`
	EscalatedAt *time.Time     `json:"escalated_at,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}

// Notice is an alert that the compliance team is told of: one that held,
// refused, or froze, and an escalation.
type Notice struct {
	AlertID   string
	RuleCode  string
	UserHK    string
	Action    Action
	Escalated bool
}

// Notifier tells the compliance team of alerts.
type Notifier interface {
	Notify(ctx context.Context, notices []Notice) error
}

// insertAlert records an alert of hit for the user, with the action that
// was taken, and its audit event.
func insertAlert(ctx context.Context, tx *sql.Tx, hit Hit, userHK, eventID string, action Action, dedupe string, origin audit.Event) (Notice, bool, error) {
	data := hit.Data
	if data == nil {
		data = map[string]any{}
	}
	data["rule_action"] = string(hit.Rule.Action)
	raw, err := json.Marshal(data)
	if err != nil {
		return Notice{}, false, err
	}
	var id string
	err = tx.QueryRowContext(ctx, `INSERT INTO aml_alerts (rule_id, rule_version, user_id, event_id, action, trigger_data,
			risk_score, dedupe_key)
		VALUES ($1, $2, $3, NULLIF($4, '')::uuid, $5::aml_action, $6::jsonb, $7::numeric, NULLIF($8, ''))
		ON CONFLICT (dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING
		RETURNING id`, hit.Rule.ID, hit.Rule.Version, userHK, eventID, string(action), raw, hit.Rule.RiskScore(), dedupe).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Notice{}, false, nil
	}
	if err != nil {
		return Notice{}, false, fmt.Errorf("aml: record an alert of %s: %w", hit.Rule.Code, err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.AMLAlerts, "id = $1", id); err != nil {
		return Notice{}, false, err
	}
	e := audit.Of(origin, audit.ActionAMLAlert, audit.TargetAMLAlert, id)
	e.Reason = hit.Rule.Code
	e.Changes = &audit.Changes{After: map[string]any{"user": userHK, "rule": hit.Rule.Code, "version": hit.Rule.Version,
		"action": string(action)}}
	if err := audit.Record(ctx, tx, e); err != nil {
		return Notice{}, false, err
	}
	return Notice{AlertID: id, RuleCode: hit.Rule.Code, UserHK: userHK, Action: action}, true, nil
}

// system is origin as the engine's own: the system acts, from the request
// of origin.
func system(origin audit.Event) audit.Event {
	origin.ActorType, origin.ActorID = audit.ActorSystem, ""
	return origin
}

// Record records the alerts of a decision that let the operation run, in
// tx, the transaction of the operation, which commits them with it. It
// returns what to notify once tx commits.
func Record(ctx context.Context, tx *sql.Tx, d Decision, origin audit.Event) ([]Notice, error) {
	if d.Action.Refuses() {
		return nil, fmt.Errorf("aml: a refusal is recorded by Enforce")
	}
	var notices []Notice
	for _, hit := range d.Hits {
		n, _, err := insertAlert(ctx, tx, hit, d.Event.UserHK, d.EventID, hit.Action, "", system(origin))
		if err != nil {
			return nil, err
		}
		if n.Action != ActionAlert {
			notices = append(notices, n)
		}
	}
	return notices, nil
}

// systemAdmin is who the restrictions of the engine are created by: no
// administrator.
const systemAdmin = "00000000-0000-0000-0000-000000000000"

// ErrRefused is an operation that a rule refused. The user is told no
// more: not the rule, nor that it was an AML rule.
var ErrRefused = errors.New("aml: the operation is refused")

// Enforce records a decision that refused its operation, after the
// transaction of the operation rolled back: the operation, as refused, and
// its alerts, and for a freeze, the freeze of the account, in one
// transaction. It notifies, and returns ErrRefused.
func (e *Engine) Enforce(ctx context.Context, db *sql.DB, d Decision, origin audit.Event) error {
	if !d.Action.Refuses() {
		return fmt.Errorf("aml: Enforce of a decision that refuses nothing")
	}
	notices, err := e.enforce(ctx, db, d, system(origin))
	if err != nil {
		return err
	}
	e.Notify(ctx, notices)
	return ErrRefused
}

// Refuse records a decision whose operation is refused though its action
// would only hold it for review: an operation that cannot wait for a
// person, such as an OTC trade at its quoted price (#552). It records the
// operation as refused with its alerts, freezes the account only for a
// freeze, notifies, and returns ErrRefused.
func (e *Engine) Refuse(ctx context.Context, db *sql.DB, d Decision, origin audit.Event) error {
	if d.Action == ActionNone || d.Action == ActionAlert {
		return fmt.Errorf("aml: Refuse of a decision that holds nothing")
	}
	notices, err := e.enforce(ctx, db, d, system(origin))
	if err != nil {
		return err
	}
	e.Notify(ctx, notices)
	return ErrRefused
}

func (e *Engine) enforce(ctx context.Context, db *sql.DB, d Decision, origin audit.Event) ([]Notice, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("aml: record a refusal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	caseRef := "aml:" + uuid.NewString()
	if d.Action == ActionFreeze {
		if _, err := restrictions.CreateIn(ctx, tx, restrictions.New{UserHK: d.Event.UserHK, Scope: restrictions.ScopeAccount,
			Reason: restrictions.ReasonAMLReview, CaseRef: caseRef, CreatedBy: systemAdmin}, origin); err != nil {
			return nil, err
		}
	}
	eventID, err := insertEvent(ctx, tx, d, true)
	if err != nil {
		return nil, err
	}
	var notices []Notice
	for _, hit := range d.Hits {
		if hit.Data == nil {
			hit.Data = map[string]any{}
		}
		if d.Action == ActionFreeze {
			hit.Data["case_ref"] = caseRef
		}
		n, _, err := insertAlert(ctx, tx, hit, d.Event.UserHK, eventID, hit.Action, "", origin)
		if err != nil {
			return nil, err
		}
		if n.Action != ActionAlert {
			notices = append(notices, n)
		}
	}
	return notices, tx.Commit()
}

// Notify tells the notifier of notices, if any, and logs a failure: the
// alerts are recorded whatever it does.
func (e *Engine) Notify(ctx context.Context, notices []Notice) {
	for _, n := range notices {
		log.Printf("AML: alert %s of %s for user %s: %s", n.AlertID, n.RuleCode, n.UserHK, n.Action)
	}
	if e.Notifier == nil || len(notices) == 0 {
		return
	}
	if err := e.Notifier.Notify(ctx, notices); err != nil {
		log.Printf("AML: failed to notify of %d alerts: %v", len(notices), err)
	}
}

// alertSelect reads an alert with its rule and operation.
const alertSelect = `SELECT r.code, e.kind, e.ref, a.risk_score::text, a.trigger_data, a.resolved_at, a.escalated_at, a.created_at, `

func scanAlert(ctx context.Context, row interface{ Scan(...any) error }) (Alert, error) {
	t := rowseal.AMLAlerts
	r := t.NewRow()
	var a Alert
	var kind, ref sql.NullString
	var resolved, escalated sql.NullTime
	var data []byte
	if err := row.Scan(append([]any{&a.RuleCode, &kind, &ref, &a.RiskScore, &data, &resolved, &escalated, &a.CreatedAt},
		r.Dest()...)...); err != nil {
		return a, err
	}
	if err := rowseal.Require(ctx, t, r); err != nil {
		return a, err
	}
	if err := json.Unmarshal(data, &a.Data); err != nil {
		return a, fmt.Errorf("aml: alert %s: trigger_data: %w", r.Key, err)
	}
	col := func(name string) string { return r.Values[t.Index(name)].String }
	a.ID, a.RuleID, a.UserHK, a.EventID = r.Key, col("rule_id"), col("user_id"), col("event_id")
	a.EventKind, a.EventRef = kind.String, ref.String
	_, _ = fmt.Sscan(col("rule_version"), &a.RuleVersion)
	a.Action, a.Status = Action(col("action")), Status(col("status"))
	a.AssignedTo, a.Resolution, a.ResolvedBy = col("assigned_to"), col("resolution"), col("resolved_by")
	if resolved.Valid {
		at := resolved.Time.UTC()
		a.ResolvedAt = &at
	}
	if escalated.Valid {
		at := escalated.Time.UTC()
		a.EscalatedAt = &at
	}
	a.CreatedAt = a.CreatedAt.UTC()
	return a, nil
}

const alertFrom = ` FROM aml_alerts a JOIN aml_rules r ON r.id = a.rule_id LEFT JOIN aml_events e ON e.id = a.event_id`

// Alerts lists the alerts of a status, or of every status for "", the
// newest first, each checked against its seal.
func Alerts(ctx context.Context, q Querier, status Status, userHK string, limit int) ([]Alert, error) {
	t := rowseal.AMLAlerts
	rows, err := q.QueryContext(ctx, alertSelect+t.Fields("a")+alertFrom+`
		WHERE ($1 = '' OR a.status::text = $1) AND ($2 = '' OR a.user_id::text = $2)
		ORDER BY a.created_at DESC, a.id LIMIT $3`, string(status), userHK, limit)
	if err != nil {
		return nil, fmt.Errorf("aml: list the alerts: %w", err)
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		a, err := scanAlert(ctx, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAlert returns an alert.
func GetAlert(ctx context.Context, q Querier, id string) (Alert, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Alert{}, ErrNotFound
	}
	a, err := scanAlert(ctx, q.QueryRowContext(ctx, alertSelect+rowseal.AMLAlerts.Fields("a")+alertFrom+` WHERE a.id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// ErrTransition is a move of an alert that its status does not allow.
type ErrTransition struct{ From, To Status }

func (e *ErrTransition) Error() string {
	return fmt.Sprintf("aml: an alert %s cannot become %s", e.From, e.To)
}

// ErrResolution is a closing of an alert without a resolution, or a
// resolution on another move.
var ErrResolution = errors.New("aml: a resolution is required to resolve or dismiss an alert, and only then")

// Transition moves an alert to status to, by the administrator adminHK:
// INVESTIGATING assigns it to them, and RESOLVED or DISMISSED close it
// with resolution, 1 to 1000 characters, which only they take.
func Transition(ctx context.Context, db *sql.DB, id string, to Status, adminHK, resolution string, origin audit.Event) (Alert, error) {
	if to.Final() != (resolution != "") || len(resolution) > 1000 {
		return Alert{}, ErrResolution
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Alert{}, fmt.Errorf("aml: change the alert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := uuid.Parse(id); err != nil {
		return Alert{}, ErrNotFound
	}
	a, err := scanAlert(ctx, tx.QueryRowContext(ctx, alertSelect+rowseal.AMLAlerts.Fields("a")+alertFrom+
		` WHERE a.id = $1 FOR UPDATE OF a`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	if !slices.Contains(transitions[a.Status], to) {
		return a, &ErrTransition{From: a.Status, To: to}
	}
	_, err = tx.ExecContext(ctx, `UPDATE aml_alerts SET status = $2::aml_alert_status,
			assigned_to = CASE WHEN $2 = 'INVESTIGATING' THEN $3::uuid ELSE assigned_to END,
			resolved_at = CASE WHEN $2 IN ('RESOLVED', 'DISMISSED') THEN NOW() END,
			resolved_by = CASE WHEN $2 IN ('RESOLVED', 'DISMISSED') THEN $3::uuid END,
			resolution = NULLIF($4, ''),
			escalated_at = CASE WHEN $2 = 'ESCALATED' THEN NOW() ELSE escalated_at END
		WHERE id = $1`, id, string(to), adminHK, resolution)
	if err != nil {
		return a, fmt.Errorf("aml: change the alert: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.AMLAlerts, "id = $1", id); err != nil {
		return a, err
	}
	e := audit.Of(origin, audit.ActionAMLAlertStatus, audit.TargetAMLAlert, id)
	e.Changes = &audit.Changes{Before: map[string]any{"status": string(a.Status)}, After: map[string]any{"status": string(to)}}
	if err := audit.Record(ctx, tx, e); err != nil {
		return a, err
	}
	out, err := GetAlert(ctx, tx, id)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// MaxList is the most alerts that a list returns.
const MaxList = 100

// Desk is what the administrators use: the rules and the alerts, in db.
type Desk struct{ DB *sql.DB }

// Rules returns every rule, retired ones included.
func (d Desk) Rules(ctx context.Context) ([]Stored, error) { return All(ctx, d.DB) }

// Alerts lists the alerts.
func (d Desk) Alerts(ctx context.Context, status Status, userHK string, limit int) ([]Alert, error) {
	return Alerts(ctx, d.DB, status, userHK, limit)
}

// Alert returns an alert.
func (d Desk) Alert(ctx context.Context, id string) (Alert, error) { return GetAlert(ctx, d.DB, id) }

// Transition moves an alert.
func (d Desk) Transition(ctx context.Context, id string, to Status, adminHK, resolution string, origin audit.Event) (Alert, error) {
	return Transition(ctx, d.DB, id, to, adminHK, resolution, origin)
}
