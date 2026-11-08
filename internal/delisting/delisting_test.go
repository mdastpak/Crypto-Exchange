package delisting_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/delisting"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDBName = "testdb_delisting"

var ctx = context.Background()

func origin() audit.Event {
	return audit.Event{ActorType: audit.ActorAdmin, ActorID: uuid.NewString()}
}

func insertCurrency(t *testing.T, db *sql.DB, symbol, status string) string {
	t.Helper()
	info, err := json.Marshal(models.CurrencyInfo{EnName: symbol, FaName: symbol, Symbol: symbol})
	require.NoError(t, err)
	var hk string
	require.NoError(t, db.QueryRow(`INSERT INTO currencies (status, info) VALUES ($1, $2::jsonb) RETURNING hk`, status, info).Scan(&hk))
	return hk
}

// plan is a standard delisting with a claim, whose steps start at start and
// follow each other by an hour.
func plan(start time.Time, states ...currencies.Lifecycle) delisting.Plan {
	p := delisting.Plan{Kind: delisting.KindStandard, Settlement: delisting.SettlementClaim, ClaimDays: 180}
	for i, s := range states {
		p.Steps = append(p.Steps, delisting.Step{State: s, At: start.Add(time.Duration(i) * time.Hour)})
	}
	return p
}

var fullDelisting = []currencies.Lifecycle{
	currencies.LifecycleDelistAnnounced, currencies.LifecycleDepositsDisabled, currencies.LifecycleSellOnly,
	currencies.LifecycleTradingHalted, currencies.LifecycleWithdrawalOnly, currencies.LifecycleFrozen, currencies.LifecycleDelisted,
}

func lifecycle(t *testing.T, db *sql.DB, hk string) *models.CurrencyLifecycle {
	t.Helper()
	l, err := currencies.ReadLifecycle(ctx, db, hk, false)
	require.NoError(t, err)
	return l
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(query, args...).Scan(&n))
	return n
}

func TestPlan_Validate(t *testing.T) {
	now := time.Now()
	for name, tc := range map[string]struct {
		edit  func(p *delisting.Plan)
		field string
	}{
		"no steps":                   {func(p *delisting.Plan) { p.Steps = nil }, "steps"},
		"out of order":               {func(p *delisting.Plan) { p.Steps[0], p.Steps[1] = p.Steps[1], p.Steps[0] }, "steps"},
		"the same time twice":        {func(p *delisting.Plan) { p.Steps[1].At = p.Steps[0].At }, "steps"},
		"in the past":                {func(p *delisting.Plan) { p.Steps[0].At = now.Add(-time.Hour) }, "steps"},
		"active is not a step":       {func(p *delisting.Plan) { p.Steps[0].State = currencies.LifecycleActive }, "steps"},
		"standard skips the notice":  {func(p *delisting.Plan) { p.Steps = p.Steps[1:] }, "steps"},
		"emergency starts too early": {func(p *delisting.Plan) { p.Kind = delisting.KindEmergency }, "steps"},
		"unknown kind":               {func(p *delisting.Plan) { p.Kind = "soon" }, "kind"},
		"claim without days":         {func(p *delisting.Plan) { p.ClaimDays = 0 }, "claim_days"},
		"convert without a currency": {func(p *delisting.Plan) {
			p.Settlement, p.ClaimDays, p.PriceSource = delisting.SettlementConvert, 0, delisting.PriceTWAP
		}, "settlement_currency_hk"},
		"fixed without a price": {func(p *delisting.Plan) {
			p.Settlement, p.ClaimDays, p.PriceSource, p.SettlementCurrencyHK = delisting.SettlementConvert, 0, delisting.PriceFixed, uuid.NewString()
		}, "fixed_price"},
		"a price that is not a decimal": {func(p *delisting.Plan) {
			p.Settlement, p.ClaimDays, p.PriceSource, p.SettlementCurrencyHK, p.FixedPrice = delisting.SettlementConvert, 0, delisting.PriceFixed, uuid.NewString(), "1e3"
		}, "fixed_price"},
		"an http announcement": {func(p *delisting.Plan) { p.AnnouncementURL = "http://example.com/a" }, "announcement_url"},
	} {
		p := plan(now.Add(time.Hour), fullDelisting...)
		tc.edit(&p)
		assert.Contains(t, p.Validate(now), tc.field, name)
	}

	p := plan(now.Add(time.Hour), fullDelisting...)
	assert.Empty(t, p.Validate(now))
	emergency := plan(now, currencies.LifecycleTradingHalted, currencies.LifecycleFrozen)
	emergency.Kind = delisting.KindEmergency
	emergency.Settlement, emergency.ClaimDays, emergency.PriceSource, emergency.FixedPrice = delisting.SettlementConvert, 0, delisting.PriceFixed, "0.0125"
	emergency.SettlementCurrencyHK = uuid.NewString()
	assert.Empty(t, emergency.Validate(now), "an emergency delisting can start now and leave steps out")
}

