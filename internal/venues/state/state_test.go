package state

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/jobs"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"
	"github.com/mdastpak/crypto-exchange/internal/venues"
	"github.com/mdastpak/crypto-exchange/internal/venues/stub"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dec(s string) shared.Decimal { return shared.MustNewDecimalFromString(s) }

// fixture is a store on a migrated database, with row seals in enforce
// mode, a clock the test moves, and the currency USDT.
type fixture struct {
	*Store
	db    *sql.DB
	clock time.Time
	usdt  shared.UUID
}

var mapping = venues.Mapping{"USDT": {Currency: "USDT", Networks: map[string]string{"TRX": "tron"}}}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := pgtest.Migrated(t, "testdb_venue_state")
	db.SetMaxOpenConns(20)
	sealtest.Use(t, db, rowseal.ModeEnforce)
	f := &fixture{db: db, clock: time.Now()}
	f.Store = New(db, Options{MaxAge: time.Minute, ConfirmRuns: 2, Tolerances: map[string]shared.Decimal{"USDT": dec("0.01")}})
	f.Now = func() time.Time { return f.clock }
	require.NoError(t, db.QueryRow(`INSERT INTO currencies (status, info)
		VALUES ('approved', jsonb_build_object('symbol', 'USDT', 'en_name', 'USDT', 'fa_name', 'USDT',
			'if_fiat', false, 'is_token', true, 'contract', '')) RETURNING hk`).Scan(&f.usdt))
	return f
}

func (f *fixture) balance(t *testing.T, free string) {
	t.Helper()
	require.NoError(t, f.SaveBalances(context.Background(), "binance",
		[]venues.Balance{{Asset: "USDT", Free: dec(free), Locked: dec("0")}}, f.clock))
}

// deposit books a deposit of ours to binance, so that the books say it
// holds value more, and moves the clock past the entry.
func (f *fixture) deposit(t *testing.T, key, value string) {
	t.Helper()
	_, err := f.Ledger.VenueDeposit(context.Background(), key, "binance", ledger.Amount{Currency: f.usdt, Value: dec(value)},
		ledger.Ref{Type: "venue_deposit", ID: key})
	require.NoError(t, err)
	f.clock = time.Now().Add(time.Second)
}

func (f *fixture) entries(t *testing.T, kind string) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM ledger_entries WHERE kind = $1`, kind).Scan(&n))
	return n
}

// TestReserve_ConcurrentRoutingsNeverOverspend covers the first criterion
// of #385: routings in parallel transactions against one venue balance
// never reserve more than it holds.
func TestReserve_ConcurrentRoutingsNeverOverspend(t *testing.T) {
	f := newFixture(t)
	f.balance(t, "100")

	const routings = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, short := 0, 0
	start := make(chan struct{})
	for i := 0; i < routings; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := f.Reserve(context.Background(), Reservation{Venue: "binance", Asset: "USDT", Amount: dec("30"),
				ReferenceType: "route", ReferenceID: string(rune('a' + i))})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrInsufficient):
				short++
			default:
				t.Errorf("reserve: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	assert.Equal(t, 3, won, "100 covers three reservations of 30")
	assert.Equal(t, routings-3, short)
	available, err := f.Available(context.Background(), "binance", "USDT")
	require.NoError(t, err)
	assert.Equal(t, "10", available.String())
}

// TestReserve_ReferencesAndRelease covers the life of a reservation: a
// reference reserves once, a release frees its amount, and a reservation
// changed in SQL is refused.
func TestReserve_ReferencesAndRelease(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.balance(t, "100")
	r := Reservation{Venue: "binance", Asset: "USDT", Amount: dec("60"), ReferenceType: "route", ReferenceID: "1"}
	first, err := f.Reserve(ctx, r)
	require.NoError(t, err)
	again, err := f.Reserve(ctx, r)
	require.NoError(t, err)
	assert.Equal(t, first.ID, again.ID, "the same reference reserves once")
	r.Amount = dec("61")
	_, err = f.Reserve(ctx, r)
	require.ErrorIs(t, err, ErrReferenceReused)

	require.NoError(t, f.Release(ctx, first.ID))
	require.NoError(t, f.Release(ctx, first.ID), "a release is idempotent")
	available, err := f.Available(ctx, "binance", "USDT")
	require.NoError(t, err)
	assert.Equal(t, "100", available.String())
	r.Amount = dec("60")
	_, err = f.Reserve(ctx, r)
	require.ErrorIs(t, err, ErrReferenceReused, "a released reference does not reserve again")

	second, err := f.Reserve(ctx, Reservation{Venue: "binance", Asset: "USDT", Amount: dec("50"), ReferenceType: "route", ReferenceID: "2"})
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE venue_reservations SET amount = 1 WHERE id = $1`, second.ID)
	require.NoError(t, err)
	_, err = f.Reserve(ctx, Reservation{Venue: "binance", Asset: "USDT", Amount: dec("90"), ReferenceType: "route", ReferenceID: "3"})
	require.ErrorIs(t, err, rowseal.ErrUnverified, "a reservation lowered in SQL lets no plan through")

	_, err = f.Reserve(ctx, Reservation{Venue: "binance", Asset: "BTC", Amount: dec("1"), ReferenceType: "route", ReferenceID: "4"})
	require.ErrorIs(t, err, ErrNotRoutable, "no balance at the venue")
}

