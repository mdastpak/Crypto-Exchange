package delisting_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/delisting"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openFindings returns the ids of the open findings of the delisting hk.
func openFindings(t *testing.T, db *sql.DB, hk string) []int64 {
	t.Helper()
	findings, err := rowseal.ListFindings(ctx, db, rowseal.ListOptions{Limit: 100})
	require.NoError(t, err)
	var ids []int64
	for _, f := range findings {
		if f.Table == rowseal.CurrencyDelistings.Name && f.RowKey == hk {
			ids = append(ids, f.ID)
		}
	}
	return ids
}

// TestSeals_AFixedPriceChangedInSQLConvertsNothing raises the fixed price of
// a delisting in SQL (#451): no price is recorded and its holders keep their
// balances, with one finding, while another delisting settles. Once an
// operator re-seals the reviewed row, the settlement goes on.
func TestSeals_AFixedPriceChangedInSQLConvertsNothing(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	sealer := sealtest.Use(t, db, rowseal.ModeEnforce)
	b := books{t: t, db: db}
	xyz, abc, usdt := insertCurrency(t, db, sym("X"), "approved"), insertCurrency(t, db, sym("A"), "approved"),
		insertCurrency(t, db, sym("U"), "approved")
	alice, bob := b.user(), b.user()
	b.fund(alice, xyz, "100")
	b.fund(bob, abc, "100")
	start := time.Now()
	d, _, err := delisting.Schedule(ctx, db, xyz, convertPlan(start, usdt, "0.5"), origin())
	require.NoError(t, err)
	_, _, err = delisting.Schedule(ctx, db, abc, convertPlan(start, usdt, "0.25"), origin())
	require.NoError(t, err)

	_, err = db.Exec(`UPDATE currency_delistings SET fixed_price = 500 WHERE hk = $1`, d.ID)
	require.NoError(t, err)
	settler := delisting.Settler{}
	for range 2 {
		_, err = settler.Advance(ctx, db, start.Add(24*time.Hour), 100)
		require.NoError(t, err)
	}
	assert.Equal(t, [3]string{"100", "0", "0"}, b.balance(alice, xyz), "nothing converts at the raised price")
	assert.Equal(t, [3]string{"0", "0", "0"}, b.balance(alice, usdt))
	assert.Nil(t, b.delisting(xyz).SettlementPrice, "no price is recorded")
	assert.Equal(t, [3]string{"25", "0", "0"}, b.balance(bob, usdt), "another delisting settles")
	ids := openFindings(t, db, d.ID)
	require.Len(t, ids, 1)
	report, err := delisting.Report(ctx, db, xyz, d.ID)
	require.NoError(t, err)
	assert.True(t, report.Unverified)

	_, err = db.Exec(`UPDATE currency_delistings SET fixed_price = 0.5 WHERE hk = $1`, d.ID)
	require.NoError(t, err)
	_, err = sealer.ResealFinding(ctx, ids[0], audit.Event{ActorType: audit.ActorSystem}, "the price was restored from the audit log")
	require.NoError(t, err)
	_, err = settler.Advance(ctx, db, start.Add(24*time.Hour), 100)
	require.NoError(t, err)
	assert.Equal(t, [3]string{"50", "0", "0"}, b.balance(alice, usdt), "it settles at the reviewed price")
	report, err = delisting.Report(ctx, db, xyz, d.ID)
	require.NoError(t, err)
	assert.False(t, report.Unverified)
}

