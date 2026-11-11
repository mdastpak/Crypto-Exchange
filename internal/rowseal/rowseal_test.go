package rowseal_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"log"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDBName = "testdb_rowseal"

type env struct {
	conn   *sql.DB
	store  *keys.Store
	live   *keys.Live
	logs   *bytes.Buffer
	sealer func(mode string) *rowseal.Sealer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	conn := pgtest.Migrated(t, testDBName)
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	provider, err := keys.NewLocalProvider(base64.StdEncoding.EncodeToString(kek))
	require.NoError(t, err)
	store := &keys.Store{DB: conn, Provider: provider}
	live, err := keys.NewLive(context.Background(), store, keys.LiveOptions{})
	require.NoError(t, err)
	e := &env{conn: conn, store: store, live: live, logs: &bytes.Buffer{}}
	e.sealer = func(mode string) *rowseal.Sealer {
		s := rowseal.New(conn, live, mode)
		s.Logger = log.New(e.logs, "", 0)
		t.Cleanup(rowseal.Use(s))
		return s
	}
	return e
}

// newUser inserts a user and seals it, as the backend writes one.
func (e *env) newUser(t *testing.T) string {
	t.Helper()
	hk := usertest.Insert(t, e.conn, usertest.User{Status: "ACTIVE"})
	require.NoError(t, rowseal.Reseal(context.Background(), e.conn, rowseal.Users, "hk = $1", hk))
	return hk
}

// readUser reads the sealed columns of a user.
func (e *env) readUser(t *testing.T, hk string) *rowseal.Row {
	t.Helper()
	r := rowseal.Users.NewRow()
	require.NoError(t, e.conn.QueryRow(`SELECT `+rowseal.Users.Fields("")+` FROM users WHERE hk = $1`, hk).Scan(r.Dest()...))
	return r
}

func (e *env) openFindings(t *testing.T) []rowseal.Finding {
	t.Helper()
	findings, err := rowseal.ListFindings(context.Background(), e.conn, rowseal.ListOptions{})
	require.NoError(t, err)
	return findings
}

func (e *env) events(t *testing.T, action string) int {
	t.Helper()
	var n int
	require.NoError(t, e.conn.QueryRow(`SELECT count(*) FROM audit_events WHERE action = $1`, action).Scan(&n))
	return n
}

func TestReseal_SealsWhatTheBackendWrites(t *testing.T) {
	e := newEnv(t)
	e.sealer(rowseal.ModeEnforce)
	hk := e.newUser(t)

	r := e.readUser(t, hk)
	assert.Equal(t, int64(1), r.Version)
	assert.Equal(t, int64(1), r.KeyVersion.Int64)
	assert.Len(t, r.Seal, 32)
	require.NoError(t, rowseal.Check(context.Background(), rowseal.Users, r))

	// A write through the backend seals again, with the next version.
	_, err := e.conn.Exec(`UPDATE users SET first_name = 'Sara' WHERE hk = $1`, hk)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(context.Background(), e.conn, rowseal.Users, "hk = $1", hk))
	r = e.readUser(t, hk)
	assert.Equal(t, int64(2), r.Version)
	require.NoError(t, rowseal.Check(context.Background(), rowseal.Users, r))
	assert.Empty(t, e.openFindings(t))
}