// TestRoutable_AStaleSnapshotIsNotRoutable covers the second criterion: a
// balance older than the cutoff is unknown.
func TestRoutable_AStaleSnapshotIsNotRoutable(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.balance(t, "100")
	r, err := f.Routable(ctx, "binance", "USDT", "")
	require.NoError(t, err)
	assert.True(t, r.Routable)

	f.clock = f.clock.Add(2 * time.Minute)
	r, err = f.Routable(ctx, "binance", "USDT", "")
	require.NoError(t, err)
	assert.False(t, r.Routable)
	assert.Equal(t, []string{ReasonStaleBalance}, r.Reasons)
	_, err = f.Reserve(ctx, Reservation{Venue: "binance", Asset: "USDT", Amount: dec("1"), ReferenceType: "route", ReferenceID: "1"})
	require.ErrorIs(t, err, ErrNotRoutable)
}

// TestRoutable_ASuspendedNetworkIsNotRoutable covers the second criterion
// for the networks, and that a network the venue no longer reports is
// suspended, not deleted.
func TestRoutable_ASuspendedNetworkIsNotRoutable(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.balance(t, "100")
	require.NoError(t, f.SaveNetworks(ctx, "binance", map[string][]venues.Network{"USDT": {
		{Network: "TRX", DepositEnabled: true, WithdrawEnabled: true},
		{Network: "BSC", DepositEnabled: true, WithdrawEnabled: false},
	}}, f.clock))
	r, err := f.Routable(ctx, "binance", "USDT", "TRX")
	require.NoError(t, err)
	assert.True(t, r.Routable)
	r, err = f.Routable(ctx, "binance", "USDT", "BSC")
	require.NoError(t, err)
	assert.Equal(t, []string{ReasonNetworkSuspended}, r.Reasons)
	r, err = f.Routable(ctx, "binance", "USDT", "ETH")
	require.NoError(t, err)
	assert.Equal(t, []string{ReasonNetworkUnknown}, r.Reasons)

	require.NoError(t, f.SaveNetworks(ctx, "binance", map[string][]venues.Network{"USDT": {
		{Network: "BSC", DepositEnabled: true, WithdrawEnabled: true},
	}}, f.clock))
	r, err = f.Routable(ctx, "binance", "USDT", "TRX")
	require.NoError(t, err)
	assert.Equal(t, []string{ReasonNetworkSuspended}, r.Reasons, "a network the venue dropped is suspended")
}

// TestRoutable_AMarketHaltedOrDropped covers the markets.
func TestRoutable_AMarketHaltedOrDropped(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	require.NoError(t, f.SaveInstruments(ctx, "binance", []venues.Instrument{
		{Symbol: "BTCUSDT", Base: "BTC", Quote: "USDT", Trading: true},
		{Symbol: "ETHUSDT", Base: "ETH", Quote: "USDT", Trading: false},
	}, f.clock))
	r, err := f.MarketRoutable(ctx, "binance", "BTCUSDT")
	require.NoError(t, err)
	assert.True(t, r.Routable)
	r, err = f.MarketRoutable(ctx, "binance", "ETHUSDT")
	require.NoError(t, err)
	assert.Equal(t, []string{ReasonMarketHalted}, r.Reasons)
	require.NoError(t, f.SaveInstruments(ctx, "binance", nil, f.clock))
	r, err = f.MarketRoutable(ctx, "binance", "BTCUSDT")
	require.NoError(t, err)
	assert.Equal(t, []string{ReasonMarketHalted}, r.Reasons, "a market the venue dropped is halted")
}

