package rebrand_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/rebrand"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openFindings returns the ids of the open findings of the migration hk.
func openFindings(t *testing.T, db *sql.DB, hk string) []int64 {
	t.Helper()
	findings, err := rowseal.ListFindings(ctx, db, rowseal.ListOptions{Limit: 100})
	require.NoError(t, err)
	var ids []int64
	for _, f := range findings {
		if f.Table == rowseal.CurrencyMigrations.Name && f.RowKey == hk {
			ids = append(ids, f.ID)
		}
	}
	return ids
}

// TestSeals_ARatioChangedInSQLConvertsNothing raises the ratio of a forced
// migration in SQL (#451): its holders keep their balances, its finding is
// recorded once, and the holders of another migration still convert. Once
// an operator re-seals the reviewed ratio, the migration goes on.
func TestSeals_ARatioChangedInSQLConvertsNothing(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	sealer := sealtest.Use(t, db, rowseal.ModeEnforce)
	h := holdings{t: t, db: db}
	lend, aave := insertCurrency(t, db, "LEND"), insertCurrency(t, db, "AAVE")
	agix, fet := insertCurrency(t, db, "AGIX"), insertCurrency(t, db, "FET")
	alice, bob := h.user(), h.user()
	h.fund(alice, lend, "10")
	h.fund(bob, agix, "10")
	start := time.Now()
	tampered, err := rebrand.Schedule(ctx, db, aave, plan(start, true, []rebrand.Source{{CurrencyHK: lend, Ratio: "2"}}, allPhases...), origin())
	require.NoError(t, err)
	_, err = rebrand.Schedule(ctx, db, fet, plan(start, true, []rebrand.Source{{CurrencyHK: agix, Ratio: "3"}}, allPhases...), origin())
	require.NoError(t, err)

	_, err = db.Exec(`UPDATE currency_migration_sources SET ratio = 200 WHERE migration_hk = $1`, tampered.ID)
	require.NoError(t, err)
	for range 3 {
		_, err = rebrand.Advance(ctx, db, start.Add(24*time.Hour), 10)
		require.NoError(t, err)
	}
	assert.Equal(t, [3]string{"10", "0", "0"}, h.balance(alice, lend), "nothing converts at the raised ratio")
	assert.Equal(t, [3]string{"0", "0", "0"}, h.balance(alice, aave))
	assert.Equal(t, "scheduled", h.phase(aave), "its phases wait")
	assert.Equal(t, [3]string{"30", "0", "0"}, h.balance(bob, fet), "another migration still converts")
	assert.Equal(t, "completed", h.phase(fet))
	ids := openFindings(t, db, tampered.ID)
	require.Len(t, ids, 1, "one finding")
	progress, err := rebrand.Progress(ctx, db, aave, tampered.ID)
	require.NoError(t, err)
	assert.True(t, progress[0].Unverified)

	// The operator restores the ratio, and re-seals the reviewed row.
	_, err = db.Exec(`UPDATE currency_migration_sources SET ratio = 2 WHERE migration_hk = $1`, tampered.ID)
	require.NoError(t, err)
	_, err = sealer.ResealFinding(ctx, ids[0], audit.Event{ActorType: audit.ActorSystem}, "the ratio was restored from the audit log")
	require.NoError(t, err)
	for range 3 {
		_, err = rebrand.Advance(ctx, db, start.Add(24*time.Hour), 10)
		require.NoError(t, err)
	}
	assert.Equal(t, [3]string{"20", "0", "0"}, h.balance(alice, aave), "it converts at the reviewed ratio")
	assert.Equal(t, "completed", h.phase(aave))
	progress, err = rebrand.Progress(ctx, db, aave, tampered.ID)
	require.NoError(t, err)
	assert.False(t, progress[0].Unverified)
}

// TestSeals_ATargetChangedInSQLConvertsNothing points a converting
// migration at another currency in SQL (#451), and adds a source: the
// forced conversion and a user's own conversion convert nothing.
func TestSeals_ATargetChangedInSQLConvertsNothing(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	sealtest.Use(t, db, rowseal.ModeEnforce)
	h := holdings{t: t, db: db}
	mkr, sky, btc, eth := insertCurrency(t, db, "MKR"), insertCurrency(t, db, "SKY"), insertCurrency(t, db, "BTC"), insertCurrency(t, db, "ETH")
	alice := h.user()
	h.fund(alice, mkr, "2")
	h.fund(alice, eth, "1")
	start := time.Now()
	m, err := rebrand.Schedule(ctx, db, sky, plan(start, false, []rebrand.Source{{CurrencyHK: mkr, Ratio: "24000"}},
		rebrand.PhaseAnnounced, rebrand.PhaseConverting, rebrand.PhaseCompleted), origin())
	require.NoError(t, err)
	_, err = rebrand.Advance(ctx, db, start.Add(90*time.Minute), 10)
	require.NoError(t, err)

	_, err = db.Exec(`UPDATE currency_migrations SET target_hk = $2 WHERE hk = $1`, m.ID, btc)
	require.NoError(t, err)
	_, err = rebrand.ConvertOwn(ctx, db, m.ID, mkr, alice)
	assert.ErrorIs(t, err, rowseal.ErrUnverified)
	assert.Equal(t, [3]string{"2", "0", "0"}, h.balance(alice, mkr))
	assert.Equal(t, [3]string{"0", "0", "0"}, h.balance(alice, btc))
	_, err = db.Exec(`UPDATE currency_migrations SET target_hk = $2 WHERE hk = $1`, m.ID, sky)
	require.NoError(t, err)

	// A source added in SQL breaks the seal as well.
	_, err = db.Exec(`INSERT INTO currency_migration_sources (migration_hk, currency_hk, ratio) VALUES ($1, $2, 1000000)`, m.ID, eth)
	require.NoError(t, err)
	_, err = rebrand.ConvertOwn(ctx, db, m.ID, eth, alice)
	assert.ErrorIs(t, err, rowseal.ErrUnverified)
	assert.Equal(t, [3]string{"1", "0", "0"}, h.balance(alice, eth))
	assert.Len(t, openFindings(t, db, m.ID), 1, "the detections count on one finding")
}