func TestCheck_DetectsChangesMadeInSQL(t *testing.T) {
	for name, change := range map[string]string{
		"a role":                  `UPDATE users SET role = 'admin' WHERE hk = $1`,
		"a status":                `UPDATE users SET status = 'SUSPENDED' WHERE hk = $1`,
		"a deletion":              `UPDATE users SET deleted_at = NOW() WHERE hk = $1`,
		"a security hold cleared": `UPDATE users SET security_hold_until = NOW() - INTERVAL '1 day' WHERE hk = $1`,
		"a removed seal":          `UPDATE users SET row_seal = NULL WHERE hk = $1`,
		"a seal of another row":   `UPDATE users SET row_seal = (SELECT row_seal FROM users o WHERE o.hk <> users.hk LIMIT 1) WHERE hk = $1`,
		"a raised version":        `UPDATE users SET row_version = row_version + 1 WHERE hk = $1`,
		"an unknown key":          `UPDATE users SET seal_key_version = 99 WHERE hk = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.sealer(rowseal.ModeEnforce)
			e.newUser(t) // the other row
			hk := e.newUser(t)
			_, err := e.conn.Exec(change, hk)
			require.NoError(t, err)

			err = rowseal.Check(context.Background(), rowseal.Users, e.readUser(t, hk))
			require.ErrorIs(t, err, rowseal.ErrUnverified)
			findings := e.openFindings(t)
			require.Len(t, findings, 1)
			assert.Equal(t, "users", findings[0].Table)
			assert.Equal(t, hk, findings[0].RowKey)
			assert.Equal(t, rowseal.DetectedByRead, findings[0].DetectedBy)
			assert.Equal(t, 1, e.events(t, audit.ActionIntegritySealMismatch))
			assert.Contains(t, e.logs.String(), "SECURITY")
		})
	}
}

func TestCheck_CountsRepeatedDetectionsOnOneFinding(t *testing.T) {
	e := newEnv(t)
	e.sealer(rowseal.ModeEnforce)
	hk := e.newUser(t)
	_, err := e.conn.Exec(`UPDATE users SET status = 'ACTIVE', role = 'admin' WHERE hk = $1`, hk)
	require.NoError(t, err)
	for range 3 {
		require.ErrorIs(t, rowseal.Check(context.Background(), rowseal.Users, e.readUser(t, hk)), rowseal.ErrUnverified)
	}
	findings := e.openFindings(t)
	require.Len(t, findings, 1)
	assert.Equal(t, int64(3), findings[0].SeenCount)
	assert.Equal(t, 1, e.events(t, audit.ActionIntegritySealMismatch), "one event per finding, not per request")
}

func TestCheck_ReportModeRecordsAndServes(t *testing.T) {
	e := newEnv(t)
	e.sealer(rowseal.ModeReport)
	hk := e.newUser(t)
	_, err := e.conn.Exec(`UPDATE users SET role = 'admin' WHERE hk = $1`, hk)
	require.NoError(t, err)
	require.NoError(t, rowseal.Check(context.Background(), rowseal.Users, e.readUser(t, hk)))
	assert.Len(t, e.openFindings(t), 1)
}

func TestCheck_WithoutASealerChecksNothing(t *testing.T) {
	e := newEnv(t)
	t.Cleanup(rowseal.Use(nil))
	hk := usertest.Insert(t, e.conn, usertest.User{Status: "ACTIVE"})
	require.NoError(t, rowseal.Reseal(context.Background(), e.conn, rowseal.Users, "hk = $1", hk))
	r := e.readUser(t, hk)
	assert.Equal(t, int64(0), r.Version, "nothing is sealed")
	require.NoError(t, rowseal.Check(context.Background(), rowseal.Users, r))
	assert.Empty(t, e.openFindings(t))
}

func TestSeal_DoesNotDependOnTheSessionTimeZone(t *testing.T) {
	e := newEnv(t)
	e.sealer(rowseal.ModeEnforce)
	ctx := context.Background()
	hk := e.newUser(t)
	_, err := e.conn.Exec(`UPDATE users SET security_hold_until = NOW() + INTERVAL '1 day', deleted_at = NOW() WHERE hk = $1`, hk)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, e.conn, rowseal.Users, "hk = $1", hk))

	conn, err := e.conn.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(ctx, `SET TimeZone = 'Asia/Tehran'; SET DateStyle = 'SQL, DMY'`)
	require.NoError(t, err)
	r := rowseal.Users.NewRow()
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT `+rowseal.Users.Fields("")+` FROM users WHERE hk = $1`, hk).Scan(r.Dest()...))
	require.NoError(t, rowseal.Check(ctx, rowseal.Users, r))
}

