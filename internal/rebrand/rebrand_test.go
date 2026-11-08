package rebrand_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/rebrand"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDBName = "testdb_rebrand"

var ctx = context.Background()

func origin() audit.Event {
	return audit.Event{ActorType: audit.ActorAdmin, ActorID: uuid.NewString()}
}

func insertCurrency(t *testing.T, db *sql.DB, symbol string) string {
	t.Helper()
	// The backend stores symbols in upper case, and migration 36 makes the
	// database refuse any other (#126).
	symbol = strings.ToUpper(symbol)
	info, err := json.Marshal(models.CurrencyInfo{EnName: symbol, FaName: symbol, Symbol: symbol})
	require.NoError(t, err)
	var hk string
	require.NoError(t, db.QueryRow(`INSERT INTO currencies (status, info) VALUES ('approved', $1::jsonb) RETURNING hk`, info).Scan(&hk))
	return hk
}

func rename(db *sql.DB, hk, symbol string) error {
	_, err := db.Exec(`UPDATE currencies SET info = jsonb_set(info, '{symbol}', to_jsonb($2::text)) WHERE hk = $1`, hk, symbol)
	return err
}

var allPhases = []rebrand.Phase{rebrand.PhaseAnnounced, rebrand.PhaseDepositsPaused, rebrand.PhaseTradingHalted, rebrand.PhaseConverting, rebrand.PhaseCompleted}

func plan(start time.Time, forced bool, sources []rebrand.Source, phases ...rebrand.Phase) rebrand.Plan {
	p := rebrand.Plan{Sources: sources, Forced: forced}
	for i, ph := range phases {
		p.Steps = append(p.Steps, rebrand.Step{Phase: ph, At: start.Add(time.Duration(i) * time.Hour)})
	}
	return p
}

func details(t *testing.T, db *sql.DB, hk string) models.Currency {
	t.Helper()
	var c models.Currency
	var s currencies.DetailScan
	require.NoError(t, db.QueryRow(`SELECT `+currencies.DetailColumns+` FROM currencies c WHERE c.hk = $1`, hk).Scan(s.Dest()...))
	require.NoError(t, s.Apply(&c))
	return c
}

func TestPlan_Validate(t *testing.T) {
	now := time.Now()
	target := uuid.NewString()
	one := []rebrand.Source{{CurrencyHK: uuid.NewString(), Ratio: "1"}}
	for name, tc := range map[string]struct {
		edit  func(p *rebrand.Plan)
		field string
	}{
		"no sources":             {func(p *rebrand.Plan) { p.Sources = nil }, "sources"},
		"the target as a source": {func(p *rebrand.Plan) { p.Sources[0].CurrencyHK = target }, "sources"},
		"a zero ratio":           {func(p *rebrand.Plan) { p.Sources[0].Ratio = "0.000" }, "sources"},
		"a float ratio":          {func(p *rebrand.Plan) { p.Sources[0].Ratio = "1e-2" }, "sources"},
		"too precise a ratio":    {func(p *rebrand.Plan) { p.Sources[0].Ratio = "0.1234567890123456789" }, "sources"},
		"twice the same source": {func(p *rebrand.Plan) {
			p.Sources = append(p.Sources, rebrand.Source{CurrencyHK: p.Sources[0].CurrencyHK, Ratio: "2"})
		}, "sources"},
		"no announcement":            {func(p *rebrand.Plan) { p.Steps = p.Steps[1:] }, "steps"},
		"no completion":              {func(p *rebrand.Plan) { p.Steps = p.Steps[:4] }, "steps"},
		"out of order":               {func(p *rebrand.Plan) { p.Steps[1].Phase, p.Steps[2].Phase = p.Steps[2].Phase, p.Steps[1].Phase }, "steps"},
		"an optional one that halts": {func(p *rebrand.Plan) { p.Forced = false }, "steps"},
	} {
		p := plan(now.Add(time.Hour), true, append([]rebrand.Source(nil), one...), allPhases...)
		tc.edit(&p)
		assert.Contains(t, p.Validate(target, now), tc.field, name)
	}
	p := plan(now, false, []rebrand.Source{{CurrencyHK: uuid.NewString(), Ratio: "24000"}}, rebrand.PhaseAnnounced, rebrand.PhaseConverting, rebrand.PhaseCompleted)
	assert.Empty(t, p.Validate(target, now), "MKR to SKY: optional, 1 MKR = 24,000 SKY")
	merger := plan(now, true, []rebrand.Source{{CurrencyHK: uuid.NewString(), Ratio: "0.433350"}, {CurrencyHK: uuid.NewString(), Ratio: "0.433226"}},
		rebrand.PhaseAnnounced, rebrand.PhaseCompleted)
	assert.Empty(t, merger.Validate(target, now), "a merger of two sources")
}

