package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/pagination"

	"github.com/google/uuid"
)

// Entry is an audit event as the admin query returns it.
type Entry struct {
	HK         string `json:"hk" format:"uuid"`
	OccurredAt string `json:"occurred_at" example:"2026-11-07T10:00:00Z"`
	ActorType  string `json:"actor_type" enums:"user,admin,anonymous,system"`
	// ActorID is the hk of the user or administrator.
	ActorID    string `json:"actor_id,omitempty" format:"uuid"`
	Action     string `json:"action" example:"currency.update"`
	TargetType string `json:"target_type,omitempty" example:"currency"`
	TargetID   string `json:"target_id,omitempty"`
	Outcome    string `json:"outcome" enums:"success,failure,denied"`
	// Reason is the code of a failure or a denial.
	Reason    string   `json:"reason,omitempty" example:"authenticator_code_invalid"`
	RequestID string   `json:"request_id,omitempty"`
	IP        string   `json:"ip,omitempty" example:"203.0.113.7"`
	UserAgent string   `json:"user_agent,omitempty"`
	Changes   *Changes `json:"changes,omitempty"`
	// Integrity tells whether the event matches its seal: verified;
	// mismatch, changed without the backend; unsealed, written without a
	// seal; or unchecked, since this instance cannot check it now.
	Integrity string `json:"integrity" enums:"verified,mismatch,unsealed,unchecked"`
}

// EntryPage is a page of entries. It describes pagination.Page[Entry] in
// the OpenAPI document, which swag cannot read from the generic type.
type EntryPage struct {
	Items []Entry `json:"items"`
	// NextCursor asks for the next page. It is left out on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// Filter selects events. An empty field selects any value.
type Filter struct {
	ActorID    string
	Action     string
	TargetType string
	TargetID   string
	Outcome    string
	// Since and Until bound occurred_at, Since included and Until
	// excluded; a zero time does not bound it.
	Since, Until time.Time
}

// position is the cursor of the admin query: the hk of the last event of a
// page. It is a UUID v7, public and in the order of time.
type position struct {
	HK string `json:"hk"`
}

// List returns a page of the events that f selects, newest first. The
// query names only the filters that are set, so that it can use the index
// of each.
func List(ctx context.Context, db *sql.DB, f Filter, page pagination.Request) (pagination.Page[Entry], error) {
	var where []string
	var args []interface{}
	add := func(condition string, arg interface{}) {
		args = append(args, arg)
		where = append(where, fmt.Sprintf(condition, len(args)))
	}
	if f.ActorID != "" {
		add("actor_id = $%d", f.ActorID)
	}
	if f.Action != "" {
		add("action = $%d", f.Action)
	}
	if f.TargetType != "" {
		add("target_type = $%d", f.TargetType)
	}
	if f.TargetID != "" {
		add("target_id = $%d", f.TargetID)
	}
	if f.Outcome != "" {
		add("outcome = $%d", f.Outcome)
	}
	if !f.Since.IsZero() {
		add("occurred_at >= $%d", f.Since)
	}
	if !f.Until.IsZero() {
		add("occurred_at < $%d", f.Until)
	}
	var after position
	more, err := page.After(&after)
	if err != nil {
		return pagination.Page[Entry]{}, err
	}
	if more {
		if _, err := uuid.Parse(after.HK); err != nil {
			return pagination.Page[Entry]{}, pagination.ErrInvalidCursor
		}
		add("hk < $%d", after.HK)
	}

	// With a sealer, the columns are also read in the form that their seal
	// covers.
	sealer := currentSealer()
	var sealed []string
	if sealer != nil {
		sealed = sealer.EventFields("")
	}
	query := `SELECT hk, occurred_at, actor_type, COALESCE(actor_id::text, ''), action,
		COALESCE(target_type, ''), COALESCE(target_id, ''), outcome, COALESCE(reason, ''),
		COALESCE(request_id, ''), COALESCE(host(ip), ''), COALESCE(user_agent, ''), changes::text,
		row_seal, seal_key_version::bigint`
	for _, field := range sealed {
		query += ", " + field
	}
	query += " FROM audit_events"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, page.Limit+1)
	query += fmt.Sprintf(" ORDER BY hk DESC LIMIT $%d", len(args)) //nolint:gosec // G202: the query joins constant fragments and placeholder numbers only; every value is an argument.

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return pagination.Page[Entry]{}, err
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		var e Entry
		var occurred time.Time
		var changes sql.NullString
		var seal []byte
		var keyVersion sql.NullInt64
		values := make([]sql.NullString, len(sealed))
		dest := []interface{}{&e.HK, &occurred, &e.ActorType, &e.ActorID, &e.Action, &e.TargetType, &e.TargetID,
			&e.Outcome, &e.Reason, &e.RequestID, &e.IP, &e.UserAgent, &changes, &seal, &keyVersion}
		for i := range values {
			dest = append(dest, &values[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return pagination.Page[Entry]{}, err
		}
		e.OccurredAt = occurred.UTC().Format(time.RFC3339Nano)
		e.Integrity = integrity(ctx, sealer, e.HK, values, seal, keyVersion)
		if changes.Valid {
			e.Changes = &Changes{}
			if err := json.Unmarshal([]byte(changes.String), e.Changes); err != nil {
				return pagination.Page[Entry]{}, fmt.Errorf("audit event %s: %w", e.HK, err)
			}
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return pagination.Page[Entry]{}, err
	}
	return pagination.NewPage(entries, page, func(e Entry) interface{} { return position{HK: e.HK} })
}
