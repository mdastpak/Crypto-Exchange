package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDBName is the PostgreSQL database of this package.
const testDBName = "testdb_audit"

func count(t *testing.T, db *sql.DB, where string, args ...interface{}) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM audit_events WHERE "+where, args...).Scan(&n))
	return n
}

func TestRecord_WritesTheEvent(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	admin, currency := uuid.NewString(), uuid.NewString()
	require.NoError(t, Record(context.Background(), db, Event{
		ActorType: ActorAdmin, ActorID: admin, Action: ActionCurrencyUpdate,
		TargetType: TargetCurrency, TargetID: currency, Outcome: Success,
		Changes:   &Changes{Before: map[string]interface{}{"status": "pending"}, After: map[string]interface{}{"status": "approved"}},
		RequestID: "req-1", IP: "203.0.113.7", UserAgent: "test-agent",
	}))

	var hk, actorType, actorID, action, targetType, targetID, outcome, requestID, ip, userAgent, changes string
	var reason sql.NullString
	require.NoError(t, db.QueryRow(`SELECT hk, actor_type, actor_id, action, target_type, target_id, outcome, reason,
		request_id, host(ip), user_agent, changes::text FROM audit_events`).Scan(
		&hk, &actorType, &actorID, &action, &targetType, &targetID, &outcome, &reason, &requestID, &ip, &userAgent, &changes))
	id, err := uuid.Parse(hk)
	require.NoError(t, err)
	assert.Equal(t, uuid.Version(7), id.Version(), "the hk sorts the events by time")
	assert.Equal(t, []string{ActorAdmin, admin, ActionCurrencyUpdate, TargetCurrency, currency, Success, "req-1", "203.0.113.7", "test-agent"},
		[]string{actorType, actorID, action, targetType, targetID, outcome, requestID, ip, userAgent})
	assert.False(t, reason.Valid)
	var decoded Changes
	require.NoError(t, json.Unmarshal([]byte(changes), &decoded))
	assert.Equal(t, "approved", decoded.After["status"])
}

func TestRecord_RollsBackWithItsTransaction(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, Record(context.Background(), tx, Event{ActorType: ActorSystem, Action: "test.rolled_back", Outcome: Success}))
	require.NoError(t, tx.Rollback())
	assert.Zero(t, count(t, db, "action = 'test.rolled_back'"), "an event of a change that rolls back is not kept")
}

// TestAuditEvents_AreAppendOnly checks that the triggers refuse every way of
// changing or removing an event, also for the role that owns the table.
func TestAuditEvents_AreAppendOnly(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	require.NoError(t, Record(context.Background(), db, Event{ActorType: ActorSystem, Action: "test.kept", Outcome: Success}))

	for name, statement := range map[string]string{
		"update":   "UPDATE audit_events SET outcome = 'failure'",
		"delete":   "DELETE FROM audit_events",
		"truncate": "TRUNCATE audit_events",
	} {
		_, err := db.Exec(statement)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "audit_events is append-only", name)
	}
	assert.Equal(t, 1, count(t, db, "action = 'test.kept' AND outcome = 'success'"), "the event is unchanged")
}

func TestRecord_RefusesMalformedEvents(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	for name, e := range map[string]Event{
		"an action without a domain": {ActorType: ActorSystem, Action: "sign_in", Outcome: Success},
		"an action in upper case":    {ActorType: ActorSystem, Action: "Admin.SignIn", Outcome: Success},
		"an unknown outcome":         {ActorType: ActorSystem, Action: "test.x", Outcome: "maybe"},
		"an admin without an id":     {ActorType: ActorAdmin, Action: "test.x", Outcome: Success},
		"an anonymous actor with id": {ActorType: ActorAnonymous, ActorID: uuid.NewString(), Action: "test.x", Outcome: Success},
		"a target without an id":     {ActorType: ActorSystem, Action: "test.x", Outcome: Success, TargetType: TargetCurrency},
		"a reason with spaces":       {ActorType: ActorSystem, Action: "test.x", Outcome: Failure, Reason: "bad code"},
	} {
		assert.Error(t, Record(context.Background(), db, e), name)
	}
	assert.Zero(t, count(t, db, "TRUE"))
}

func TestRecord_CleansWhatTheClientChose(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	agent := "agent\x00" + strings.Repeat("é", 600)
	require.NoError(t, Record(context.Background(), db, Event{
		ActorType: ActorAnonymous, Action: "test.cleaned", Outcome: Denied, IP: "not an address", UserAgent: agent,
	}))
	var ip sql.NullString
	var userAgent string
	require.NoError(t, db.QueryRow("SELECT host(ip), user_agent FROM audit_events WHERE action = 'test.cleaned'").Scan(&ip, &userAgent))
	assert.False(t, ip.Valid, "an address that does not parse is not stored")
	assert.Len(t, []rune(userAgent), maxUserAgent, "the user agent is shortened to 512 characters")
	assert.True(t, strings.HasPrefix(userAgent, "agenté"), "the NUL is dropped")
}