// TestMigration_ForcedPhasesNarrowTheSources walks a forced rebrand, as
// MATIC to POL, through its phases.
func TestMigration_ForcedPhasesNarrowTheSources(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	matic, pol := insertCurrency(t, db, "MATIC"), insertCurrency(t, db, "POL")
	start := time.Now()
	m, err := rebrand.Schedule(ctx, db, pol, plan(start, true, []rebrand.Source{{CurrencyHK: matic, Ratio: "1.000"}}, allPhases...), origin())
	require.NoError(t, err)
	assert.Equal(t, rebrand.PhaseScheduled, m.Phase)
	assert.Equal(t, "1", m.Sources[0].Ratio)
	assert.Nil(t, details(t, db, matic).Migration, "nothing shows before the announcement")

	_, err = rebrand.Schedule(ctx, db, pol, plan(start, true, []rebrand.Source{{CurrencyHK: matic, Ratio: "1"}}, allPhases...), origin())
	assert.ErrorIs(t, err, rebrand.ErrSourceInMigration)
	_, err = rebrand.Schedule(ctx, db, insertCurrency(t, db, "X"), plan(start.Add(48*time.Hour), true, []rebrand.Source{{CurrencyHK: pol, Ratio: "1"}}, allPhases...), origin())
	assert.NoError(t, err, "the target may later be a source of its own migration")

	check := func(k currencies.Capability) error { return currencies.Check(ctx, db, matic, k, false) }
	for i, phase := range allPhases {
		changes, err := rebrand.Advance(ctx, db, start.Add(time.Duration(i)*time.Hour), 1)
		require.NoError(t, err)
		require.NotEmpty(t, changes, phase)
		c := details(t, db, matic)
		require.NotNil(t, c.Migration, phase)
		assert.Equal(t, string(phase), c.Migration.Phase)
		assert.Equal(t, pol, c.Migration.TargetHK)
		assert.True(t, c.Migration.Forced)
		switch phase {
		case rebrand.PhaseAnnounced:
			assert.NoError(t, check(currencies.CapabilityDeposit))
			require.NotNil(t, c.Migration.Next)
			assert.Equal(t, "deposits_paused", c.Migration.Next.Phase)
		case rebrand.PhaseDepositsPaused:
			var r *currencies.Refusal
			require.ErrorAs(t, check(currencies.CapabilityDeposit), &r)
			require.NotNil(t, r.Migration)
			assert.ErrorAs(t, check(currencies.CapabilityWithdraw), &r)
			assert.NoError(t, check(currencies.CapabilitySell))
		case rebrand.PhaseTradingHalted, rebrand.PhaseConverting:
			var r *currencies.Refusal
			assert.ErrorAs(t, check(currencies.CapabilitySell), &r)
			assert.True(t, c.Lifecycle.Capabilities.Listed)
		case rebrand.PhaseCompleted:
			assert.False(t, c.Lifecycle.Capabilities.Listed, "the source is retired")
			assert.Nil(t, c.Migration.Next)
		}
	}
	assert.NoError(t, currencies.Check(ctx, db, pol, currencies.CapabilityDeposit, false), "the target is untouched")
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM audit_events WHERE action = 'currency.migration_phase' AND target_id = $1`, pol).Scan(&n))
	assert.Equal(t, len(allPhases), n)
}

func TestMigration_AnOptionalOneLeavesTheSource(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	mkr, sky := insertCurrency(t, db, "MKR"), insertCurrency(t, db, "SKY")
	start := time.Now()
	_, err := rebrand.Schedule(ctx, db, sky, plan(start, false, []rebrand.Source{{CurrencyHK: mkr, Ratio: "24000"}},
		rebrand.PhaseAnnounced, rebrand.PhaseConverting, rebrand.PhaseCompleted), origin())
	require.NoError(t, err)
	_, err = rebrand.Advance(ctx, db, start.Add(24*time.Hour), 10)
	require.NoError(t, err)
	c := details(t, db, mkr)
	require.NotNil(t, c.Migration)
	assert.Equal(t, "completed", c.Migration.Phase)
	assert.Equal(t, "24000", c.Migration.Ratio)
	assert.Equal(t, currencies.StateCapabilities(currencies.LifecycleActive), c.Lifecycle.Capabilities, "the old token still trades")
}

func TestMigration_CancelRestoresTheSourcesUntilConverting(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	agix, ocean, fet := insertCurrency(t, db, "AGIX"), insertCurrency(t, db, "OCEAN"), insertCurrency(t, db, "FET")
	start := time.Now()
	sources := []rebrand.Source{{CurrencyHK: agix, Ratio: "0.433350"}, {CurrencyHK: ocean, Ratio: "0.433226"}}
	m, err := rebrand.Schedule(ctx, db, fet, plan(start, true, sources, allPhases...), origin())
	require.NoError(t, err)
	changes, err := rebrand.Advance(ctx, db, start.Add(2*time.Hour), 10)
	require.NoError(t, err)
	assert.Len(t, changes, 6, "three phases, each announced for both sources")
	assert.False(t, details(t, db, ocean).Lifecycle.Capabilities.Sell)

	stale := int64(1)
	_, _, err = rebrand.Cancel(ctx, db, fet, m.ID, &stale, origin())
	var conflict *rebrand.VersionConflict
	require.ErrorAs(t, err, &conflict)
	_, _, err = rebrand.Cancel(ctx, db, agix, m.ID, nil, origin())
	assert.ErrorIs(t, err, rebrand.ErrNotFound, "a migration is cancelled through its target")
	cancelled, changes, err := rebrand.Cancel(ctx, db, fet, m.ID, &conflict.Current, origin())
	require.NoError(t, err)
	assert.Equal(t, rebrand.PhaseCancelled, cancelled.Phase)
	assert.Len(t, changes, 2)
	for _, hk := range []string{agix, ocean} {
		c := details(t, db, hk)
		assert.Nil(t, c.Migration)
		assert.Equal(t, currencies.StateCapabilities(currencies.LifecycleActive), c.Lifecycle.Capabilities)
	}
	changes, err = rebrand.Advance(ctx, db, start.Add(24*time.Hour), 10)
	require.NoError(t, err)
	assert.Empty(t, changes)

	m, err = rebrand.Schedule(ctx, db, fet, plan(start, true, sources, allPhases...), origin())
	require.NoError(t, err, "the sources are free again")
	_, err = rebrand.Advance(ctx, db, start.Add(3*time.Hour), 10)
	require.NoError(t, err)
	_, _, err = rebrand.Cancel(ctx, db, fet, m.ID, nil, origin())
	assert.ErrorIs(t, err, rebrand.ErrIrreversible)
}

// TestMigration_EachPhaseAppliesOnceOnEveryInstance runs eight schedulers at
// once over migrations whose sources overlap their targets, and cancels
// between them.
func TestMigration_EachPhaseAppliesOnceOnEveryInstance(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	start := time.Now()
	var targets []string
	for range 4 {
		a, b, target := insertCurrency(t, db, "S"+uuid.NewString()[:6]), insertCurrency(t, db, "S"+uuid.NewString()[:6]), insertCurrency(t, db, "T"+uuid.NewString()[:6])
		_, err := rebrand.Schedule(ctx, db, target, plan(start, true, []rebrand.Source{{CurrencyHK: a, Ratio: "1"}, {CurrencyHK: b, Ratio: "2"}}, allPhases...), origin())
		require.NoError(t, err)
		targets = append(targets, target)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				changes, err := rebrand.Advance(ctx, db, start.Add(24*time.Hour), 2)
				if err != nil {
					t.Errorf("advance: %v", err)
					return
				}
				if len(changes) == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	for _, target := range targets {
		var n int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM audit_events WHERE action = 'currency.migration_phase' AND target_id = $1`, target).Scan(&n))
		assert.Equal(t, len(allPhases), n)
	}
}