func TestDelisting_StepsApplyInOrderAndChangeTheCapabilities(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	hk := insertCurrency(t, db, "OLD", "approved")
	start := time.Now().Add(time.Hour)
	d, change, err := delisting.Schedule(ctx, db, hk, plan(start, fullDelisting...), origin())
	require.NoError(t, err)
	assert.True(t, change.Public)
	assert.Equal(t, delisting.StatusScheduled, d.Status)
	require.Len(t, d.Steps, len(fullDelisting))

	l := lifecycle(t, db, hk)
	assert.Equal(t, "active", l.State)
	require.NotNil(t, l.Next, "the next step is shown")
	assert.Equal(t, "delist_announced", l.Next.State)
	assert.WithinDuration(t, start, l.Next.At, time.Second)

	_, _, err = delisting.Schedule(ctx, db, hk, plan(start, fullDelisting...), origin())
	assert.ErrorIs(t, err, delisting.ErrInProgress, "one delisting at a time")

	changes, err := delisting.Advance(ctx, db, start.Add(-time.Minute), 10)
	require.NoError(t, err)
	assert.Empty(t, changes, "nothing is due yet")

	want := map[currencies.Lifecycle]func(c models.CurrencyCapabilities) bool{
		currencies.LifecycleDelistAnnounced:  func(c models.CurrencyCapabilities) bool { return c.Deposit && c.Buy && c.Sell && c.Withdraw },
		currencies.LifecycleDepositsDisabled: func(c models.CurrencyCapabilities) bool { return !c.Deposit && c.Buy && c.Sell },
		currencies.LifecycleSellOnly:         func(c models.CurrencyCapabilities) bool { return !c.Buy && !c.ConvertInto && c.Sell && c.ConvertFrom },
		currencies.LifecycleTradingHalted:    func(c models.CurrencyCapabilities) bool { return !c.Sell && !c.ConvertFrom && c.Withdraw && c.Listed },
		currencies.LifecycleWithdrawalOnly:   func(c models.CurrencyCapabilities) bool { return c.Withdraw && !c.Deposit },
		currencies.LifecycleFrozen:           func(c models.CurrencyCapabilities) bool { return !c.Withdraw && c.Listed },
		currencies.LifecycleDelisted:         func(c models.CurrencyCapabilities) bool { return !c.Withdraw && !c.Listed },
	}
	for i, state := range fullDelisting {
		changes, err := delisting.Advance(ctx, db, start.Add(time.Duration(i)*time.Hour), 10)
		require.NoError(t, err)
		require.Len(t, changes, 1, state)
		l := lifecycle(t, db, hk)
		assert.Equal(t, string(state), l.State)
		assert.True(t, want[state](l.Capabilities), "the capabilities of %s: %+v", state, l.Capabilities)
	}
	list, err := delisting.List(ctx, db, hk)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, delisting.StatusCompleted, list[0].Status)
	assert.Nil(t, lifecycle(t, db, hk).Next)
	for _, s := range list[0].Steps {
		assert.NotNil(t, s.AppliedAt, s.State)
	}
	assert.Equal(t, len(fullDelisting), count(t, db, `SELECT count(*) FROM audit_events WHERE action = 'currency.lifecycle_change' AND target_id = $1`, hk))
	assert.Equal(t, len(fullDelisting)+1, count(t, db, `SELECT count(*) FROM outbox_events WHERE topic = 'currencies'`),
		"the schedule and each step announce the currency")

	err = currencies.Check(ctx, db, hk, currencies.CapabilityWithdraw, false)
	var refusal *currencies.Refusal
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, currencies.CapabilityWithdraw, refusal.Capability)
	assert.Equal(t, "delisted", refusal.Lifecycle.State)
}

