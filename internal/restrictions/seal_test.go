package restrictions_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func restrictionFindings(t *testing.T, db *sql.DB, hk string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM integrity_findings
		WHERE table_name = 'user_restrictions' AND row_key = $1 AND resolved_at IS NULL`, hk).Scan(&n))
	return n
}

// TestRestrictions_TheBackendSealsCreateAndLift creates and lifts
// restrictions in enforce mode (#337): each write is sealed, so every check
// passes.
func TestRestrictions_TheBackendSealsCreateAndLift(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	sealtest.Use(t, db, rowseal.ModeEnforce)
	user := usertest.Insert(t, db, usertest.User{Email: "sealed@example.com"})

	freeze := create(t, db, restrictions.New{UserHK: user, Scope: restrictions.ScopeAccount})
	assert.Equal(t, restrictions.CodeAccountFrozen, refusal(t, restrictions.Check(ctx, db, user, restrictions.OpDeposit, "")).Code)
	admin := uuid.NewString()
	_, err := restrictions.Lift(ctx, db, user, freeze.Restriction.HK, admin, "reviewed", nil, origin(admin))
	require.NoError(t, err)
	assert.NoError(t, restrictions.Check(ctx, db, user, restrictions.OpDeposit, ""))
	active, err := restrictions.Active(ctx, db, user)
	require.NoError(t, err)
	assert.Empty(t, active)
	assert.Zero(t, restrictionFindings(t, db, freeze.Restriction.HK))
}

// TestRestrictions_AFreezeLiftedInSQLStillFreezes lifts a freeze in SQL,
// and ends a block early there (#337). Neither looks active any more, so
// every restriction of the user is checked: in enforce mode the user is
// refused as frozen, and neither can be lifted through the backend; report
// mode serves them as changed, with a finding.
func TestRestrictions_AFreezeLiftedInSQLStillFreezes(t *testing.T) {
	for _, mode := range []string{rowseal.ModeEnforce, rowseal.ModeReport} {
		t.Run(mode, func(t *testing.T) {
			db := pgtest.Migrated(t, testDBName)
			sealtest.Use(t, db, mode)
			user := usertest.Insert(t, db, usertest.User{Email: "lifted@example.com"})
			freeze := create(t, db, restrictions.New{UserHK: user, Scope: restrictions.ScopeAccount})
			ends := time.Now().Add(48 * time.Hour)
			block := create(t, db, restrictions.New{UserHK: user, Scope: restrictions.ScopeOperation,
				Operations: []restrictions.Operation{restrictions.OpWithdraw}, EndsAt: &ends})

			_, err := db.Exec(`UPDATE user_restrictions SET lifted_at = NOW(), lifted_by = $2 WHERE hk = $1`,
				freeze.Restriction.HK, uuid.NewString())
			require.NoError(t, err)
			_, err = db.Exec(`UPDATE user_restrictions SET ends_at = NOW() - INTERVAL '1 second', created_at = NOW() - INTERVAL '1 hour'
				WHERE hk = $1`, block.Restriction.HK)
			require.NoError(t, err)

			checkErr := restrictions.Check(ctx, db, user, restrictions.OpWithdraw, "")
			_, activeErr := restrictions.Active(ctx, db, user)
			admin := uuid.NewString()
			_, liftErr := restrictions.Lift(ctx, db, user, block.Restriction.HK, admin, "reviewed", nil, origin(admin))
			if mode == rowseal.ModeEnforce {
				assert.Equal(t, restrictions.CodeAccountFrozen, refusal(t, checkErr).Code, "a changed restriction counts as a freeze")
				assert.ErrorIs(t, activeErr, rowseal.ErrUnverified)
				assert.ErrorIs(t, liftErr, rowseal.ErrUnverified, "a changed restriction is not lifted over")
			} else {
				assert.NoError(t, checkErr)
				assert.NoError(t, activeErr)
				assert.NoError(t, liftErr)
			}
			assert.Equal(t, 1, restrictionFindings(t, db, freeze.Restriction.HK))
			assert.Equal(t, 1, restrictionFindings(t, db, block.Restriction.HK))
		})
	}
}