// TestMigration_ALaterPhaseWaitsForThePhaseBefore holds the earliest phase
// of a migration as an instance that is applying it would: another instance
// must not apply a later phase first, which would leave the earlier one
// behind for good once the migration completes.
func TestMigration_ALaterPhaseWaitsForThePhaseBefore(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	start := time.Now()
	source, target := insertCurrency(t, db, "WS"), insertCurrency(t, db, "WT")
	m, err := rebrand.Schedule(ctx, db, target, plan(start, true, []rebrand.Source{{CurrencyHK: source, Ratio: "1"}}, allPhases...), origin())
	require.NoError(t, err)
	held, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer held.Rollback()
	var phase string
	require.NoError(t, held.QueryRowContext(ctx, `SELECT phase FROM currency_migration_phases WHERE migration_hk = $1
		ORDER BY scheduled_at LIMIT 1 FOR UPDATE`, m.ID).Scan(&phase))

	_, err = rebrand.Advance(ctx, db, start.Add(24*time.Hour), 10)
	require.NoError(t, err)
	assert.Zero(t, phaseAudits(t, db, target), "no phase applies while the one before is being applied")
	require.NoError(t, held.Rollback())

	_, err = rebrand.Advance(ctx, db, start.Add(24*time.Hour), 10)
	require.NoError(t, err)
	assert.Equal(t, len(allPhases), phaseAudits(t, db, target))
}