// TestSeals_EveryWriteSealsTheMigration checks that the schedule, each
// phase, and the cancel leave a migration that matches its seal.
func TestSeals_EveryWriteSealsTheMigration(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	sealtest.Use(t, db, rowseal.ModeEnforce)
	lend, aave, gno, safe := insertCurrency(t, db, "LEND"), insertCurrency(t, db, "AAVE"), insertCurrency(t, db, "GNO"), insertCurrency(t, db, "SAFE")
	start := time.Now()
	m, err := rebrand.Schedule(ctx, db, aave, plan(start, true, []rebrand.Source{{CurrencyHK: lend, Ratio: "0.01"}}, allPhases...), origin())
	require.NoError(t, err)
	verify := func(why string, hks ...string) {
		t.Helper()
		_, err := rowseal.Active().VerifyAll(ctx)
		require.NoError(t, err)
		for _, hk := range hks {
			assert.Empty(t, openFindings(t, db, hk), why)
		}
	}
	verify("schedule", m.ID)
	_, err = rebrand.Advance(ctx, db, start.Add(30*time.Minute), 10)
	require.NoError(t, err)
	verify("a phase", m.ID)
	_, err = rebrand.Advance(ctx, db, start.Add(24*time.Hour), 10)
	require.NoError(t, err)
	assert.Equal(t, "completed", holdings{t: t, db: db}.phase(aave))
	verify("completed", m.ID)

	c, err := rebrand.Schedule(ctx, db, safe, plan(start, true, []rebrand.Source{{CurrencyHK: gno, Ratio: "2"}}, allPhases...), origin())
	require.NoError(t, err)
	_, _, err = rebrand.Cancel(ctx, db, safe, c.ID, nil, origin())
	require.NoError(t, err)
	verify("cancel", c.ID)
}

// TestSeals_AConvertingMigrationStopsAtARatioChangedInSQL raises the ratio
// of a forced migration that already converts (#451): the balance that
// becomes available afterwards does not convert at it, and another
// migration that converts goes on.
func TestSeals_AConvertingMigrationStopsAtARatioChangedInSQL(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	sealtest.Use(t, db, rowseal.ModeEnforce)
	h := holdings{t: t, db: db}
	lend, aave, agix, fet := insertCurrency(t, db, "LEND"), insertCurrency(t, db, "AAVE"), insertCurrency(t, db, "AGIX"), insertCurrency(t, db, "FET")
	start := time.Now()
	m, err := rebrand.Schedule(ctx, db, aave, plan(start, true, []rebrand.Source{{CurrencyHK: lend, Ratio: "2"}}, allPhases...), origin())
	require.NoError(t, err)
	_, err = rebrand.Schedule(ctx, db, fet, plan(start, true, []rebrand.Source{{CurrencyHK: agix, Ratio: "3"}}, allPhases...), origin())
	require.NoError(t, err)
	converting := start.Add(3*time.Hour + 30*time.Minute)
	_, err = rebrand.Advance(ctx, db, converting, 10)
	require.NoError(t, err)
	require.Equal(t, "converting", h.phase(aave))

	_, err = db.Exec(`UPDATE currency_migration_sources SET ratio = 200 WHERE migration_hk = $1`, m.ID)
	require.NoError(t, err)
	alice, bob := h.user(), h.user()
	h.fund(alice, lend, "10")
	h.fund(bob, agix, "10")
	_, err = rebrand.Advance(ctx, db, converting, 10)
	require.NoError(t, err)
	assert.Equal(t, [3]string{"10", "0", "0"}, h.balance(alice, lend), "nothing converts at the raised ratio")
	assert.Equal(t, [3]string{"0", "0", "0"}, h.balance(alice, aave))
	assert.Equal(t, [3]string{"30", "0", "0"}, h.balance(bob, fet), "the other migration converts")
	assert.Len(t, openFindings(t, db, m.ID), 1)
}

// TestSeals_AMigrationOfBeforeWaitsForTheVerifier schedules a migration
// before the rows are sealed, as before the release of #451: in report
// mode it converts nothing until the verifier seals it, and then converts.
func TestSeals_AMigrationOfBeforeWaitsForTheVerifier(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	h := holdings{t: t, db: db}
	lend, aave := insertCurrency(t, db, "LEND"), insertCurrency(t, db, "AAVE")
	start := time.Now()
	_, err := rebrand.Schedule(ctx, db, aave, plan(start, true, []rebrand.Source{{CurrencyHK: lend, Ratio: "2"}}, allPhases...), origin())
	require.NoError(t, err)
	sealer := sealtest.Use(t, db, rowseal.ModeReport)
	alice := h.user()
	h.fund(alice, lend, "10")
	_, err = rebrand.Advance(ctx, db, start.Add(24*time.Hour), 10)
	require.NoError(t, err)
	assert.Equal(t, [3]string{"10", "0", "0"}, h.balance(alice, lend), "an unsealed migration waits")
	assert.Equal(t, "scheduled", h.phase(aave))

	report, err := sealer.VerifyAll(ctx)
	require.NoError(t, err)
	assert.Positive(t, report.Resealed)
	for range 3 {
		_, err = rebrand.Advance(ctx, db, start.Add(24*time.Hour), 10)
		require.NoError(t, err)
	}
	assert.Equal(t, [3]string{"20", "0", "0"}, h.balance(alice, aave), "it converts once sealed")
}