// TestReconcile_ADifferenceHoldsTheCurrency covers the second criterion
// for the reconciliation, and the review of the hold: a difference beyond
// the tolerance, found twice in a row, makes the currency not routable
// until an operator clears it, audited; a hold cleared in SQL still holds.
func TestReconcile_ADifferenceHoldsTheCurrency(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.deposit(t, "d1", "100")
	f.balance(t, "100.005")
	report, err := f.Reconcile(ctx, "binance", mapping)
	require.NoError(t, err)
	require.Len(t, report.Differences, 1)
	assert.False(t, report.Differences[0].Beyond, "within the tolerance")

	f.balance(t, "90")
	report, err = f.Reconcile(ctx, "binance", mapping)
	require.NoError(t, err)
	d := report.Differences[0]
	assert.True(t, d.Beyond)
	assert.Equal(t, "-10", d.Difference.String())
	assert.False(t, d.HoldOpened, "one run is not enough: a move may be in flight")
	r, err := f.Routable(ctx, "binance", "USDT", "")
	require.NoError(t, err)
	assert.True(t, r.Routable)

	report, err = f.Reconcile(ctx, "binance", mapping)
	require.NoError(t, err)
	assert.True(t, report.Differences[0].HoldOpened)
	r, err = f.Routable(ctx, "binance", "USDT", "")
	require.NoError(t, err)
	assert.Equal(t, []string{ReasonHold}, r.Reasons)
	_, err = f.Reserve(ctx, Reservation{Venue: "binance", Asset: "USDT", Amount: dec("1"), ReferenceType: "route", ReferenceID: "1"})
	require.ErrorIs(t, err, ErrNotRoutable)

	// A hold cleared in SQL fails its seal and still holds.
	var id string
	require.NoError(t, f.db.QueryRow(`SELECT id::text FROM venue_holds WHERE cleared_at IS NULL`).Scan(&id))
	admin := shared.NewUUID().String()
	_, err = f.db.Exec(`UPDATE venue_holds SET cleared_at = now(), cleared_by = $2 WHERE id = $1`, id, admin)
	require.NoError(t, err)
	r, err = f.Routable(ctx, "binance", "USDT", "")
	require.NoError(t, err)
	assert.False(t, r.Routable, "a hold cleared in SQL still holds")
	_, err = f.db.Exec(`UPDATE venue_holds SET cleared_at = NULL, cleared_by = NULL WHERE id = $1`, id)
	require.NoError(t, err)

	origin := audit.Event{ActorType: audit.ActorAdmin, ActorID: admin}
	h, err := f.ClearHold(ctx, id, origin, "the venue credited a deposit late")
	require.NoError(t, err)
	assert.Equal(t, admin, h.ClearedBy)
	_, err = f.ClearHold(ctx, id, origin, "again")
	require.ErrorIs(t, err, ErrAlreadyCleared)
	var events int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM audit_events WHERE action = $1 AND target_id = $2`,
		audit.ActionAdminVenueHoldClear, id).Scan(&events))
	assert.Equal(t, 1, events)
	r, err = f.Routable(ctx, "binance", "USDT", "")
	require.NoError(t, err)
	assert.True(t, r.Routable)

	// The difference stays: the next runs hold the currency again.
	_, err = f.Reconcile(ctx, "binance", mapping)
	require.NoError(t, err)
	r, err = f.Routable(ctx, "binance", "USDT", "")
	require.NoError(t, err)
	assert.Equal(t, []string{ReasonHold}, r.Reasons)
}

// TestReconcile_SkipsASnapshotOlderThanAMove covers a move booked after
// the balances were taken: it is not in them, so nothing is compared.
func TestReconcile_SkipsASnapshotOlderThanAMove(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.balance(t, "0")
	f.clock = f.clock.Add(-time.Hour)
	f.balance(t, "0")
	f.clock = time.Now()
	_, err := f.Ledger.VenueDeposit(ctx, "late", "binance", ledger.Amount{Currency: f.usdt, Value: dec("5")}, ledger.Ref{})
	require.NoError(t, err)
	f.Opts.MaxAge = 2 * time.Hour
	report, err := f.Reconcile(ctx, "binance", mapping)
	require.NoError(t, err)
	assert.True(t, report.Skipped)
}

// feeVenue is a stub that reports fees.
type feeVenue struct {
	*stub.Venue
	fees []Fee
}

func (v feeVenue) Fees(context.Context) ([]Fee, error) { return v.fees, nil }

// TestJob_PostsNothingWithoutADifferenceAndExactlyTheFee covers the third
// criterion: a run with no difference posts nothing, and a run with a
// venue fee posts exactly that fee, once, and finds no difference after.
func TestJob_PostsNothingWithoutADifferenceAndExactlyTheFee(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.deposit(t, "d1", "100")
	v := stub.New("binance")
	v.SetBalance("USDT", dec("100"))
	v.AddNetwork("USDT", venues.Network{Network: "TRX", DepositEnabled: true, WithdrawEnabled: true})
	venue := &feeVenue{Venue: v}
	job := Job{Store: f.Store, Venue: venue, Mapping: mapping, Now: func() time.Time { return time.Now() }}
	assert.Equal(t, "venue_state:binance", job.Name())

	before := f.entries(t, ledger.KindVenueFee)
	require.NoError(t, job.Run(ctx, jobs.NewRun(marketdata.ModeLive, nil)))
	assert.Equal(t, before, f.entries(t, ledger.KindVenueFee), "no difference posts nothing")
	r, err := f.Routable(ctx, "binance", "USDT", "TRX")
	require.NoError(t, err)
	assert.True(t, r.Routable, "%v", r.Reasons)

	v.SetBalance("USDT", dec("98.5"))
	venue.fees = []Fee{{ID: "w-1", Asset: "USDT", Amount: dec("1.5")}}
	for i := 0; i < 3; i++ {
		require.NoError(t, job.Run(ctx, jobs.NewRun(marketdata.ModeLive, nil)))
	}
	assert.Equal(t, before+1, f.entries(t, ledger.KindVenueFee), "exactly the fee, once")
	held, err := f.Ledger.VenueHoldings(ctx, "binance")
	require.NoError(t, err)
	assert.Equal(t, "98.5", held[f.usdt.String()].String())
	r, err = f.Routable(ctx, "binance", "USDT", "TRX")
	require.NoError(t, err)
	assert.True(t, r.Routable, "the fee leaves no difference: %v", r.Reasons)

	// A dry run writes nothing.
	v.SetBalance("USDT", dec("1"))
	require.NoError(t, job.Run(ctx, jobs.NewRun(marketdata.ModeDryRun, nil)))
	available, err := f.Available(ctx, "binance", "USDT")
	require.NoError(t, err)
	assert.Equal(t, "98.5", available.String())

	states, err := f.Overview(ctx, map[string]venues.Mapping{"binance": mapping})
	require.NoError(t, err)
	require.Len(t, states, 1)
	require.Len(t, states[0].Balances, 1)
	b := states[0].Balances[0]
	assert.Equal(t, "USDT", b.Currency)
	assert.Equal(t, "0", b.Difference.String())
	assert.True(t, b.Routability.Routable)
}

// TestCollector_ExportsTheStateOfTheVenues covers the metrics of #385.
func TestCollector_ExportsTheStateOfTheVenues(t *testing.T) {
	f := newFixture(t)
	f.deposit(t, "d1", "100")
	f.balance(t, "99")
	_, err := f.Reserve(context.Background(), Reservation{Venue: "binance", Asset: "USDT", Amount: dec("40"),
		ReferenceType: "route", ReferenceID: "1"})
	require.NoError(t, err)
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(Collector{Store: f.Store, Mappings: map[string]venues.Mapping{"binance": mapping}}))
	families, err := reg.Gather()
	require.NoError(t, err)
	values := map[string]float64{}
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			values[mf.GetName()] = m.GetGauge().GetValue()
		}
	}
	assert.Equal(t, 59.0, values["venue_routable_balance"])
	assert.Equal(t, -1.0, values["venue_reconciliation_difference"])
	assert.Contains(t, values, "venue_snapshot_age_seconds")
	assert.Equal(t, 0.0, values["venue_open_holds"])
}