func TestVerifyAll_EnforceModeReportsAndLeavesTheRows(t *testing.T) {
	e := newEnv(t)
	sealer := e.sealer(rowseal.ModeEnforce)
	ctx := context.Background()
	good := e.newUser(t)
	bad := e.newUser(t)
	unsealed := usertest.Insert(t, e.conn, usertest.User{Status: "ACTIVE"})
	_, err := e.conn.Exec(`UPDATE users SET status = 'BANNED' WHERE hk = $1`, bad)
	require.NoError(t, err)

	report, err := sealer.VerifyAll(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, report.Checked)
	assert.Equal(t, 2, report.Findings)
	assert.Zero(t, report.Resealed)
	findings := e.openFindings(t)
	require.Len(t, findings, 2)
	checks := map[string]string{}
	for _, f := range findings {
		checks[f.RowKey] = f.Check
		assert.Equal(t, rowseal.DetectedByVerifier, f.DetectedBy)
	}
	assert.Equal(t, map[string]string{bad: rowseal.CheckMismatch, unsealed: rowseal.CheckMissing}, checks)
	require.ErrorIs(t, rowseal.Check(ctx, rowseal.Users, e.readUser(t, bad)), rowseal.ErrUnverified)
	require.NoError(t, rowseal.Check(ctx, rowseal.Users, e.readUser(t, good)))

	// A second pass counts on the same findings.
	_, err = sealer.VerifyAll(ctx)
	require.NoError(t, err)
	assert.Len(t, e.openFindings(t), 2)
	assert.Equal(t, 2, e.events(t, audit.ActionIntegritySealMismatch))
}

func TestVerifyAll_ReportModeAdoptsTheRows(t *testing.T) {
	e := newEnv(t)
	sealer := e.sealer(rowseal.ModeReport)
	ctx := context.Background()
	unsealed := usertest.Insert(t, e.conn, usertest.User{Status: "ACTIVE", Email: "adopt@example.com"})

	report, err := sealer.VerifyAll(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, report.Findings, "the user and its contact were never sealed")
	assert.Equal(t, 2, report.Resealed)
	assert.Empty(t, e.openFindings(t))
	all, err := rowseal.ListFindings(ctx, e.conn, rowseal.ListOptions{All: true})
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, "report_mode", all[0].Resolution)

	// The rows match from now on, also in enforce mode.
	enforce := rowseal.New(e.conn, e.live, rowseal.ModeEnforce)
	require.NoError(t, enforce.Check(ctx, rowseal.Users, e.readUser(t, unsealed)))
	report, err = enforce.VerifyAll(ctx)
	require.NoError(t, err)
	assert.Zero(t, report.Findings)
}

func TestVerifyAll_SealsAgainWithARotatedKey(t *testing.T) {
	e := newEnv(t)
	sealer := e.sealer(rowseal.ModeEnforce)
	ctx := context.Background()
	hk := e.newUser(t)
	_, err := e.store.Rotate(ctx, keys.PurposeRowMAC)
	require.NoError(t, err)
	_, err = e.live.Reload(ctx)
	require.NoError(t, err)

	// The old seal still matches.
	require.NoError(t, rowseal.Check(ctx, rowseal.Users, e.readUser(t, hk)))
	report, err := sealer.VerifyAll(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Rotated)
	r := e.readUser(t, hk)
	assert.Equal(t, int64(2), r.KeyVersion.Int64)
	require.NoError(t, rowseal.Check(ctx, rowseal.Users, r))

	// Once every row uses version 2, version 1 can be retired.
	require.NoError(t, e.store.Retire(ctx, keys.PurposeRowMAC, 1))
	_, err = e.live.Reload(ctx)
	require.NoError(t, err)
	require.NoError(t, rowseal.Check(ctx, rowseal.Users, e.readUser(t, hk)))
}

func TestVerifyAll_SkipsWhileAnotherInstanceVerifies(t *testing.T) {
	e := newEnv(t)
	sealer := e.sealer(rowseal.ModeEnforce)
	ctx := context.Background()
	tx, err := e.conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`SELECT pg_advisory_xact_lock($1)`, int64(0x726f777365616c))
	require.NoError(t, err)
	report, err := sealer.VerifyAll(ctx)
	require.NoError(t, err)
	assert.True(t, report.Skipped)
}

