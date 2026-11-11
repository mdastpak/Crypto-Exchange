package platform_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func controlFindings(t *testing.T, db *sql.DB, hk string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM integrity_findings
		WHERE table_name = 'platform_controls' AND ($1 = '' OR row_key = $1) AND resolved_at IS NULL`, hk).Scan(&n))
	return n
}

// TestControls_TheBackendSealsEveryWrite creates, changes, and lifts
// controls in enforce mode (#337), the lift of a lockdown with two
// administrators: every write is sealed, so the reads pass and the verifier
// finds nothing.
func TestControls_TheBackendSealsEveryWrite(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	sealer := sealtest.Use(t, db, rowseal.ModeEnforce)
	first, second := uuid.NewString(), uuid.NewString()
	approval := []platform.Feature{platform.FeatureWithdrawals}

	ends := time.Now().Add(time.Hour)
	m := create(t, db, platform.New{Kind: platform.KindMaintenance, EndsAt: &ends, CreatedBy: first})
	later := time.Now().Add(2 * time.Hour)
	_, err := platform.Update(ctx, db, m.HK, platform.Changes{EndsAt: &later}, nil, origin(first))
	require.NoError(t, err)
	l := create(t, db, platform.New{Kind: platform.KindLockdown, Feature: platform.FeatureWithdrawals, CreatedBy: first})
	assert.NotNil(t, locked(t, check(t, db, platform.FeatureWithdrawals, platform.Subject{})))

	r, err := platform.Lift(ctx, db, platform.LiftRequest{HK: l.HK, By: first, Note: "checked", Approval: approval}, origin(first))
	require.NoError(t, err)
	require.True(t, r.Pending)
	r, err = platform.Lift(ctx, db, platform.LiftRequest{HK: l.HK, By: second, Note: "approved", Approval: approval}, origin(second))
	require.NoError(t, err)
	assert.False(t, r.Pending, "the sealed request is approved")

	open, err := platform.Open(ctx, db)
	require.NoError(t, err)
	require.Len(t, open, 1)
	assert.Equal(t, m.HK, open[0].HK)
	_, err = sealer.VerifyAll(ctx)
	require.NoError(t, err)
	assert.Zero(t, controlFindings(t, db, ""))
}

// TestControls_ALockdownLiftedInSQLFailsItsCheck lifts a lockdown in SQL,
// without its audited call (#337). It no longer looks open, so every
// control is checked: in enforce mode the controls cannot be read, so a
// watcher keeps the ones it read before and closes its fail-closed features
// once they are stale, and a check in a transaction refuses; report mode
// serves the lift, with a finding.
func TestControls_ALockdownLiftedInSQLFailsItsCheck(t *testing.T) {
	for _, mode := range []string{rowseal.ModeEnforce, rowseal.ModeReport} {
		t.Run(mode, func(t *testing.T) {
			db := pgtest.Migrated(t, testDBName)
			sealtest.Use(t, db, mode)
			l := create(t, db, platform.New{Kind: platform.KindLockdown, Feature: platform.FeatureSignUp})
			_, err := db.Exec(`UPDATE platform_controls SET lifted_at = NOW() WHERE hk = $1`, l.HK)
			require.NoError(t, err)

			open, openErr := platform.Open(ctx, db)
			checkErr := check(t, db, platform.FeatureSignUp, platform.Subject{})
			refreshErr := platform.NewWatcher(db, platform.Options{}).Refresh(ctx)
			if mode == rowseal.ModeEnforce {
				assert.ErrorIs(t, openErr, rowseal.ErrUnverified)
				assert.ErrorIs(t, checkErr, rowseal.ErrUnverified)
				assert.ErrorIs(t, refreshErr, rowseal.ErrUnverified)
			} else {
				require.NoError(t, openErr)
				assert.Empty(t, open)
				assert.NoError(t, checkErr)
				assert.NoError(t, refreshErr)
			}
			assert.Equal(t, 1, controlFindings(t, db, l.HK))
		})
	}
}

// TestLift_ARequestWrittenInSQLApprovesNothing writes the lift request of a
// lockdown in SQL, in the name of another administrator, so that one could
// lift it alone (#337). In enforce mode the control fails its seal check;
// in report mode, which serves such a row, the request still needs its
// sealed audit event, so the call asks again in its own name.
func TestLift_ARequestWrittenInSQLApprovesNothing(t *testing.T) {
	for _, mode := range []string{rowseal.ModeEnforce, rowseal.ModeReport} {
		t.Run(mode, func(t *testing.T) {
			db := pgtest.Migrated(t, testDBName)
			sealtest.Use(t, db, mode)
			named, alone := uuid.NewString(), uuid.NewString()
			approval := []platform.Feature{platform.FeatureWithdrawals}
			l := create(t, db, platform.New{Kind: platform.KindLockdown, Feature: platform.FeatureWithdrawals, CreatedBy: named})
			_, err := db.Exec(`UPDATE platform_controls SET lift_requested_by = $2, lift_requested_at = NOW() WHERE hk = $1`, l.HK, named)
			require.NoError(t, err)

			r, err := platform.Lift(ctx, db, platform.LiftRequest{HK: l.HK, By: alone, Note: "alone", Approval: approval}, origin(alone))
			if mode == rowseal.ModeEnforce {
				assert.ErrorIs(t, err, rowseal.ErrUnverified)
			} else {
				require.NoError(t, err)
				assert.True(t, r.Pending, "the forged request counts as none")
				assert.Equal(t, alone, *r.Control.LiftRequestedBy)
				// The request of that call is real, and another
				// administrator approves it.
				r, err = platform.Lift(ctx, db, platform.LiftRequest{HK: l.HK, By: named, Note: "approved", Approval: approval}, origin(named))
				require.NoError(t, err)
				assert.False(t, r.Pending)
			}
		})
	}
}
