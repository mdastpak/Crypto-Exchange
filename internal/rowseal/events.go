package rowseal

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
)

// EventMatch names the audit event of a request that a second
// administrator approves (#337), such as a re-seal or the lift of a
// lockdown. The backend records the event in the transaction that writes
// the request, so the event has the time of the request.
type EventMatch struct {
	Action, TargetType, TargetID string
	// ActorID is the administrator who asked.
	ActorID string
	// At is the time of the request.
	At time.Time
	// After are values, as text, that the changes.after of the event has.
	After map[string]string
}

// maxMatchingEvents bounds the events that EventRecorded checks. The
// backend writes one; more were inserted in SQL.
const maxMatchingEvents = 10

// EventRecorded reports whether the audit log has an event that matches m,
// under a seal that matches. The columns of a request are as trustworthy as
// the row that holds them; the sealed event is not: anyone who writes to
// the database can insert an event, but only the backend can seal one. So
// a request written in SQL in the name of another administrator, to let one
// approve alone, has no such event. An event that matches m but fails its
// seal check is logged.
func (s *Sealer) EventRecorded(ctx context.Context, q Querier, m EventMatch) (bool, error) {
	fields := AuditEvents.ColumnFields("e")
	query := `SELECT e.hk::text, e.row_seal, e.seal_key_version::bigint, ` + strings.Join(fields, ", ") + `
		FROM audit_events e
		WHERE e.target_type = $1 AND e.target_id = $2 AND e.action = $3 AND e.actor_id = $4::uuid AND e.occurred_at = $5`
	args := []any{m.TargetType, m.TargetID, m.Action, m.ActorID, m.At}
	keys := make([]string, 0, len(m.After))
	for k := range m.After {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		args = append(args, k, m.After[k])
		query += fmt.Sprintf(" AND e.changes->'after'->>$%d = $%d", len(args)-1, len(args))
	}
	query += fmt.Sprintf(" LIMIT %d", maxMatchingEvents)

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("read the audit event of %s: %w", m.Action, err)
	}
	type event struct {
		hk         string
		seal       []byte
		keyVersion sql.NullInt64
		values     []sql.NullString
	}
	var events []event
	for rows.Next() {
		e := event{values: make([]sql.NullString, len(fields))}
		dest := []any{&e.hk, &e.seal, &e.keyVersion}
		for i := range e.values {
			dest = append(dest, &e.values[i])
		}
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return false, fmt.Errorf("read the audit event of %s: %w", m.Action, err)
		}
		events = append(events, e)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read the audit event of %s: %w", m.Action, err)
	}
	for _, e := range events {
		verdict, err := s.CheckEvent(ctx, e.hk, e.values, e.seal, e.keyVersion)
		if err != nil {
			return false, err
		}
		if verdict == "" {
			return true, nil
		}
		s.logf("SECURITY: audit event %s, %s of %s %s, fails its seal check (%s)", e.hk, m.Action, m.TargetType, m.TargetID, verdict)
	}
	return false, nil
}

// EventRecorded is Sealer.EventRecorded with the sealer in use. Without
// data keys nothing is sealed, the events neither, so it reports true.
func EventRecorded(ctx context.Context, q Querier, m EventMatch) (bool, error) {
	s := Active()
	if s == nil {
		return true, nil
	}
	return s.EventRecorded(ctx, q, m)
}