func TestResealFinding_ClosesTheFindingWithAnAuditEvent(t *testing.T) {
	e := newEnv(t)
	sealer := e.sealer(rowseal.ModeEnforce)
	ctx := context.Background()
	hk := e.newUser(t)
	_, err := e.conn.Exec(`UPDATE users SET first_name = 'Fixed', totp_secret = 'x' WHERE hk = $1`, hk)
	require.NoError(t, err)
	require.ErrorIs(t, rowseal.Check(ctx, rowseal.Users, e.readUser(t, hk)), rowseal.ErrUnverified)
	finding := e.openFindings(t)[0]

	_, err = sealer.ResealFinding(ctx, finding.ID, audit.Event{ActorType: audit.ActorAdmin}, " ")
	require.Error(t, err, "a reason is required")

	admin := e.newUser(t)
	origin := audit.Event{ActorType: audit.ActorAdmin, ActorID: admin}
	resolved, err := sealer.ResealFinding(ctx, finding.ID, origin, "Reviewed data fix of ticket 42")
	require.NoError(t, err)
	assert.Equal(t, "resealed", resolved.Resolution)
	assert.Equal(t, admin, resolved.ResolvedBy)
	assert.Equal(t, "Reviewed data fix of ticket 42", resolved.Reason)
	require.NoError(t, rowseal.Check(ctx, rowseal.Users, e.readUser(t, hk)))

	var changes string
	require.NoError(t, e.conn.QueryRow(`SELECT changes::text FROM audit_events WHERE action = $1 AND actor_id = $2`,
		audit.ActionIntegrityReseal, admin).Scan(&changes))
	assert.Contains(t, changes, `"first_name": "Fixed"`)
	assert.Contains(t, changes, `"totp_secret": "[secret]"`)
	assert.NotContains(t, changes, `"x"`, "secret values never reach the audit log")

	_, err = sealer.ResealFinding(ctx, finding.ID, origin, "again")
	require.ErrorIs(t, err, rowseal.ErrResolved)
	_, err = sealer.ResealFinding(ctx, finding.ID+100, origin, "none")
	require.ErrorIs(t, err, rowseal.ErrNotFound)
}

func TestReseal_SessionsAndContacts(t *testing.T) {
	e := newEnv(t)
	e.sealer(rowseal.ModeEnforce)
	ctx := context.Background()
	hk := usertest.Insert(t, e.conn, usertest.User{Status: "ACTIVE", Email: "seal@example.com"})
	require.NoError(t, rowseal.Reseal(ctx, e.conn, rowseal.UserContacts, "user_hk = $1", hk))
	var sessions []string
	for range 2 {
		var id string
		require.NoError(t, e.conn.QueryRow(`INSERT INTO sessions (user_id, auth_method, access_token_hash, refresh_token_hash, expires_at)
			VALUES ($1, 'otp', 'a', 'r', NOW() + INTERVAL '1 hour') RETURNING id`, hk).Scan(&id))
		sessions = append(sessions, id)
	}
	require.NoError(t, rowseal.Reseal(ctx, e.conn, rowseal.Sessions, "id = ANY($1::uuid[])", sessions))

	for _, tc := range []struct {
		table *rowseal.Table
		where string
		arg   any
	}{
		{rowseal.UserContacts, "user_hk = $1", hk},
		{rowseal.Sessions, "id = $1", sessions[0]},
		{rowseal.Sessions, "id = $1", sessions[1]},
	} {
		r := tc.table.NewRow()
		require.NoError(t, e.conn.QueryRow(`SELECT `+tc.table.Fields("")+` FROM `+tc.table.Name+` WHERE `+tc.where, tc.arg).Scan(r.Dest()...))
		require.NoError(t, rowseal.Check(ctx, tc.table, r), tc.table.Name)
	}

	// A revoked session set back to active is not trusted.
	_, err := e.conn.Exec(`UPDATE sessions SET status = 'REVOKED' WHERE id = $1`, sessions[0])
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, e.conn, rowseal.Sessions, "id = $1", sessions[0]))
	_, err = e.conn.Exec(`UPDATE sessions SET status = 'ACTIVE', revoked_at = NULL WHERE id = $1`, sessions[0])
	require.NoError(t, err)
	r := rowseal.Sessions.NewRow()
	require.NoError(t, e.conn.QueryRow(`SELECT `+rowseal.Sessions.Fields("")+` FROM sessions WHERE id = $1`, sessions[0]).Scan(r.Dest()...))
	require.ErrorIs(t, rowseal.Check(ctx, rowseal.Sessions, r), rowseal.ErrUnverified)
}