func TestDelisting_StepsThatAreAllDueApplyInOrder(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	hk := insertCurrency(t, db, "LATE", "approved")
	start := time.Now()
	_, _, err := delisting.Schedule(ctx, db, hk, plan(start, fullDelisting[:4]...), origin())
	require.NoError(t, err)
	// The instances were down for the whole schedule.
	changes, err := delisting.Advance(ctx, db, start.Add(24*time.Hour), 2)
	require.NoError(t, err)
	assert.Len(t, changes, 2, "limit bounds a run")
	assert.Equal(t, "deposits_disabled", lifecycle(t, db, hk).State)
	_, err = delisting.Advance(ctx, db, start.Add(24*time.Hour), 10)
	require.NoError(t, err)
	assert.Equal(t, "trading_halted", lifecycle(t, db, hk).State)
}

// TestDelisting_EachStepAppliesOnceOnEveryInstance runs the scheduler of
// eight instances at once over a backlog of steps of several currencies.
func TestDelisting_EachStepAppliesOnceOnEveryInstance(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	start := time.Now()
	var hks []string
	for _, s := range []string{"A1", "A2", "A3", "A4"} {
		hk := insertCurrency(t, db, s, "approved")
		_, _, err := delisting.Schedule(ctx, db, hk, plan(start, fullDelisting...), origin())
		require.NoError(t, err)
		hks = append(hks, hk)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	applied := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				changes, err := delisting.Advance(ctx, db, start.Add(24*time.Hour), 3)
				if err != nil {
					t.Errorf("advance: %v", err)
					return
				}
				mu.Lock()
				applied += len(changes)
				mu.Unlock()
				if len(changes) == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, len(hks)*len(fullDelisting), applied)
	for _, hk := range hks {
		assert.Equal(t, "delisted", lifecycle(t, db, hk).State)
		assert.Equal(t, len(fullDelisting), count(t, db, `SELECT count(*) FROM audit_events WHERE action = 'currency.lifecycle_change' AND target_id = $1`, hk))
	}
}

// TestDelisting_ALaterStepWaitsForTheStepBefore holds the earliest step of
// a delisting as an instance that is applying it would: another instance
// must not apply a later step first, which would leave the earlier one
// behind for good once the delisting completes.
func TestDelisting_ALaterStepWaitsForTheStepBefore(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	start := time.Now()
	hk := insertCurrency(t, db, "WT", "approved")
	_, _, err := delisting.Schedule(ctx, db, hk, plan(start, fullDelisting...), origin())
	require.NoError(t, err)
	held, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer held.Rollback()
	var state string
	require.NoError(t, held.QueryRowContext(ctx, `SELECT s.state FROM currency_delisting_steps s JOIN currency_delistings d ON d.hk = s.delisting_hk
		WHERE d.currency_hk = $1 ORDER BY s.scheduled_at LIMIT 1 FOR UPDATE OF s`, hk).Scan(&state))

	changes, err := delisting.Advance(ctx, db, start.Add(24*time.Hour), 10)
	require.NoError(t, err)
	assert.Empty(t, changes, "no step applies while the one before is being applied")
	require.NoError(t, held.Rollback())

	changes, err = delisting.Advance(ctx, db, start.Add(24*time.Hour), 10)
	require.NoError(t, err)
	assert.Len(t, changes, len(fullDelisting))
	assert.Equal(t, "delisted", lifecycle(t, db, hk).State)
}