// TestSeals_ARecordedPriceOrCurrencyChangedInSQLConvertsNothing changes the
// recorded settlement price, and then the settlement currency, of a
// delisting that settles (#451): the balance that becomes available
// afterwards does not convert.
func TestSeals_ARecordedPriceOrCurrencyChangedInSQLConvertsNothing(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	sealtest.Use(t, db, rowseal.ModeEnforce)
	b := books{t: t, db: db}
	xyz, usdt, btc := insertCurrency(t, db, sym("X"), "approved"), insertCurrency(t, db, sym("U"), "approved"),
		insertCurrency(t, db, sym("B"), "approved")
	carol := b.user()
	b.fund(carol, xyz, "40")
	l := ledger.Ledger{DB: db}
	_, err := l.Lock(ctx, "hold:carol", carol, b.amount(xyz, "40"), ledger.Ref{Type: "withdrawal", ID: "w1"})
	require.NoError(t, err)
	start := time.Now()
	d, _, err := delisting.Schedule(ctx, db, xyz, convertPlan(start, usdt, "0.5"), origin())
	require.NoError(t, err)
	settler := delisting.Settler{}
	_, err = settler.Advance(ctx, db, start.Add(24*time.Hour), 100)
	require.NoError(t, err)
	require.NotNil(t, b.delisting(xyz).SettlementPrice)
	assert.Empty(t, openFindings(t, db, d.ID), "the recorded price is sealed")

	for i, change := range []string{`UPDATE currency_delistings SET settlement_price = 5000 WHERE hk = $1`,
		`UPDATE currency_delistings SET settlement_price = 0.5, settlement_currency_hk = '` + btc + `' WHERE hk = $1`} {
		_, err = db.Exec(change, d.ID)
		require.NoError(t, err)
		_, err = l.Release(ctx, fmt.Sprintf("hold:carol:release:%d", i), carol, b.amount(xyz, "40"), ledger.Ref{Type: "withdrawal", ID: "w1"})
		require.NoError(t, err)
		_, err = settler.Advance(ctx, db, start.Add(24*time.Hour), 100)
		require.NoError(t, err)
		assert.Equal(t, [3]string{"40", "0", "0"}, b.balance(carol, xyz), change)
		assert.Equal(t, [3]string{"0", "0", "0"}, b.balance(carol, usdt))
		assert.Equal(t, [3]string{"0", "0", "0"}, b.balance(carol, btc))
		_, err = l.Lock(ctx, fmt.Sprintf("hold:carol:again:%d", i), carol, b.amount(xyz, "40"), ledger.Ref{Type: "withdrawal", ID: "w1"})
		require.NoError(t, err)
	}
	assert.Len(t, openFindings(t, db, d.ID), 1)
}

// TestSeals_EveryWriteSealsTheDelisting checks that the schedule, the steps,
// the recorded price, the completion, the cancel, and a superseding
// schedule leave delistings that match their seal.
func TestSeals_EveryWriteSealsTheDelisting(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	sealtest.Use(t, db, rowseal.ModeEnforce)
	xyz, abc, usdt := insertCurrency(t, db, sym("X"), "approved"), insertCurrency(t, db, sym("A"), "approved"),
		insertCurrency(t, db, sym("U"), "approved")
	start := time.Now()
	verify := func(why string, hks ...string) {
		t.Helper()
		_, err := rowseal.Active().VerifyAll(ctx)
		require.NoError(t, err)
		for _, hk := range hks {
			assert.Empty(t, openFindings(t, db, hk), why)
		}
	}
	d, _, err := delisting.Schedule(ctx, db, xyz, convertPlan(start, usdt, "0.5"), origin())
	require.NoError(t, err)
	verify("schedule", d.ID)
	settler := delisting.Settler{}
	_, err = settler.Advance(ctx, db, start.Add(30*time.Minute), 100)
	require.NoError(t, err)
	verify("a step", d.ID)
	_, err = settler.Advance(ctx, db, start.Add(24*time.Hour), 100)
	require.NoError(t, err)
	assert.Equal(t, delisting.StatusCompleted, b(t, db).delisting(xyz).Status)
	verify("price and completion", d.ID)

	c, _, err := delisting.Schedule(ctx, db, abc, convertPlan(start, usdt, "0.5"), origin())
	require.NoError(t, err)
	_, _, err = delisting.Cancel(ctx, db, abc, c.ID, nil, origin())
	require.NoError(t, err)
	verify("cancel", c.ID)

	// An emergency halt that stops before delisted is superseded by the
	// next schedule, which completes it.
	halt, _, err := delisting.Schedule(ctx, db, abc, plan(start, currencies.LifecycleTradingHalted), origin())
	require.NoError(t, err)
	_, err = settler.Advance(ctx, db, start.Add(time.Minute), 100)
	require.NoError(t, err)
	next, _, err := delisting.Schedule(ctx, db, abc, plan(start.Add(time.Hour), currencies.LifecycleFrozen, currencies.LifecycleDelisted), origin())
	require.NoError(t, err)
	verify("supersede", halt.ID, next.ID)
}

func b(t *testing.T, db *sql.DB) books { return books{t: t, db: db} }