// A re-seal by an administrator needs a second one (#308).
func TestReviewReseal_NeedsASecondAdministrator(t *testing.T) {
	e := newEnv(t)
	sealer := e.sealer(rowseal.ModeEnforce)
	// The server seals its audit events with the same keys; an approval
	// needs the sealed event of the request.
	t.Cleanup(audit.UseSealer(sealer))
	ctx := context.Background()
	first := audit.Event{ActorType: audit.ActorAdmin, ActorID: e.newUser(t)}
	second := audit.Event{ActorType: audit.ActorAdmin, ActorID: e.newUser(t)}
	changed := func() int64 {
		_, err := e.conn.Exec(`UPDATE users SET first_name = 'Changed' WHERE hk = $1`, first.ActorID)
		require.NoError(t, err)
		require.ErrorIs(t, rowseal.Check(ctx, rowseal.Users, e.readUser(t, first.ActorID)), rowseal.ErrUnverified)
		return e.openFindings(t)[0].ID
	}
	id := changed()

	_, _, err := sealer.ReviewReseal(ctx, id, first, "my own row")
	require.ErrorIs(t, err, rowseal.ErrOwnRow, "the account of the row cannot ask")
	_, _, err = sealer.ReviewReseal(ctx, id, audit.Event{ActorType: audit.ActorAdmin}, "nobody")
	require.Error(t, err, "an administrator is required")

	third := audit.Event{ActorType: audit.ActorAdmin, ActorID: e.newUser(t)}
	pending, waits, err := sealer.ReviewReseal(ctx, id, second, "Reviewed fix of ticket 7")
	require.NoError(t, err)
	assert.True(t, waits)
	assert.Nil(t, pending.ResolvedAt)
	assert.Equal(t, second.ActorID, pending.ResealRequestedBy)
	assert.Equal(t, "Reviewed fix of ticket 7", pending.ResealRequestReason)
	require.ErrorIs(t, rowseal.Check(ctx, rowseal.Users, e.readUser(t, first.ActorID)), rowseal.ErrUnverified, "nothing is sealed yet")

	_, _, err = sealer.ReviewReseal(ctx, id, second, "again, alone")
	require.ErrorIs(t, err, rowseal.ErrSameApprover)
	_, _, err = sealer.ReviewReseal(ctx, id, first, "approve my own row")
	require.ErrorIs(t, err, rowseal.ErrOwnRow, "nor approve")

	resolved, waits, err := sealer.ReviewReseal(ctx, id, third, "Confirmed with the ticket")
	require.NoError(t, err)
	assert.False(t, waits)
	assert.Equal(t, "resealed", resolved.Resolution)
	assert.Equal(t, third.ActorID, resolved.ResolvedBy)
	require.NoError(t, rowseal.Check(ctx, rowseal.Users, e.readUser(t, first.ActorID)))
	assert.Equal(t, 1, e.events(t, audit.ActionIntegrityResealRequest))
	var changes string
	require.NoError(t, e.conn.QueryRow(`SELECT changes::text FROM audit_events WHERE action = $1`, audit.ActionIntegrityReseal).Scan(&changes))
	assert.Contains(t, changes, second.ActorID, "the event names who asked")
	assert.Contains(t, changes, "Reviewed fix of ticket 7")

	// A request that waited too long starts again.
	_, err = e.conn.Exec(`UPDATE users SET first_name = 'Again' WHERE hk = $1`, first.ActorID)
	require.NoError(t, err)
	require.ErrorIs(t, rowseal.Check(ctx, rowseal.Users, e.readUser(t, first.ActorID)), rowseal.ErrUnverified)
	id = e.openFindings(t)[0].ID
	_, waits, err = sealer.ReviewReseal(ctx, id, second, "first request")
	require.NoError(t, err)
	require.True(t, waits)
	_, err = e.conn.Exec(`UPDATE integrity_findings SET reseal_requested_at = NOW() - $2 * INTERVAL '1 second' WHERE id = $1`,
		id, (rowseal.ResealApprovalTTL + time.Minute).Seconds())
	require.NoError(t, err)
	late, waits, err := sealer.ReviewReseal(ctx, id, third, "too late to approve")
	require.NoError(t, err)
	assert.True(t, waits, "the late approval is a new request")
	assert.Equal(t, third.ActorID, late.ResealRequestedBy)
}