func TestDelisting_CancelBeforeFrozenRestoresEverything(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	hk := insertCurrency(t, db, "BACK", "approved")
	start := time.Now()
	d, _, err := delisting.Schedule(ctx, db, hk, plan(start, fullDelisting...), origin())
	require.NoError(t, err)
	_, err = delisting.Advance(ctx, db, start.Add(3*time.Hour), 10)
	require.NoError(t, err)
	assert.Equal(t, "trading_halted", lifecycle(t, db, hk).State)

	stale := d.Version + 5
	_, _, err = delisting.Cancel(ctx, db, hk, d.ID, &stale, origin())
	var conflict *delisting.VersionConflict
	require.ErrorAs(t, err, &conflict)
	_, _, err = delisting.Cancel(ctx, db, uuid.NewString(), d.ID, nil, origin())
	assert.ErrorIs(t, err, delisting.ErrNotFound, "the delisting of another currency")

	cancelled, change, err := delisting.Cancel(ctx, db, hk, d.ID, &d.Version, origin())
	require.NoError(t, err)
	assert.True(t, change.Public)
	assert.Equal(t, delisting.StatusCancelled, cancelled.Status)
	l := lifecycle(t, db, hk)
	assert.Equal(t, "active", l.State)
	assert.Equal(t, currencies.StateCapabilities(currencies.LifecycleActive), l.Capabilities)
	assert.Nil(t, l.Next)
	changes, err := delisting.Advance(ctx, db, start.Add(24*time.Hour), 10)
	require.NoError(t, err)
	assert.Empty(t, changes, "the steps that were left are not applied")
	_, _, err = delisting.Cancel(ctx, db, hk, d.ID, nil, origin())
	assert.ErrorIs(t, err, delisting.ErrNotScheduled)

	// A new delisting can follow, and cannot be cancelled from frozen on.
	d, _, err = delisting.Schedule(ctx, db, hk, plan(start, fullDelisting...), origin())
	require.NoError(t, err)
	_, err = delisting.Advance(ctx, db, start.Add(5*time.Hour), 10)
	require.NoError(t, err)
	assert.Equal(t, "frozen", lifecycle(t, db, hk).State)
	_, _, err = delisting.Cancel(ctx, db, hk, d.ID, nil, origin())
	assert.ErrorIs(t, err, delisting.ErrIrreversible)
}

func TestDelisting_OnlyApprovedCurrenciesAndSettlementCurrencies(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	pending := insertCurrency(t, db, "PEN", "pending")
	hk := insertCurrency(t, db, "X", "approved")
	start := time.Now().Add(time.Hour)
	_, _, err := delisting.Schedule(ctx, db, pending, plan(start, fullDelisting...), origin())
	assert.ErrorIs(t, err, delisting.ErrNotListed)
	_, _, err = delisting.Schedule(ctx, db, uuid.NewString(), plan(start, fullDelisting...), origin())
	assert.ErrorIs(t, err, delisting.ErrCurrencyNotFound)

	convert := plan(start, fullDelisting...)
	convert.Settlement, convert.ClaimDays, convert.PriceSource, convert.FixedPrice = delisting.SettlementConvert, 0, delisting.PriceFixed, "0.012500"
	for _, target := range []string{pending, hk, uuid.NewString()} {
		convert.SettlementCurrencyHK = target
		_, _, err = delisting.Schedule(ctx, db, hk, convert, origin())
		assert.ErrorIs(t, err, delisting.ErrSettlementCurrency)
	}
	convert.SettlementCurrencyHK = insertCurrency(t, db, "USDT", "approved")
	d, _, err := delisting.Schedule(ctx, db, hk, convert, origin())
	require.NoError(t, err)
	require.NotNil(t, d.FixedPrice)
	assert.Equal(t, "0.0125", *d.FixedPrice)
}

