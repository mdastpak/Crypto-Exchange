package aml

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// Querier runs the queries of the package: a *sql.DB or a *sql.Tx.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Stored is a rule of aml_rules: its definition at its current version.
type Stored struct {
	ID      string     `json:"id" format:"uuid"`
	Version int        `json:"version"`
	Retired *time.Time `json:"retired_at,omitempty"`
	Rule
}

// Synced is what a sync changed.
type Synced struct {
	Added, Changed, Retired, Unchanged []string
}

// syncLock is the advisory lock that one sync at a time takes, over every
// instance.
const syncLock = "aml_rules_sync"

// Sync writes the rules of f into aml_rules, in one transaction: a new
// rule at version 1, a changed or returning one at its next version, and a
// rule that left the file retired. Each version is kept in
// aml_rule_history, and each change audited. A rule changed in SQL fails
// its seal, and the sync with it, until it is reviewed.
func Sync(ctx context.Context, db *sql.DB, f File) (Synced, error) {
	var out Synced
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("aml: sync the rules: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, syncLock); err != nil {
		return out, fmt.Errorf("aml: sync the rules: %w", err)
	}
	stored, err := loadAll(ctx, tx, true)
	if err != nil {
		return out, err
	}
	byCode := map[string]Stored{}
	for _, s := range stored {
		byCode[s.Code] = s
	}
	origin := audit.Event{ActorType: audit.ActorSystem, UserAgent: "aml rules sync"}
	for _, r := range f.Rules {
		def, err := r.definition()
		if err != nil {
			return out, err
		}
		s, ok := byCode[r.Code]
		delete(byCode, r.Code)
		switch {
		case !ok:
			if err := insertRule(ctx, tx, r, def); err != nil {
				return out, err
			}
			out.Added = append(out.Added, r.Code)
		default:
			old, err := s.definition()
			if err != nil {
				return out, err
			}
			if bytes.Equal(old, def) && s.Retired == nil {
				out.Unchanged = append(out.Unchanged, r.Code)
				continue
			}
			if err := updateRule(ctx, tx, s.ID, s.Version+1, r, def); err != nil {
				return out, err
			}
			out.Changed = append(out.Changed, r.Code)
		}
		e := audit.Of(origin, audit.ActionAMLRuleSync, audit.TargetAMLRule, r.Code)
		e.Changes = &audit.Changes{After: map[string]any{"enabled": r.Enabled, "action": string(r.Action), "type": string(r.Type)}}
		if ok {
			e.Changes.Before = map[string]any{"version": s.Version, "enabled": s.Enabled, "action": string(s.Action)}
		}
		if err := audit.Record(ctx, tx, e); err != nil {
			return out, err
		}
	}
	for _, s := range byCode {
		if s.Retired != nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE aml_rules SET retired_at = NOW() WHERE id = $1`, s.ID); err != nil {
			return out, fmt.Errorf("aml: retire %s: %w", s.Code, err)
		}
		if err := rowseal.Reseal(ctx, tx, rowseal.AMLRules, "id = $1", s.ID); err != nil {
			return out, err
		}
		e := audit.Of(origin, audit.ActionAMLRuleSync, audit.TargetAMLRule, s.Code)
		e.Reason = "retired"
		e.Changes = &audit.Changes{Before: map[string]any{"version": s.Version, "enabled": s.Enabled}}
		if err := audit.Record(ctx, tx, e); err != nil {
			return out, err
		}
		out.Retired = append(out.Retired, s.Code)
	}
	slices.Sort(out.Retired)
	return out, tx.Commit()
}

// columns are the values of the columns of aml_rules for r, after its id.
func columns(r Rule) ([]any, error) {
	config, err := r.configJSON()
	if err != nil {
		return nil, err
	}
	applies := make([]string, len(r.AppliesTo))
	for i, k := range r.AppliesTo {
		applies[i] = string(k)
	}
	slices.Sort(applies)
	return []any{r.Code, r.Name, r.Description, string(r.Type), config, r.ThresholdAmount.String(), r.ThresholdCount,
		r.WindowHours, string(r.Action), r.Severity, r.Enabled, "{" + strings.Join(applies, ",") + "}"}, nil
}

func insertRule(ctx context.Context, tx *sql.Tx, r Rule, def []byte) error {
	values, err := columns(r)
	if err != nil {
		return err
	}
	var id string
	err = tx.QueryRowContext(ctx, `INSERT INTO aml_rules (code, name, description, rule_type, config, threshold_amount,
			threshold_count, time_window_hours, action, severity, enabled, applies_to, rule_version)
		VALUES ($1, $2, NULLIF($3, ''), $4::aml_rule_type, $5::jsonb, $6::numeric, $7, $8, $9::aml_action, $10, $11, $12::text[], 1)
		RETURNING id`, values...).Scan(&id)
	if err != nil {
		return fmt.Errorf("aml: add %s: %w", r.Code, err)
	}
	return writeHistory(ctx, tx, id, 1, def)
}

func updateRule(ctx context.Context, tx *sql.Tx, id string, version int, r Rule, def []byte) error {
	values, err := columns(r)
	if err != nil {
		return err
	}
	values = append(values, version, id)
	_, err = tx.ExecContext(ctx, `UPDATE aml_rules SET code = $1, name = $2, description = NULLIF($3, ''),
			rule_type = $4::aml_rule_type, config = $5::jsonb, threshold_amount = $6::numeric, threshold_count = $7,
			time_window_hours = $8, action = $9::aml_action, severity = $10, enabled = $11, applies_to = $12::text[],
			rule_version = $13, retired_at = NULL
		WHERE id = $14`, values...)
	if err != nil {
		return fmt.Errorf("aml: change %s: %w", r.Code, err)
	}
	return writeHistory(ctx, tx, id, version, def)
}

func writeHistory(ctx context.Context, tx *sql.Tx, id string, version int, def []byte) error {
	if err := rowseal.Reseal(ctx, tx, rowseal.AMLRules, "id = $1", id); err != nil {
		return err
	}
	var hid string
	if err := tx.QueryRowContext(ctx, `INSERT INTO aml_rule_history (rule_id, rule_version, definition)
		VALUES ($1, $2, $3::jsonb) RETURNING id`, id, version, def).Scan(&hid); err != nil {
		return fmt.Errorf("aml: keep version %d of a rule: %w", version, err)
	}
	return rowseal.Reseal(ctx, tx, rowseal.AMLRuleHistory, "id = $1", hid)
}

// Rules returns the rules in force: enabled and not retired, each checked
// against its seal and the seal of its version. A rule changed in SQL
// fails, and with it every evaluation, until it is reviewed.
func Rules(ctx context.Context, q Querier) ([]Stored, error) {
	all, err := loadAll(ctx, q, false)
	if err != nil {
		return nil, err
	}
	in := all[:0]
	for _, s := range all {
		if s.Enabled && s.Retired == nil {
			in = append(in, s)
		}
	}
	return in, nil
}

// All returns every rule, retired ones included, for the administrators.
func All(ctx context.Context, q Querier) ([]Stored, error) { return loadAll(ctx, q, false) }

// loadAll reads every rule at its version. With lock, the rows are locked
// for the sync.
func loadAll(ctx context.Context, q Querier, lock bool) ([]Stored, error) {
	t, h := rowseal.AMLRules, rowseal.AMLRuleHistory
	query := `SELECT r.rule_version, r.retired_at, h.definition, ` + t.FieldsAs("r", "r_") + `, ` + h.FieldsAs("h", "h_") + `
		FROM aml_rules r JOIN aml_rule_history h ON h.rule_id = r.id AND h.rule_version = r.rule_version
		ORDER BY r.code`
	if lock {
		query += ` FOR UPDATE OF r`
	}
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("aml: read the rules: %w", err)
	}
	defer rows.Close()
	type read struct {
		version int
		retired sql.NullTime
		def     []byte
		r, h    *rowseal.Row
	}
	var reads []read
	for rows.Next() {
		x := read{r: t.NewRow(), h: h.NewRow()}
		if err := rows.Scan(append(append([]any{&x.version, &x.retired, &x.def}, x.r.Dest()...), x.h.Dest()...)...); err != nil {
			return nil, fmt.Errorf("aml: read the rules: %w", err)
		}
		reads = append(reads, x)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aml: read the rules: %w", err)
	}
	rows.Close()
	out := make([]Stored, 0, len(reads))
	for _, x := range reads {
		if err := rowseal.Require(ctx, t, x.r); err != nil {
			return nil, err
		}
		if err := rowseal.Require(ctx, h, x.h); err != nil {
			return nil, err
		}
		s := Stored{ID: x.r.Key, Version: x.version}
		if err := json.Unmarshal(x.def, &s.Rule); err != nil {
			return nil, fmt.Errorf("aml: rule %s: version %d: %w", x.r.Key, x.version, err)
		}
		if code := x.r.Values[t.Index("code")].String; code != s.Code {
			return nil, fmt.Errorf("aml: rule %s: its version is of %s", code, s.Code)
		}
		if v := x.r.Values[t.Index("enabled")].String; v != strconv.FormatBool(s.Enabled) {
			return nil, fmt.Errorf("aml: rule %s: enabled is %s, its version says %t", s.Code, v, s.Enabled)
		}
		if x.retired.Valid {
			at := x.retired.Time.UTC()
			s.Retired = &at
		}
		out = append(out, s)
	}
	return out, nil
}

// ErrNotFound is a rule or an alert that does not exist.
var ErrNotFound = errors.New("aml: not found")
