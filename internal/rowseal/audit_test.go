package rowseal_test

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/pagination"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sealEvents makes the audit events of the test sealed, as the server does
// once its data keys are loaded.
func (e *env) sealEvents(t *testing.T) *rowseal.Sealer {
	t.Helper()
	s := e.sealer(rowseal.ModeEnforce)
	t.Cleanup(audit.UseSealer(s))
	return s
}

// integrityOf lists the one event of a test target, and returns its
// integrity.
func integrityOf(t *testing.T, db *sql.DB, target string) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/audit", nil)
	page, errs := pagination.Parse(c)
	require.Nil(t, errs)
	list, err := audit.List(context.Background(), db, audit.Filter{TargetType: "test", TargetID: target}, page)
	require.NoError(t, err)
	require.Len(t, list.Items, 1, target)
	return list.Items[0].Integrity
}

// tamper changes audit_events as someone with direct database access would:
// it turns the append-only trigger off, runs statement, and turns it on.
func tamper(t *testing.T, db *sql.DB, statement string, args ...interface{}) {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`ALTER TABLE audit_events DISABLE TRIGGER audit_events_no_update_or_delete`)
	require.NoError(t, err)
	_, err = tx.Exec(statement, args...)
	require.NoError(t, err, statement)
	_, err = tx.Exec(`ALTER TABLE audit_events ENABLE TRIGGER audit_events_no_update_or_delete`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

// TestAuditEvents_SealWhatAReadReturns records events whose values
// PostgreSQL or Record rewrite, directly and in a transaction. Each must
// read back as verified (#337).
func TestAuditEvents_SealWhatAReadReturns(t *testing.T) {
	e := newEnv(t)
	e.sealEvents(t)
	ctx := context.Background()
	events := map[string]audit.Event{
		"ipv4":          {IP: "203.0.113.7"},
		"ipv6":          {IP: "2001:DB8:0:0::1"},
		"mapped":        {IP: "::ffff:203.0.113.8"},
		"invalid ip":    {IP: "not an address"},
		"user agent":    {UserAgent: "agent\xff\x00/1.0 " + strings.Repeat("x", 600)},
		"upper case hk": {ActorType: audit.ActorUser, ActorID: strings.ToUpper(uuid.NewString())},
		"empty":         {},
		"reason":        {Outcome: audit.Failure, Reason: "code_invalid", RequestID: "req-1"},
		"jsonb": {Changes: &audit.Changes{
			Before: map[string]interface{}{"zeta": 1.50, "alpha": 1e21, "nested": map[string]interface{}{"b": true, "a": nil}},
			After:  map[string]interface{}{"list": []interface{}{3, "two", 1.0}, "unicode": "é ", "html": "<a&b>"},
		}},
	}
	record := func(db audit.Execer, name string, ev audit.Event) {
		if ev.ActorType == "" {
			ev.ActorType = audit.ActorSystem
		}
		if ev.Outcome == "" {
			ev.Outcome = audit.Success
		}
		ev.Action, ev.TargetType, ev.TargetID = "test.seal", "test", name
		require.NoError(t, audit.Record(ctx, db, ev), name)
	}
	for name, ev := range events {
		record(e.conn, name, ev)
	}
	tx, err := e.conn.Begin()
	require.NoError(t, err)
	record(tx, "in a transaction", audit.Event{IP: "198.51.100.1"})
	require.NoError(t, tx.Commit())

	for name := range events {
		assert.Equal(t, audit.IntegrityVerified, integrityOf(t, e.conn, name), name)
	}
	assert.Equal(t, audit.IntegrityVerified, integrityOf(t, e.conn, "in a transaction"))
}

// TestAuditEvents_DetectChangesMadeInSQL changes sealed events directly in
// the database, each in a different way, and expects the query to report
// each one.
func TestAuditEvents_DetectChangesMadeInSQL(t *testing.T) {
	e := newEnv(t)
	e.sealEvents(t)
	ctx := context.Background()
	record := func(target string) {
		require.NoError(t, audit.Record(ctx, e.conn, audit.Event{ActorType: audit.ActorAdmin, ActorID: uuid.NewString(),
			Action: "test.tamper", Outcome: audit.Success, TargetType: "test", TargetID: target, IP: "203.0.113.7",
			Changes: &audit.Changes{After: map[string]interface{}{"status": "approved"}}}))
	}
	changes := map[string]string{
		"outcome":         `UPDATE audit_events SET outcome = 'failure' WHERE target_id = $1`,
		"NULL to empty":   `UPDATE audit_events SET request_id = '' WHERE target_id = $1`,
		"actor":           `UPDATE audit_events SET actor_id = gen_random_uuid() WHERE target_id = $1`,
		"time":            `UPDATE audit_events SET occurred_at = occurred_at - interval '1 day' WHERE target_id = $1`,
		"address to net":  `UPDATE audit_events SET ip = '203.0.113.7/24' WHERE target_id = $1`,
		"changes":         `UPDATE audit_events SET changes = '{"after": {"status": "rejected"}}' WHERE target_id = $1`,
		"unknown key":     `UPDATE audit_events SET seal_key_version = 99 WHERE target_id = $1`,
		"seal of another": `UPDATE audit_events SET row_seal = (SELECT row_seal FROM audit_events WHERE target_id = 'donor') WHERE target_id = $1`,
	}
	record("donor")
	for name := range changes {
		record(name)
	}
	for name, statement := range changes {
		require.Equal(t, audit.IntegrityVerified, integrityOf(t, e.conn, name), name)
		tamper(t, e.conn, statement, name)
		assert.Equal(t, audit.IntegrityMismatch, integrityOf(t, e.conn, name), name)
	}
	assert.Equal(t, audit.IntegrityVerified, integrityOf(t, e.conn, "donor"), "the other events still check")

	// A removed seal makes the event look unsealed: see the limits in
	// docs/data-protection.md.
	record("removed")
	tamper(t, e.conn, `UPDATE audit_events SET row_seal = NULL, seal_key_version = NULL WHERE target_id = $1`, "removed")
	assert.Equal(t, audit.IntegrityUnsealed, integrityOf(t, e.conn, "removed"))
}

// TestAuditEvents_ReportWhatCannotBeChecked lists a sealed event on an
// instance without data keys, and an event written without them.
func TestAuditEvents_ReportWhatCannotBeChecked(t *testing.T) {
	e := newEnv(t)
	e.sealEvents(t)
	ctx := context.Background()
	require.NoError(t, audit.Record(ctx, e.conn, audit.Event{ActorType: audit.ActorSystem, Action: "test.keys",
		Outcome: audit.Success, TargetType: "test", TargetID: "sealed"}))
	restore := audit.UseSealer(nil)
	defer restore()
	require.NoError(t, audit.Record(ctx, e.conn, audit.Event{ActorType: audit.ActorSystem, Action: "test.keys",
		Outcome: audit.Success, TargetType: "test", TargetID: "plain"}))

	assert.Equal(t, audit.IntegrityUnchecked, integrityOf(t, e.conn, "sealed"))
	assert.Equal(t, audit.IntegrityUnsealed, integrityOf(t, e.conn, "plain"))
}

// TestSealsWithKey names a table whose rows still need a row-mac version,
// so that the version is not retired under them.
func TestSealsWithKey(t *testing.T) {
	e := newEnv(t)
	e.sealEvents(t)
	ctx := context.Background()

	table, err := rowseal.SealsWithKey(ctx, e.conn, 1)
	require.NoError(t, err)
	assert.Empty(t, table, "nothing is sealed yet")

	require.NoError(t, audit.Record(ctx, e.conn, audit.Event{ActorType: audit.ActorSystem, Action: "test.keys",
		Outcome: audit.Success}))
	table, err = rowseal.SealsWithKey(ctx, e.conn, 1)
	require.NoError(t, err)
	assert.Equal(t, rowseal.AuditEvents.Name, table)

	e.newUser(t)
	table, err = rowseal.SealsWithKey(ctx, e.conn, 1)
	require.NoError(t, err)
	assert.Equal(t, rowseal.Users.Name, table, "the tables that the verifier moves come first")

	table, err = rowseal.SealsWithKey(ctx, e.conn, 2)
	require.NoError(t, err)
	assert.Empty(t, table)
}