func TestOverride_SetsAndRemovesACapability(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	hk := insertCurrency(t, db, "OVR", "approved")
	refused, allowed := false, true
	l, change, err := delisting.Override(ctx, db, hk, currencies.CapabilityDeposit, &refused, "incident", origin())
	require.NoError(t, err)
	assert.True(t, change.Public)
	assert.False(t, l.Capabilities.Deposit)
	assert.True(t, l.Capabilities.Withdraw)
	var refusal *currencies.Refusal
	require.ErrorAs(t, currencies.Check(ctx, db, hk, currencies.CapabilityDeposit, false), &refusal)
	assert.Equal(t, "active", refusal.Lifecycle.State)
	assert.NoError(t, currencies.Check(ctx, db, hk, currencies.CapabilityWithdraw, false))

	// An override wins over the state, either way.
	start := time.Now()
	_, _, err = delisting.Schedule(ctx, db, hk, plan(start, fullDelisting[:6]...), origin())
	require.NoError(t, err)
	_, err = delisting.Advance(ctx, db, start.Add(24*time.Hour), 10)
	require.NoError(t, err)
	assert.ErrorAs(t, currencies.Check(ctx, db, hk, currencies.CapabilityWithdraw, false), &refusal, "frozen")
	_, _, err = delisting.Override(ctx, db, hk, currencies.CapabilityWithdraw, &allowed, "", origin())
	require.NoError(t, err)
	assert.NoError(t, currencies.Check(ctx, db, hk, currencies.CapabilityWithdraw, false), "withdrawals kept open at frozen")

	l, _, err = delisting.Override(ctx, db, hk, currencies.CapabilityWithdraw, nil, "", origin())
	require.NoError(t, err)
	assert.False(t, l.Capabilities.Withdraw)
	assert.Equal(t, 3, count(t, db, `SELECT count(*) FROM audit_events WHERE action = 'currency.capability_override' AND target_id = $1`, hk))

	_, _, err = delisting.Override(ctx, db, uuid.NewString(), currencies.CapabilityWithdraw, &allowed, "", origin())
	assert.ErrorIs(t, err, delisting.ErrCurrencyNotFound)
	assert.ErrorIs(t, currencies.Check(ctx, db, uuid.NewString(), currencies.CapabilityWithdraw, false), currencies.ErrNotFound)
}

// TestCheck_AStepWaitsForAFlowThatChecked: a flow that checked a capability
// with lock, in its transaction, finishes before a step can change it, on
// any instance.
func TestCheck_AStepWaitsForAFlowThatChecked(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	hk := insertCurrency(t, db, "LOCK", "approved")
	start := time.Now()
	_, _, err := delisting.Schedule(ctx, db, hk, plan(start, fullDelisting[:2]...), origin())
	require.NoError(t, err)
	_, err = delisting.Advance(ctx, db, start, 10)
	require.NoError(t, err)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.NoError(t, currencies.Check(ctx, tx, hk, currencies.CapabilityDeposit, true))
	done := make(chan error, 1)
	go func() {
		_, err := delisting.Advance(ctx, db, start.Add(2*time.Hour), 10)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the step did not wait for the flow: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	assert.Equal(t, "delist_announced", lifecycle(t, db, hk).State)
	require.NoError(t, tx.Commit())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the step never applied")
	}
	assert.Equal(t, "deposits_disabled", lifecycle(t, db, hk).State)
}

func TestScheduler_RunsTheStepsThatAreDue(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	hk := insertCurrency(t, db, "RUN", "approved")
	_, _, err := delisting.Schedule(ctx, db, hk, plan(time.Now(), fullDelisting[0]), origin())
	require.NoError(t, err)
	got := make(chan []currencies.Change, 1)
	s := currencies.NewScheduler(db, 50*time.Millisecond, func(_ context.Context, changes []currencies.Change) { got <- changes }, delisting.Advance)
	s.Start()
	defer func() { require.NoError(t, s.Stop(ctx)) }()
	select {
	case changes := <-got:
		require.Len(t, changes, 1)
		assert.Equal(t, hk, changes[0].CurrencyHK)
	case <-time.After(5 * time.Second):
		t.Fatal("the scheduler did not apply the step")
	}
	assert.Equal(t, "delist_announced", lifecycle(t, db, hk).State)
}

