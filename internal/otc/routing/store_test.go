package routing

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"
	"github.com/mdastpak/crypto-exchange/internal/venues"
	"github.com/mdastpak/crypto-exchange/internal/venues/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db := pgtest.Migrated(t, "testdb_otc_routing")
	sealtest.Use(t, db, rowseal.ModeEnforce)
	return &Store{DB: db}, db
}

func admin() audit.Event {
	return audit.Event{ActorType: audit.ActorAdmin, ActorID: shared.NewUUID().String()}
}

var (
	ownRules    = []Rule{{Name: "own", UseInventory: true}}
	venueRules  = []Rule{{Name: "binance", Venues: []VenueRule{{Venue: "binance"}}}}
	openVenues  = func() fakeVenues { v, _ := world(); return v }
	noInventory = fakeInventory{}
)

// TestStore_SaveAppendsAnAuditedVersion covers the third criterion of
// #386: each change is a new version, recorded in the audit log in its
// transaction.
func TestStore_SaveAppendsAnAuditedVersion(t *testing.T) {
	s, db := newStore(t)
	ctx := context.Background()
	none, err := s.Latest(ctx)
	require.NoError(t, err)
	assert.Zero(t, none.Version)

	origin := admin()
	first, err := s.Save(ctx, ownRules, "start with the inventory", origin)
	require.NoError(t, err)
	second, err := s.Save(ctx, venueRules, "cover on binance", origin)
	require.NoError(t, err)
	assert.Greater(t, second.Version, first.Version)
	assert.Equal(t, origin.ActorID, second.CreatedBy)
	assert.Equal(t, "cover on binance", second.Reason)

	latest, err := s.Latest(ctx)
	require.NoError(t, err)
	assert.Equal(t, second.Version, latest.Version)
	assert.Equal(t, "binance", latest.Rules[0].Name)

	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM audit_events WHERE action = $1 AND target_type = $2 AND target_id = $3`,
		audit.ActionAdminOTCRoutingRules, audit.TargetOTCRoutingRules, strconv.FormatInt(second.Version, 10)).Scan(&n))
	assert.Equal(t, 1, n)

	// Rules that do not validate save nothing, and audit nothing.
	_, err = s.Save(ctx, []Rule{{Name: "bad", Venues: []VenueRule{{Venue: "kraken"}}}}, "typo", origin)
	var p Problems
	require.True(t, errors.As(err, &p))
	latest, err = s.Latest(ctx)
	require.NoError(t, err)
	assert.Equal(t, second.Version, latest.Version)
}

// TestCache_TwoNodesRouteAlikeAndSeeAChangeWithinTheirWindow covers the
// second and third criteria of #386: two instances reading one version
// route a trade alike, and a new version reaches both within their cache
// window, without a restart.
func TestCache_TwoNodesRouteAlikeAndSeeAChangeWithinTheirWindow(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	_, err := s.Save(ctx, ownRules, "start", admin())
	require.NoError(t, err)

	clock := time.Now()
	now := func() time.Time { return clock }
	_, inv := world()
	node := func() (*Router, *Cache) {
		c := &Cache{Store: &Store{DB: s.DB}, TTL: 5 * time.Second, Now: now}
		return &Router{Rules: c, Venues: openVenues(), Inventory: inv}, c
	}
	a, _ := node()
	b, _ := node()
	in := input("buy", "0.5")
	ra, err := a.Plan(ctx, in)
	require.NoError(t, err)
	rb, err := b.Plan(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, ra, rb)
	assert.Equal(t, "own", ra.Rule)

	// Another node saves a version: within the window, both keep the one
	// they hold; past it, both route by the new one.
	saved, err := s.Save(ctx, venueRules, "cover on binance", admin())
	require.NoError(t, err)
	clock = clock.Add(4 * time.Second)
	ra, err = a.Plan(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, "own", ra.Rule)
	clock = clock.Add(2 * time.Second)
	ra, err = a.Plan(ctx, in)
	require.NoError(t, err)
	rb, err = b.Plan(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, ra, rb)
	assert.Equal(t, "binance", ra.Rule)
	assert.Equal(t, saved.Version, ra.RuleVersion)
}

// TestStore_AVersionChangedInSQLRoutesNothing changes the rules of a
// version outside the backend: the routing fails closed.
func TestStore_AVersionChangedInSQLRoutesNothing(t *testing.T) {
	s, db := newStore(t)
	ctx := context.Background()
	v, err := s.Save(ctx, venueRules, "start", admin())
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE otc_routing_rules SET rules = '[{"name":"own","use_inventory":true}]' WHERE version = $1`, v.Version)
	require.NoError(t, err)

	_, err = s.Latest(ctx)
	assert.ErrorIs(t, err, rowseal.ErrUnverified)
	c := NewCache(s, time.Second)
	_, err = (&Router{Rules: c, Venues: openVenues(), Inventory: noInventory}).Plan(ctx, input("buy", "0.5"))
	assert.ErrorIs(t, err, rowseal.ErrUnverified)
}

