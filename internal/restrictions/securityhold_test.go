package restrictions_test

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startHold starts a security hold of d in its own transaction.
func startHold(t *testing.T, db *sql.DB, user string, d time.Duration) time.Time {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	until, err := restrictions.StartSecurityHold(ctx, tx, user, d)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return until
}

// TestSecurityHold_HoldsWithdrawalsUntilItEnds covers #251: a hold blocks
// withdrawals, and only them, through restrictions.Check, until it ends by
// the database clock.
func TestSecurityHold_HoldsWithdrawalsUntilItEnds(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	user := usertest.Insert(t, db, usertest.User{Email: "hold@example.com"})
	other := usertest.Insert(t, db, usertest.User{Email: "not-held@example.com"})

	hold, err := restrictions.SecurityHold(ctx, db, user)
	require.NoError(t, err)
	assert.Nil(t, hold, "no hold before a change")

	until := startHold(t, db, user, 24*time.Hour)
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), until, time.Minute)

	r := refusal(t, restrictions.Check(ctx, db, user, restrictions.OpWithdraw, ""))
	assert.Equal(t, restrictions.CodeSecurityHold, r.Code)
	require.NotNil(t, r.Until)
	assert.True(t, until.Equal(*r.Until))
	for _, op := range []restrictions.Operation{restrictions.OpDeposit, restrictions.OpBuy, restrictions.OpSell, restrictions.OpConvert} {
		assert.NoError(t, restrictions.Check(ctx, db, user, op, ""), "%s is not held", op)
	}
	assert.NoError(t, restrictions.Check(ctx, db, other, restrictions.OpWithdraw, ""), "a hold is per user")
	assert.Equal(t, restrictions.CodeSecurityHold,
		refusal(t, restrictions.CheckSecurityHold(ctx, db, user, "register_destination")).Code,
		"other second-factor operations check the hold directly")

	_, err = db.Exec(`UPDATE users SET security_hold_until = NOW() - interval '1 second' WHERE hk = $1`, user)
	require.NoError(t, err)
	assert.NoError(t, restrictions.Check(ctx, db, user, restrictions.OpWithdraw, ""), "an end needs no job")
	hold, err = restrictions.SecurityHold(ctx, db, user)
	require.NoError(t, err)
	assert.Nil(t, hold)
}

func TestSecurityHold_IsExtendedAndNeverShortened(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	user := usertest.Insert(t, db, usertest.User{Email: "extend@example.com"})

	long := startHold(t, db, user, 48*time.Hour)
	assert.True(t, long.Equal(startHold(t, db, user, time.Hour)), "a shorter hold keeps the longer one")
	longer := startHold(t, db, user, 72*time.Hour)
	assert.True(t, longer.After(long), "a longer hold extends it")

	// A hold that ended is replaced, not extended from its old end.
	_, err := db.Exec(`UPDATE users SET security_hold_until = NOW() - interval '10 days' WHERE hk = $1`, user)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(time.Hour), startHold(t, db, user, time.Hour), time.Minute)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = restrictions.StartSecurityHold(ctx, tx, uuid.NewString(), time.Hour)
	assert.ErrorIs(t, err, restrictions.ErrUserNotFound)
	_, err = restrictions.StartSecurityHold(ctx, tx, user, 0)
	assert.Error(t, err, "a hold needs a length")
}

func TestSecurityHold_ARestrictionComesFirst(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	user := usertest.Insert(t, db, usertest.User{Email: "both@example.com"})
	startHold(t, db, user, time.Hour)
	create(t, db, restrictions.New{UserHK: user, Scope: restrictions.ScopeOperation,
		Operations: []restrictions.Operation{restrictions.OpWithdraw}})
	assert.Equal(t, restrictions.CodeOperationRestricted, refusal(t, restrictions.Check(ctx, db, user, restrictions.OpWithdraw, "")).Code)
}

func TestSend_SecurityHold(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	until := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	require.True(t, restrictions.Send(c, &restrictions.Refusal{Code: restrictions.CodeSecurityHold, Operation: restrictions.OpWithdraw, Until: &until}))
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"security_hold"`)
	assert.Contains(t, w.Body.String(), `"until":"2026-11-09T12:00:00Z"`)
}
