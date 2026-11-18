package aml

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// The batch evaluation (#73). The rules run on each operation, in its
// transaction; the batch evaluates again, at an interval, the velocity and
// structuring rules over the windows of the users who had operations in
// them, for the patterns that no single operation completed, such as
// operations of a kind whose own path does not evaluate yet. It raises an
// alert once per rule, user, and window, and freezes the account for a
// FREEZE rule; it holds and refuses nothing, as the operations already
// ran. Every instance may run it: an alert of a window is unique.

// BatchResult is what a batch did.
type BatchResult struct {
	Alerts    int
	Escalated int
}

// noEvent stands for no operation evaluated, in the queries that leave
// one out.
const noEvent = "00000000-0000-0000-0000-000000000000"

// Batch evaluates the batch rules at the engine's now, and escalates the
// alerts that stayed open for escalateAfter.
func (e *Engine) Batch(ctx context.Context, db *sql.DB, escalateAfter time.Duration) (BatchResult, error) {
	var out BatchResult
	at := e.now()
	rules, err := Rules(ctx, db)
	if err != nil {
		return out, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	origin := audit.Event{ActorType: audit.ActorSystem, UserAgent: "aml batch"}
	var notices []Notice
	for _, r := range rules {
		if !batched(r) {
			continue
		}
		users, err := activeUsers(ctx, db, r, at)
		if err != nil {
			return out, err
		}
		for _, user := range users {
			n, err := e.batchUser(ctx, db, r, user, at, origin)
			if err != nil {
				return out, err
			}
			if n != nil {
				out.Alerts++
				notices = append(notices, *n)
			}
		}
	}
	escalated, err := escalate(ctx, db, at, escalateAfter, origin)
	if err != nil {
		return out, err
	}
	out.Escalated = len(escalated)
	notices = append(notices, escalated...)
	e.Notify(ctx, notices)
	return out, nil
}

// batched reports whether the batch evaluates r: the velocity rules over a
// window and the structuring rules.
func batched(r Stored) bool {
	return r.Type == TypeStructuring ||
		(r.Type == TypeVelocity && r.Config.Metric != MetricSingle)
}

// activeUsers are the users with operations of the rule's window.
func activeUsers(ctx context.Context, db *sql.DB, r Stored, at time.Time) ([]string, error) {
	kinds := make([]string, 0, len(r.AppliesTo))
	for _, k := range r.AppliesTo {
		kinds = append(kinds, string(k))
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT user_hk FROM aml_events
		WHERE NOT refused AND occurred_at > $1 AND occurred_at <= $2 AND kind = ANY($3::text[])`,
		at.Add(-time.Duration(r.WindowHours)*time.Hour), at, "{"+joinKinds(kinds)+"}")
	if err != nil {
		return nil, fmt.Errorf("aml: read the users of %s: %w", r.Code, err)
	}
	defer rows.Close()
	var users []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func joinKinds(kinds []string) string {
	s := ""
	for i, k := range kinds {
		if i > 0 {
			s += ","
		}
		s += k
	}
	return s
}

// batchUser evaluates r over the user's window, in a transaction of its
// own, and raises its alert: a Notice of it when it is new.
func (e *Engine) batchUser(ctx context.Context, db *sql.DB, r Stored, user string, at time.Time, origin audit.Event) (*Notice, error) {
	h, err := e.history(ctx, db, user, noEvent, at, time.Duration(r.WindowHours)*time.Hour)
	if err != nil {
		return nil, err
	}
	window := within(h.events, r, at)
	var data map[string]any
	switch {
	case r.Type == TypeStructuring:
		ceiling := *r.Config.Ceiling
		n := 0
		for _, p := range window {
			if p.value != nil && p.value.GreaterThanOrEqual(r.ThresholdAmount) && p.value.LessThan(ceiling) {
				n++
			}
		}
		if n >= r.ThresholdCount {
			data = map[string]any{"count": n, "band_usdt": []string{r.ThresholdAmount.String(), ceiling.String()}}
		}
	case r.Config.Metric == MetricCycles:
		if n := cycles(inWindow(h.events, r, at)); n >= r.ThresholdCount {
			data = map[string]any{"metric": MetricCycles, "cycles": n}
		}
	default:
		data, _ = velocity(r, past{}, window)
	}
	if data == nil {
		return nil, nil
	}
	data["batch"] = true
	window0 := at.Truncate(time.Duration(r.WindowHours) * time.Hour)
	dedupe := fmt.Sprintf("%s:%d:%s:%d", r.Code, r.Version, user, window0.Unix())

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("aml: raise an alert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// An alert of the rule for the user within its window, from an
	// operation, is the same pattern.
	var recent bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM aml_alerts WHERE rule_id = $1 AND user_id = $2 AND created_at > $3)`,
		r.ID, user, at.Add(-time.Duration(r.WindowHours)*time.Hour)).Scan(&recent); err != nil {
		return nil, fmt.Errorf("aml: raise an alert: %w", err)
	}
	if recent {
		return nil, nil
	}
	action := ActionAlert
	if r.Action == ActionFreeze {
		action = ActionFreeze
		if _, err := restrictions.CreateIn(ctx, tx, restrictions.New{UserHK: user, Scope: restrictions.ScopeAccount,
			Reason: restrictions.ReasonAMLReview, CaseRef: "aml:" + dedupe[:min(len(dedupe), 60)], CreatedBy: systemAdmin}, origin); err != nil {
			return nil, err
		}
	}
	n, created, err := insertAlert(ctx, tx, Hit{Rule: r, Action: r.Action, Data: data}, user, "", action, dedupe, origin)
	if err != nil || !created {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("aml: raise an alert: %w", err)
	}
	return &n, nil
}

// escalate escalates the alerts open since before at-after, each checked
// against its seal; another instance skips the ones it holds.
func escalate(ctx context.Context, db *sql.DB, at time.Time, after time.Duration, origin audit.Event) ([]Notice, error) {
	if after <= 0 {
		return nil, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("aml: escalate the alerts: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, alertSelect+rowseal.AMLAlerts.Fields("a")+alertFrom+`
		WHERE a.status = 'OPEN' AND a.created_at < $1 ORDER BY a.created_at LIMIT 100 FOR UPDATE OF a SKIP LOCKED`, at.Add(-after))
	if err != nil {
		return nil, fmt.Errorf("aml: escalate the alerts: %w", err)
	}
	var due []Alert
	for rows.Next() {
		a, err := scanAlert(ctx, rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		due = append(due, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var notices []Notice
	for _, a := range due {
		if _, err := tx.ExecContext(ctx, `UPDATE aml_alerts SET status = 'ESCALATED', escalated_at = $2 WHERE id = $1`, a.ID, at); err != nil {
			return nil, fmt.Errorf("aml: escalate %s: %w", a.ID, err)
		}
		if err := rowseal.Reseal(ctx, tx, rowseal.AMLAlerts, "id = $1", a.ID); err != nil {
			return nil, err
		}
		e := audit.Of(origin, audit.ActionAMLAlertStatus, audit.TargetAMLAlert, a.ID)
		e.Reason = "escalation"
		e.Changes = &audit.Changes{Before: map[string]any{"status": string(a.Status)}, After: map[string]any{"status": string(StatusEscalated)}}
		if err := audit.Record(ctx, tx, e); err != nil {
			return nil, err
		}
		notices = append(notices, Notice{AlertID: a.ID, RuleCode: a.RuleCode, UserHK: a.UserHK, Action: a.Action, Escalated: true})
	}
	return notices, tx.Commit()
}
