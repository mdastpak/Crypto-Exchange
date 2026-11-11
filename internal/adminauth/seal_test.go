package adminauth

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSealedService is newTestService with row seals in mode, made with the
// same data keys.
func newSealedService(t *testing.T, mode string) (*Service, *sql.DB, *rowseal.Sealer) {
	t.Helper()
	conn := migratedTestDB(t)
	live, _ := newDataKeys(t, conn)
	service, err := NewService(conn, live, "Crypto Exchange")
	require.NoError(t, err)
	sealer := rowseal.New(conn, live, mode)
	t.Cleanup(rowseal.Use(sealer))
	return service, conn, sealer
}

func sessionFindings(t *testing.T, conn *sql.DB, id string) int {
	t.Helper()
	var n int
	require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM integrity_findings
		WHERE table_name = 'admin_sessions' AND ($1 = '' OR row_key = $1) AND resolved_at IS NULL`, id).Scan(&n))
	return n
}

// TestSessions_TheBackendSealsEveryWrite runs every write of a session in
// enforce mode (#337): a sign-in that replaces an older session, a step-up,
// a wrong code past the limit, an end, and the end of every session. Each
// leaves a row that matches its seal.
func TestSessions_TheBackendSealsEveryWrite(t *testing.T) {
	service, conn, sealer := newSealedService(t, rowseal.ModeEnforce)
	ctx := context.Background()
	enrollment, err := service.Create(ctx, "sealed@example.com")
	require.NoError(t, err)
	admin := enrollment.AdminID

	replaced := startTestSession(t, service, admin, 1)
	current := startTestSession(t, service, admin, 1)
	assert.ErrorIs(t, service.VerifySession(ctx, replaced, admin), ErrSessionEnded)
	require.NoError(t, service.VerifySession(ctx, current, admin))

	event := audit.Event{ActorType: audit.ActorAdmin, ActorID: admin, Action: audit.ActionAdminStepUp,
		TargetType: audit.TargetAdminSession, TargetID: current}
	_, err = service.StepUp(ctx, current, admin, at(t, service, time.Now(), enrollment), 5*time.Minute, event)
	require.NoError(t, err)
	elevated, err := service.Elevated(ctx, current, admin)
	require.NoError(t, err)
	assert.True(t, elevated, "the step-up is sealed")

	failing := startTestSession(t, service, admin, 3)
	for range MaxStepUpFailures + 1 {
		_, _ = service.StepUp(ctx, failing, admin, "000000", 5*time.Minute, event)
	}
	assert.Equal(t, SessionStepUpFailed, sessionReason(t, conn, failing))

	ended := startTestSession(t, service, admin, 3)
	revoke := audit.Event{ActorType: audit.ActorAdmin, ActorID: admin, Action: audit.ActionAdminSessionRevoke,
		Outcome: audit.Success, TargetType: audit.TargetAdminSession, TargetID: ended}
	require.NoError(t, service.EndSession(ctx, ended, admin, SessionRevoked, revoke))
	n, err := service.EndSessions(ctx, "sealed@example.com")
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)

	_, err = sealer.VerifyAll(ctx)
	require.NoError(t, err)
	assert.Zero(t, sessionFindings(t, conn, ""), "every session matches its seal")
}

// TestSessions_ChangedInSQLEndAgain changes sessions in SQL (#337): a
// revoked one set back to active, one that expired made to last, and a
// step-up lengthened. In enforce mode each session has ended, with a
// finding; report mode serves them, with the finding.
func TestSessions_ChangedInSQLEndAgain(t *testing.T) {
	for _, mode := range []string{rowseal.ModeEnforce, rowseal.ModeReport} {
		t.Run(mode, func(t *testing.T) {
			service, conn, _ := newSealedService(t, mode)
			ctx := context.Background()
			enrollment, err := service.Create(ctx, "changed@example.com")
			require.NoError(t, err)
			admin := enrollment.AdminID

			revoked := startTestSession(t, service, admin, 3)
			event := audit.Event{ActorType: audit.ActorAdmin, ActorID: admin, Action: audit.ActionAdminSessionRevoke,
				Outcome: audit.Success, TargetType: audit.TargetAdminSession, TargetID: revoked}
			require.NoError(t, service.EndSession(ctx, revoked, admin, SessionRevoked, event))
			_, err = conn.Exec(`UPDATE admin_sessions SET revoked_at = NULL, revoked_reason = NULL WHERE id = $1`, revoked)
			require.NoError(t, err)

			extended := startTestSession(t, service, admin, 3)
			_, err = conn.Exec(`UPDATE admin_sessions SET expires_at = NOW() + INTERVAL '30 days' WHERE id = $1`, extended)
			require.NoError(t, err)

			lengthened := startTestSession(t, service, admin, 3)
			_, err = conn.Exec(`UPDATE admin_sessions SET elevated_until = NOW() + INTERVAL '30 days' WHERE id = $1`, lengthened)
			require.NoError(t, err)

			elevated, elevatedErr := service.Elevated(ctx, lengthened, admin)
			require.NoError(t, elevatedErr)
			if mode == rowseal.ModeEnforce {
				assert.ErrorIs(t, service.VerifySession(ctx, revoked, admin), ErrSessionEnded, "a revoked token stays revoked")
				assert.ErrorIs(t, service.VerifySession(ctx, extended, admin), ErrSessionEnded)
				assert.False(t, elevated, "a lengthened step-up counts as none")
			} else {
				assert.NoError(t, service.VerifySession(ctx, revoked, admin))
				assert.NoError(t, service.VerifySession(ctx, extended, admin))
				assert.True(t, elevated)
			}
			for _, id := range []string{revoked, extended, lengthened} {
				assert.Equal(t, 1, sessionFindings(t, conn, id), id)
			}
		})
	}
}