func phaseAudits(t *testing.T, db *sql.DB, target string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM audit_events WHERE action = 'currency.migration_phase' AND target_id = $1`, target).Scan(&n))
	return n
}

func TestMigration_OnlyApprovedCurrencies(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	a, target := insertCurrency(t, db, "AA"), insertCurrency(t, db, "BB")
	_, err := db.Exec(`UPDATE currencies SET status = 'pending' WHERE hk = $1`, a)
	require.NoError(t, err)
	start := time.Now()
	_, err = rebrand.Schedule(ctx, db, target, plan(start, true, []rebrand.Source{{CurrencyHK: a, Ratio: "1"}}, allPhases...), origin())
	assert.ErrorIs(t, err, rebrand.ErrSourceNotListed)
	_, err = rebrand.Schedule(ctx, db, a, plan(start, true, []rebrand.Source{{CurrencyHK: target, Ratio: "1"}}, allPhases...), origin())
	assert.ErrorIs(t, err, rebrand.ErrTargetNotListed)
}

// TestSymbols_APreviousSymbolStaysReserved: a rebrand keeps the symbol before
// for the currency, and only a release lets another currency take it.
func TestSymbols_APreviousSymbolStaysReserved(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	luna := insertCurrency(t, db, "LUNA")
	require.NoError(t, rename(db, luna, "LUNC"))
	assert.Equal(t, []string{"LUNA"}, details(t, db, luna).PreviousSymbols)

	info, err := json.Marshal(models.CurrencyInfo{EnName: "Terra", FaName: "Terra", Symbol: "LUNA"})
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO currencies (status, info) VALUES ('approved', $1::jsonb)`, info)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23505", pgErr.Code)
	assert.Equal(t, "currency_symbols_reserved", pgErr.ConstraintName)
	other := insertCurrency(t, db, "OTHER")
	require.ErrorAs(t, rename(db, other, "LUNA"), &pgErr, "a rename cannot take it either")

	// A currency takes back its own symbol.
	require.NoError(t, rename(db, luna, "LUNA"))
	assert.Equal(t, []string{"LUNC"}, details(t, db, luna).PreviousSymbols)
	require.NoError(t, rename(db, luna, "LUNC"))
	_, err = db.Exec(`DELETE FROM currency_symbols WHERE symbol = 'LUNA' AND NOT current`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO currencies (status, info) VALUES ('approved', $1::jsonb)`, info)
	assert.NoError(t, err, "a released symbol can be taken")
}

// TestSymbols_ConcurrentRenamesAndCreatesNeverShareASymbol renames a currency
// away from a symbol while other instances create currencies with it.
func TestSymbols_ConcurrentRenamesAndCreatesNeverShareASymbol(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	for round := range 10 {
		old := "OLD" + string(rune('A'+round))
		hk := insertCurrency(t, db, old)
		info, err := json.Marshal(models.CurrencyInfo{EnName: "x", FaName: "x", Symbol: old})
		require.NoError(t, err)
		var wg sync.WaitGroup
		wg.Add(4)
		go func() { defer wg.Done(); _ = rename(db, hk, "NEW"+string(rune('A'+round))) }()
		for range 3 {
			go func() {
				defer wg.Done()
				_, _ = db.Exec(`INSERT INTO currencies (status, info) VALUES ('approved', $1::jsonb)`, info)
			}()
		}
		wg.Wait()
		var holders int
		require.NoError(t, db.QueryRow(`SELECT count(DISTINCT c.hk) FROM currencies c LEFT JOIN currency_symbols s ON s.currency_hk = c.hk
			WHERE c.info->>'symbol' = $1 OR (s.symbol = $1)`, old).Scan(&holders))
		assert.Equal(t, 1, holders, "the symbol %s names one currency", old)
	}
}
