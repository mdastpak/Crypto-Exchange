package rowseal_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"log"
	"testing"

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
