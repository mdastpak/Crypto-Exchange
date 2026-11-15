package delisting_test

import (
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/convhold"
	"github.com/mdastpak/crypto-exchange/internal/delisting"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHolds_OneBalanceThatCannotSettleDoesNotStopTheOthers settles three
// holders of a delisting, the second with a balance changed in SQL (#452):
// the others convert, it is held with row_unverified and shown in the
// report, and delisted waits for it. After a re-seal and a retry, it
// converts and the currency is delisted.
func TestHolds_OneBalanceThatCannotSettleDoesNotStopTheOthers(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	sealer := sealtest.Use(t, db, rowseal.ModeEnforce)
	b := books{t: t, db: db}
	xyz, usdt := insertCurrency(t, db, sym("X"), "approved"), insertCurrency(t, db, sym("U"), "approved")
	alice, bob, carol := b.user(), b.user(), b.user()
	b.fund(alice, xyz, "10")
	b.fund(bob, xyz, "20")
	b.fund(carol, xyz, "30")
	var broken string
	require.NoError(t, db.QueryRow(`UPDATE balances SET row_version = row_version + 7 WHERE user_id = $1 AND currency_hk = $2 RETURNING id`,
		bob.UserHK, xyz).Scan(&broken))
	start := time.Now()
	d, _, err := delisting.Schedule(ctx, db, xyz, convertPlan(start, usdt, "0.5"), origin())
	require.NoError(t, err)
	settler := delisting.Settler{}
	for range 3 {
		_, err = settler.Advance(ctx, db, start.Add(24*time.Hour), 100)
		require.NoError(t, err)
	}
	assert.Equal(t, [3]string{"5", "0", "0"}, b.balance(alice, usdt))
	assert.Equal(t, [3]string{"15", "0", "0"}, b.balance(carol, usdt))
	assert.Equal(t, "frozen", lifecycle(t, db, xyz).State, "delisted waits for the held balance")
	report, err := delisting.Report(ctx, db, xyz, d.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Blocked)
	assert.Equal(t, map[string]int{convhold.CodeRowUnverified: 1}, report.BlockedCodes)
	assert.Equal(t, 1, count(t, db, `SELECT attempts FROM currency_conversion_holds WHERE balance_id = $1`, broken),
		"the claim skips it until its retry")

	findings, err := rowseal.ListFindings(ctx, db, rowseal.ListOptions{Limit: 100})
	require.NoError(t, err)
	for _, f := range findings {
		if f.Table == rowseal.Balances.Name && f.RowKey == broken {
			_, err = sealer.ResealFinding(ctx, f.ID, audit.Event{ActorType: audit.ActorSystem}, "the balance was reviewed")
			require.NoError(t, err)
		}
	}
	_, err = delisting.Retry(ctx, db, usdt, d.ID, origin())
	assert.ErrorIs(t, err, delisting.ErrNotFound, "through its currency only")
	n, err := delisting.Retry(ctx, db, xyz, d.ID, origin())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	_, err = settler.Advance(ctx, db, start.Add(24*time.Hour), 100)
	require.NoError(t, err)
	assert.Equal(t, [3]string{"10", "0", "0"}, b.balance(bob, usdt))
	assert.Equal(t, "delisted", lifecycle(t, db, xyz).State)
	report, err = delisting.Report(ctx, db, xyz, d.ID)
	require.NoError(t, err)
	assert.Zero(t, report.Blocked)
	assert.Equal(t, 0, count(t, db, `SELECT count(*) FROM currency_conversion_holds WHERE balance_id = $1 AND released_at IS NULL`, broken))
}