// TestReviewReseal_TheStateOfAnAccountBelongsToIt checks the owner of each
// table of #337: an administrator cannot ask for the re-seal of their own
// authenticator, recovery, step-up, admin session, restriction, or recovery
// code, which they could change in SQL to skip their own second factor or
// lift their own freeze. A platform control belongs to no account.
func TestReviewReseal_TheStateOfAnAccountBelongsToIt(t *testing.T) {
	e := newEnv(t)
	sealer := e.sealer(rowseal.ModeEnforce)
	ctx := context.Background()
	admin := audit.Event{ActorType: audit.ActorAdmin, ActorID: e.newUser(t)}
	other := audit.Event{ActorType: audit.ActorAdmin, ActorID: e.newUser(t)}

	var session, recovery, adminSession, restriction, code, control string
	require.NoError(t, e.conn.QueryRow(`INSERT INTO sessions (user_id, auth_method, access_token_hash, refresh_token_hash, expires_at)
		VALUES ($1, 'otp', 'a', 'r', NOW() + INTERVAL '1 hour') RETURNING id`, admin.ActorID).Scan(&session))
	_, err := e.conn.Exec(`INSERT INTO session_step_ups (session_id, elevated_until) VALUES ($1, NOW() + INTERVAL '5 minutes')`, session)
	require.NoError(t, err)
	_, err = e.conn.Exec(`INSERT INTO user_totp (user_hk, secret_sealed) VALUES ($1, 'sealed')`, admin.ActorID)
	require.NoError(t, err)
	require.NoError(t, e.conn.QueryRow(`INSERT INTO account_recoveries (id, user_hk, mode, expires_at)
		VALUES (gen_random_uuid(), $1, 'standard', NOW() + INTERVAL '1 day') RETURNING id`, admin.ActorID).Scan(&recovery))
	require.NoError(t, e.conn.QueryRow(`INSERT INTO admin_sessions (admin_id, expires_at)
		VALUES ($1, NOW() + INTERVAL '1 hour') RETURNING id`, admin.ActorID).Scan(&adminSession))
	require.NoError(t, e.conn.QueryRow(`INSERT INTO user_restrictions (hk, user_hk, scope, reason, created_by)
		VALUES (gen_random_uuid(), $1, 'account', 'aml_review', $2) RETURNING hk`, admin.ActorID, other.ActorID).Scan(&restriction))
	require.NoError(t, e.conn.QueryRow(`INSERT INTO user_recovery_codes (user_hk, code_hash)
		VALUES ($1, 'hash') RETURNING id::text`, admin.ActorID).Scan(&code))
	require.NoError(t, e.conn.QueryRow(`INSERT INTO platform_controls (hk, kind, feature, starts_at, announce_at, reason)
		VALUES (gen_random_uuid(), 'lockdown', 'withdrawals', NOW(), NOW(), 'test') RETURNING hk`).Scan(&control))

	for _, tc := range []struct {
		table *rowseal.Table
		key   string
		owned bool
	}{
		{rowseal.SessionStepUps, session, true},
		{rowseal.UserTOTP, admin.ActorID, true},
		{rowseal.AccountRecoveries, recovery, true},
		{rowseal.AdminSessions, adminSession, true},
		{rowseal.UserRestrictions, restriction, true},
		{rowseal.UserRecoveryCodes, code, true},
		{rowseal.PlatformControls, control, false},
	} {
		t.Run(tc.table.Name, func(t *testing.T) {
			r := tc.table.NewRow()
			require.NoError(t, e.conn.QueryRow(`SELECT `+tc.table.Fields("")+` FROM `+tc.table.Name+` WHERE `+tc.table.Key+` = $1`, tc.key).Scan(r.Dest()...))
			require.ErrorIs(t, rowseal.Check(ctx, tc.table, r), rowseal.ErrUnverified, "a row written in SQL has no seal")
			var id int64
			require.NoError(t, e.conn.QueryRow(`SELECT id FROM integrity_findings WHERE table_name = $1 AND row_key = $2 AND resolved_at IS NULL`,
				tc.table.Name, tc.key).Scan(&id))

			asker := other
			if tc.owned {
				_, _, err := sealer.ReviewReseal(ctx, id, admin, "my own row")
				require.ErrorIs(t, err, rowseal.ErrOwnRow)
			} else {
				asker = admin
			}
			_, waits, err := sealer.ReviewReseal(ctx, id, asker, "reviewed")
			require.NoError(t, err)
			assert.True(t, waits, "another administrator may ask")
		})
	}
}