// TestUsage_AddsUpPerVenueAndDay covers the daily usage that the limits
// read, and a usage lowered in SQL, which is refused.
func TestUsage_AddsUpPerVenueAndDay(t *testing.T) {
	s, db := newStore(t)
	ctx := context.Background()
	var hk string
	require.NoError(t, db.QueryRow(`INSERT INTO currencies (status, info)
		VALUES ('approved', jsonb_build_object('symbol', 'USDT', 'en_name', 'USDT', 'fa_name', 'USDT',
			'if_fiat', false, 'is_token', true, 'contract', '')) RETURNING hk`).Scan(&hk))
	day := time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)
	add := func(venue string, day time.Time, notional string) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		require.NoError(t, AddUsage(ctx, tx, venue, hk, day, d(notional)))
		require.NoError(t, tx.Commit())
	}
	add("binance", day, "1000")
	add("binance", day.Add(90*time.Minute), "250.5") // the next day
	add("binance", day, "500")
	add("nobitex", day, "7")

	used, err := s.DailyNotional(ctx, "binance", hk, day)
	require.NoError(t, err)
	assert.True(t, used.Equal(d("1500")), used.String())
	used, err = s.DailyNotional(ctx, "binance", hk, day.Add(2*time.Hour))
	require.NoError(t, err)
	assert.True(t, used.Equal(d("250.5")), used.String())
	used, err = s.DailyNotional(ctx, "kraken", hk, day)
	require.NoError(t, err)
	assert.True(t, used.IsZero())

	_, err = db.Exec(`UPDATE otc_routing_usage SET notional = '0' WHERE venue = 'binance' AND day = '2026-11-16'`)
	require.NoError(t, err)
	_, err = s.DailyNotional(ctx, "binance", hk, day)
	assert.ErrorIs(t, err, rowseal.ErrUnverified)
}

// TestStateVenues_ReadTheSnapshotsOfAVenue covers the venue side of the
// rules (#386) on the state of #385: the market listed and trading, the
// funds less the reservations, and the network open for withdrawal.
func TestStateVenues_ReadTheSnapshotsOfAVenue(t *testing.T) {
	_, db := newStore(t)
	ctx := context.Background()
	clock := time.Now()
	st := state.New(db, state.Options{MaxAge: time.Minute, ConfirmRuns: 2})
	st.Now = func() time.Time { return clock }
	c := &config.Configuration{}
	c.Venues.Exchanges = map[string]config.VenueConfig{"binance": {Enabled: true, FeeBPS: 10, SlippageBPS: 4}}
	v := StateVenues{State: st, Config: func() *config.Configuration { return c }, Mappings: map[string]venues.Mapping{"binance": {
		"BTC":  {Currency: "BTC", Networks: map[string]string{"BTC": "bitcoin"}},
		"USDT": {Currency: "USDT"},
	}}}

	m, err := v.Market(ctx, "binance", "BTC", "USDT")
	require.NoError(t, err)
	assert.Equal(t, []string{state.ReasonMarketUnknown}, m.Reasons)

	sd := shared.MustNewDecimalFromString
	require.NoError(t, st.SaveInstruments(ctx, "binance", []venues.Instrument{{Symbol: "BTCUSDT", Base: "BTC", Quote: "USDT", Trading: true,
		LotSize: sd("0.0001"), MinNotional: sd("5")}}, clock))
	require.NoError(t, st.SaveBalances(ctx, "binance", []venues.Balance{
		{Asset: "USDT", Free: sd("1000"), Locked: sd("0")}, {Asset: "BTC", Free: sd("1"), Locked: sd("0")},
	}, clock))
	require.NoError(t, st.SaveNetworks(ctx, "binance", map[string][]venues.Network{"BTC": {
		{Network: "BTC", DepositEnabled: true, WithdrawEnabled: true, WithdrawFee: sd("0.0002")},
	}}, clock))

	m, err = v.Market(ctx, "binance", "BTC", "USDT")
	require.NoError(t, err)
	assert.Empty(t, m.Reasons)
	assert.Equal(t, "BTCUSDT", m.Symbol)
	assert.True(t, m.Lot.Equal(d("0.0001")))
	assert.Equal(t, 10, m.FeeBPS)
	assert.Equal(t, 4, m.SlippageBPS)

	f, err := v.Funds(ctx, "binance", "USDT", "")
	require.NoError(t, err)
	assert.Empty(t, f.Reasons)
	assert.True(t, f.Available.Equal(d("1000")))
	f, err = v.Funds(ctx, "binance", "BTC", "bitcoin")
	require.NoError(t, err)
	assert.Empty(t, f.Reasons)
	assert.True(t, f.WithdrawFee.Equal(d("0.0002")))
	f, err = v.Funds(ctx, "binance", "BTC", "lightning")
	require.NoError(t, err)
	assert.Equal(t, []string{ReasonNotMapped}, f.Reasons)

	// Past the age of the snapshots, nothing is routable.
	clock = clock.Add(2 * time.Minute)
	f, err = v.Funds(ctx, "binance", "USDT", "")
	require.NoError(t, err)
	assert.Contains(t, f.Reasons, state.ReasonStaleBalance)
	m, err = v.Market(ctx, "binance", "BTC", "USDT")
	require.NoError(t, err)
	assert.Contains(t, m.Reasons, state.ReasonMarketStale)

	// A venue switched off routes nothing.
	c.Venues.Exchanges["binance"] = config.VenueConfig{}
	m, err = v.Market(ctx, "binance", "BTC", "USDT")
	require.NoError(t, err)
	assert.Equal(t, []string{ReasonVenueDisabled}, m.Reasons)
}