// TestScheduler_APauseHoldsTheSteps pauses the scheduler, as a lockdown of
// the scheduler does (#235), and expects a due step to wait for the end of
// the pause.
func TestScheduler_APauseHoldsTheSteps(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	hk := insertCurrency(t, db, "PAUSE", "approved")
	_, _, err := delisting.Schedule(ctx, db, hk, plan(time.Now(), fullDelisting[0]), origin())
	require.NoError(t, err)
	got := make(chan []currencies.Change, 1)
	s := currencies.NewScheduler(db, 50*time.Millisecond, func(_ context.Context, changes []currencies.Change) { got <- changes }, delisting.Advance)
	var paused atomic.Bool
	paused.Store(true)
	s.Paused = paused.Load
	s.Start()
	defer func() { require.NoError(t, s.Stop(ctx)) }()
	select {
	case <-got:
		t.Fatal("a paused scheduler applied a step")
	case <-time.After(300 * time.Millisecond):
	}
	paused.Store(false)
	select {
	case changes := <-got:
		require.Len(t, changes, 1)
	case <-time.After(5 * time.Second):
		t.Fatal("the scheduler did not apply the step after the pause")
	}
}

// TestDelisting_AnEmergencyHaltCanBeCancelledOrTakenFurther: a delisting
// that stops before delisted stays scheduled.
func TestDelisting_AnEmergencyHaltCanBeCancelledOrTakenFurther(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	hk := insertCurrency(t, db, "HALT", "approved")
	start := time.Now()
	halt := plan(start, currencies.LifecycleTradingHalted)
	halt.Kind = delisting.KindEmergency
	d, _, err := delisting.Schedule(ctx, db, hk, halt, origin())
	require.NoError(t, err)
	_, err = delisting.Advance(ctx, db, start, 10)
	require.NoError(t, err)
	list, err := delisting.List(ctx, db, hk)
	require.NoError(t, err)
	assert.Equal(t, delisting.StatusScheduled, list[0].Status, "a halt does not complete")

	// The investigation ends in a delisting, from where the currency is.
	_, _, err = delisting.Schedule(ctx, db, hk, halt, origin())
	assert.ErrorIs(t, err, delisting.ErrStepsBehind)
	further := plan(start.Add(time.Minute), currencies.LifecycleWithdrawalOnly, currencies.LifecycleDelisted)
	further.Kind = delisting.KindEmergency
	next, _, err := delisting.Schedule(ctx, db, hk, further, origin())
	require.NoError(t, err)
	list, err = delisting.List(ctx, db, hk)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, delisting.StatusCompleted, list[1].Status, "the halt is superseded")
	_, _, err = delisting.Cancel(ctx, db, hk, d.ID, nil, origin())
	assert.ErrorIs(t, err, delisting.ErrNotScheduled)

	// Or it ends in a relisting.
	cancelled, _, err := delisting.Cancel(ctx, db, hk, next.ID, nil, origin())
	require.NoError(t, err)
	assert.Equal(t, delisting.StatusCancelled, cancelled.Status)
	assert.Equal(t, "active", lifecycle(t, db, hk).State)
}

// TestDelisting_CancelsAndStepsOnManyInstancesDoNotDeadlock: every change
// takes its locks in one order, so cancels, schedules, and steps at once
// all finish.
func TestDelisting_CancelsAndStepsOnManyInstancesDoNotDeadlock(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	start := time.Now()
	type pair struct{ currency, delisting string }
	var all []pair
	for i := range 12 {
		hk := insertCurrency(t, db, "D"+string(rune('A'+i)), "approved")
		d, _, err := delisting.Schedule(ctx, db, hk, plan(start, fullDelisting[:4]...), origin())
		require.NoError(t, err)
		all = append(all, pair{hk, d.ID})
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				if _, err := delisting.Advance(ctx, db, start.Add(24*time.Hour), 3); err != nil {
					t.Errorf("advance: %v", err)
				}
			}
		}()
	}
	for _, p := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := delisting.Cancel(ctx, db, p.currency, p.delisting, nil, origin())
			if err != nil && !errors.Is(err, delisting.ErrNotScheduled) {
				t.Errorf("cancel: %v", err)
			}
		}()
	}
	wg.Wait()
}