// TestReviewReseal_ARequestWrittenInSQLCountsAsNone writes the request of a
// re-seal in SQL (#337): in the name of another administrator, with an
// audit event that nobody sealed, and by changing the reason of a real
// request. Each time the approval finds no sealed request event, so it asks
// again in its own name, and nothing is sealed.
func TestReviewReseal_ARequestWrittenInSQLCountsAsNone(t *testing.T) {
	e := newEnv(t)
	sealer := e.sealer(rowseal.ModeEnforce)
	t.Cleanup(audit.UseSealer(sealer))
	ctx := context.Background()
	named := audit.Event{ActorType: audit.ActorAdmin, ActorID: e.newUser(t)}
	alone := audit.Event{ActorType: audit.ActorAdmin, ActorID: e.newUser(t)}
	victim := e.newUser(t)
	_, err := e.conn.Exec(`UPDATE users SET first_name = 'Changed' WHERE hk = $1`, victim)
	require.NoError(t, err)
	require.ErrorIs(t, rowseal.Check(ctx, rowseal.Users, e.readUser(t, victim)), rowseal.ErrUnverified)
	id := e.openFindings(t)[0].ID
	forge := func(reason string) {
		t.Helper()
		_, err := e.conn.Exec(`UPDATE integrity_findings SET reseal_requested_by = $2, reseal_requested_at = NOW(),
			reseal_request_reason = $3 WHERE id = $1`, id, named.ActorID, reason)
		require.NoError(t, err)
	}
	approveAlone := func(msg string) {
		t.Helper()
		pending, waits, err := sealer.ReviewReseal(ctx, id, alone, "approved alone")
		require.NoError(t, err)
		assert.True(t, waits, msg)
		assert.Equal(t, alone.ActorID, pending.ResealRequestedBy, "the call asks again, in its own name")
		require.ErrorIs(t, rowseal.Check(ctx, rowseal.Users, e.readUser(t, victim)), rowseal.ErrUnverified, "nothing is sealed")
	}

	forge("Reviewed")
	approveAlone("a request without an event")
	assert.Contains(t, e.logs.String(), "has no sealed audit event")

	forge("Reviewed")
	_, err = e.conn.Exec(`INSERT INTO audit_events (hk, actor_type, actor_id, action, target_type, target_id, outcome, changes, occurred_at)
		SELECT gen_random_uuid(), 'admin', reseal_requested_by, $2, table_name, row_key, 'success',
			jsonb_build_object('after', jsonb_build_object('finding', id, 'reason', reseal_request_reason)), reseal_requested_at
		FROM integrity_findings WHERE id = $1`, id, audit.ActionIntegrityResealRequest)
	require.NoError(t, err)
	approveAlone("a request with an event that nobody sealed")
	assert.Contains(t, e.logs.String(), "fails its seal check (missing)")

	// The last call left a real request of alone, whose reason is then
	// changed in SQL: its event no longer matches, so named asks again.
	_, err = e.conn.Exec(`UPDATE integrity_findings SET reseal_request_reason = 'Something else' WHERE id = $1`, id)
	require.NoError(t, err)
	pending, waits, err := sealer.ReviewReseal(ctx, id, named, "Checked against the backup")
	require.NoError(t, err)
	assert.True(t, waits, "a request whose reason changed")
	assert.Equal(t, named.ActorID, pending.ResealRequestedBy)
	require.ErrorIs(t, rowseal.Check(ctx, rowseal.Users, e.readUser(t, victim)), rowseal.ErrUnverified, "nothing is sealed")
	assert.Equal(t, 0, e.events(t, audit.ActionIntegrityReseal))

	// A real request is approved by the other administrator.
	resolved, waits, err := sealer.ReviewReseal(ctx, id, alone, "Confirmed with the backup")
	require.NoError(t, err)
	assert.False(t, waits)
	assert.Equal(t, "resealed", resolved.Resolution)
	require.NoError(t, rowseal.Check(ctx, rowseal.Users, e.readUser(t, victim)))
}
