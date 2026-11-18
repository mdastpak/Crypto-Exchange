package otc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/otc/routed"
	"github.com/mdastpak/crypto-exchange/internal/otc/routing"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/venues"
	"github.com/mdastpak/crypto-exchange/internal/venues/state"
	"github.com/mdastpak/crypto-exchange/internal/venues/stub"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// routedWorld is an executor whose routes go to stub venues, and a driver
// of the orders of each.
type routedWorld struct {
	execFixture
	state   *state.Store
	config  *config.Configuration
	planner *routed.Planner
	venues  map[string]*stub.Venue
	names   []string
	now     time.Time
}

// newRoutedWorld routes every trade to the venues of names, in their order,
// each with funds of the quote currency and of the base, at price.
func newRoutedWorld(t *testing.T, price string, names ...string) *routedWorld {
	t.Helper()
	ctx := context.Background()
	w := &routedWorld{execFixture: newExecFixture(t), venues: map[string]*stub.Venue{}, now: time.Now()}
	w.state = state.New(w.DB, state.Options{MaxAge: time.Hour})
	w.config = &config.Configuration{}
	w.config.OTC.Routing = config.OTCRoutingConfig{Enabled: true, CacheSeconds: 5, MaxSlippageBPS: 50, OrderTimeoutSeconds: 60,
		UnknownGraceSeconds: 120}
	w.config.Venues.Exchanges = map[string]config.VenueConfig{}
	base, quote := w.market.Base, w.market.Quote
	var rule routing.Rule
	rule.Name = "venues"
	for i, n := range names {
		name := n + "_" + strings.ToLower(suffix())
		w.names = append(w.names, name)
		w.config.Venues.Exchanges[name] = config.VenueConfig{Enabled: true, FeeBPS: 10, SlippageBPS: 5,
			Assets: map[string]config.VenueAsset{base: {Currency: base}, quote: {Currency: quote}}}
		v := stub.New(name)
		v.SetBalance(quote, shared.MustNewDecimalFromString("100000"))
		v.SetBalance(base, shared.MustNewDecimalFromString("2"))
		v.AddInstrument(venues.Instrument{Symbol: base + quote, Base: base, Quote: quote, TickSize: shared.MustNewDecimalFromString("0.01"),
			LotSize: shared.MustNewDecimalFromString("0.001"), MinQuantity: shared.MustNewDecimalFromString("0.001"),
			MinNotional: shared.MustNewDecimalFromString("1"), Trading: true}, shared.MustNewDecimalFromString(price))
		w.venues[name] = v
		w.snapshot(t, name)
		rule.Venues = append(rule.Venues, routing.VenueRule{Venue: name, Priority: i})
	}
	settings := func() *config.Configuration { return w.config }
	w.exec.Routing = &Routing{Router: &routing.Router{
		Rules:     fixedRules{routing.RuleSet{Version: 1, Rules: []routing.Rule{rule}}},
		Venues:    routing.StateVenues{State: w.state, Config: settings},
		Inventory: HouseInventory{DB: w.DB},
		Usage:     &routing.Store{DB: w.DB},
	}}
	w.planner = &routed.Planner{State: w.state, Node: "test-executor", Settings: func() routed.Settings { return routed.SettingsOf(w.config) }}
	w.exec.Routed = w.planner
	_ = ctx
	return w
}

// snapshot saves the state of the venue name as its job would.
func (w *routedWorld) snapshot(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	v := w.venues[name]
	balances, err := v.Balances(ctx)
	require.NoError(t, err)
	instruments, err := v.Instruments(ctx)
	require.NoError(t, err)
	require.NoError(t, w.state.SaveInstruments(ctx, name, instruments, time.Now()))
	require.NoError(t, w.state.SaveBalances(ctx, name, balances, time.Now()))
}

// driver drives the orders of the venue name, as node.
func (w *routedWorld) driver(name, node string, v venues.Venue) *routed.Driver {
	if v == nil {
		v = w.venues[name]
	}
	return &routed.Driver{DB: w.DB, Venue: v, State: w.state, Ledger: ledger.Ledger{DB: w.DB}, Planner: w.planner,
		Reroute: RerouteOf(w.DB, w.exec.Routing), Positions: MovePositions, Node: node, Now: func() time.Time { return w.now }}
}

// trade executes a buy of quantity at price, routed.
func (w *routedWorld) trade(t *testing.T, side Side, quantity, price string) Trade {
	t.Helper()
	q := w.quote(t, side, quantity, price, "0")
	q.Components.RouteBPS = dec("100")
	q.Components.Route = &routing.Route{RuleVersion: 1, Rule: "venues", Lot: dec("0.001")}
	require.NoError(t, w.store.Save(context.Background(), q))
	out, err := w.execute(q, "k-"+shared.NewUUID().String())
	require.NoError(t, err)
	return out.Trade
}

func (w *routedWorld) orders(t *testing.T, tradeID string) []routed.Order {
	t.Helper()
	out, err := routed.OfTrade(context.Background(), w.DB, tradeID)
	require.NoError(t, err)
	return out
}

// holdings is what the books say the venue holds of currency.
func (w *routedWorld) holdings(t *testing.T, venue, currencyHK string) string {
	t.Helper()
	h, err := ledger.Ledger{DB: w.DB}.VenueHoldings(context.Background(), venue)
	require.NoError(t, err)
	return h[currencyHK].String()
}

func (w *routedWorld) reserved(t *testing.T, orderID string) (open bool) {
	t.Helper()
	require.NoError(t, w.DB.QueryRow(`SELECT released_at IS NULL FROM venue_reservations WHERE reference_type = $1 AND reference_id = $2`,
		routed.RefType, orderID).Scan(&open))
	return open
}

func (w *routedWorld) stubBalance(t *testing.T, venue, asset string) string {
	t.Helper()
	balances, err := w.venues[venue].Balances(context.Background())
	require.NoError(t, err)
	for _, b := range balances {
		if b.Asset == asset {
			return b.Free.String()
		}
	}
	return ""
}

// TestRouted_PlacesFillsAndPostsATrade covers the path of #387: the trade
// plans its order with a reservation, the daily usage, and an event, in its
// transaction; the driver places it with the slippage guard as its limit,
// posts its fill to the ledger, moves the inventory's positions back, and
// releases the reservation. The books then agree with the venue.
func TestRouted_PlacesFillsAndPostsATrade(t *testing.T) {
	ctx := context.Background()
	w := newRoutedWorld(t, "60000", "a")
	venue := w.names[0]
	tr := w.trade(t, SideBuy, "0.5", "60500")

	list := w.orders(t, tr.ID)
	require.Len(t, list, 1)
	o := list[0]
	assert.Equal(t, routed.StatusPlanned, o.Status)
	assert.Equal(t, "buy", o.Side)
	assertDec(t, "0.5", o.Quantity)
	// min(60500 / 1.001, 60500 × 1.005), down to the tick.
	assertDec(t, "60439.56", o.Limit)
	assert.Equal(t, routed.ClientID(tr.ID, 0, 0), o.ClientID)
	assert.True(t, w.reserved(t, o.ID))
	var usage string
	require.NoError(t, w.DB.QueryRow(`SELECT notional::text FROM otc_routing_usage WHERE venue = $1`, venue).Scan(&usage))
	assertDec(t, "30250", dec(usage))
	var events int
	require.NoError(t, w.DB.QueryRow(`SELECT count(*) FROM outbox_events WHERE topic = $1 AND payload->>'order_id' = $2`,
		routed.TopicChanged, o.ID).Scan(&events))
	assert.Equal(t, 1, events)
	// The trade took the whole of it from the inventory.
	assert.Equal(t, "-0.5", w.position(t, w.market.BaseHK))

	d := w.driver(venue, "node-a", nil)
	require.NoError(t, d.Drive(ctx, o.ID))
	o, err := routed.Read(ctx, w.DB, o.ID)
	require.NoError(t, err)
	assert.Equal(t, routed.StatusFilled, o.Status)
	assertDec(t, "0.5", o.Filled)
	assertDec(t, "30000", o.FilledQuote)
	assert.False(t, w.reserved(t, o.ID))

	// The fill gave back what the trade took; the inventory keeps the
	// difference of the prices.
	assert.Equal(t, "0", w.position(t, w.market.BaseHK))
	assert.Equal(t, "250", w.position(t, w.market.QuoteHK))
	assert.Equal(t, "0.5", w.holdings(t, venue, w.market.BaseHK))
	assert.Equal(t, "-30000", w.holdings(t, venue, w.market.QuoteHK))
	assert.Equal(t, "2.5", w.stubBalance(t, venue, w.market.Base))
	assert.Equal(t, "70000", w.stubBalance(t, venue, w.market.Quote))

	// A filled order is not driven again.
	require.NoError(t, d.Drive(ctx, o.ID))
	var entries int
	require.NoError(t, w.DB.QueryRow(`SELECT count(*) FROM ledger_entries WHERE reference_type = $1 AND reference_id = $2`,
		routed.RefType, o.ID).Scan(&entries))
	assert.Equal(t, 1, entries)
	var transitions []string
	rows, err := w.DB.Query(`SELECT to_status FROM otc_routed_order_events WHERE order_id = $1 ORDER BY id`, o.ID)
	require.NoError(t, err)
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		transitions = append(transitions, s)
	}
	require.NoError(t, rows.Close())
	assert.Equal(t, []string{"planned", "filled"}, transitions)

	// A sell sells at the venue, at a limit that keeps the trade whole.
	sell := w.trade(t, SideSell, "0.2", "59500")
	so := w.orders(t, sell.ID)[0]
	assert.Equal(t, "sell", so.Side)
	// max(59500 / 0.999, 59500 × 0.995), up to the tick.
	assertDec(t, "59559.56", so.Limit)
}

// TestRouted_ATimedOutOrderIsCancelledAndItsRemainderRoutedAgain covers a
// partial fill, the deadline of an order, and the route of what it did not
// fill at another venue.
func TestRouted_ATimedOutOrderIsCancelledAndItsRemainderRoutedAgain(t *testing.T) {
	ctx := context.Background()
	w := newRoutedWorld(t, "61000", "a", "b")
	a, b := w.names[0], w.names[1]
	tr := w.trade(t, SideBuy, "0.5", "60500")
	o := w.orders(t, tr.ID)[0]
	assert.Equal(t, a, o.Venue)

	da := w.driver(a, "node-a", nil)
	require.NoError(t, da.Drive(ctx, o.ID))
	o, err := routed.Read(ctx, w.DB, o.ID)
	require.NoError(t, err)
	assert.Equal(t, routed.StatusPlaced, o.Status, "the venue's price is above the limit: the order rests")

	require.NoError(t, w.venues[a].Fill(o.ClientID, shared.MustNewDecimalFromString("0.2")))
	require.NoError(t, da.Drive(ctx, o.ID))
	o, err = routed.Read(ctx, w.DB, o.ID)
	require.NoError(t, err)
	assert.Equal(t, routed.StatusPartiallyFilled, o.Status)
	assertDec(t, "0.2", o.Filled)
	assert.Equal(t, "0.2", w.holdings(t, a, w.market.BaseHK))

	// Past the deadline, at b's lower price.
	w.venues[b].SetPrice(w.market.Base+w.market.Quote, shared.MustNewDecimalFromString("60000"))
	w.now = w.now.Add(2 * time.Minute)
	require.NoError(t, da.Drive(ctx, o.ID))
	o, err = routed.Read(ctx, w.DB, o.ID)
	require.NoError(t, err)
	assert.Equal(t, routed.StatusCancelled, o.Status)
	assert.Equal(t, routed.ReasonTimedOut, *o.Reason)
	assert.False(t, w.reserved(t, o.ID))

	list := w.orders(t, tr.ID)
	require.Len(t, list, 2)
	child := list[1]
	assert.Equal(t, b, child.Venue, "the venue that failed the leg is left out")
	assert.Equal(t, 1, child.Attempt)
	require.NotNil(t, child.ParentID)
	assert.Equal(t, o.ID, *child.ParentID)
	assertDec(t, "0.3", child.Quantity)
	assert.Equal(t, routed.ClientID(tr.ID, 0, 1), child.ClientID)

	require.NoError(t, w.driver(b, "node-b", nil).Drive(ctx, child.ID))
	child, err = routed.Read(ctx, w.DB, child.ID)
	require.NoError(t, err)
	assert.Equal(t, routed.StatusFilled, child.Status)
	assert.Equal(t, "0", w.position(t, w.market.BaseHK), "the two venues covered the trade")
}

// TestRouted_ALostAnswerIsLookedUpNotPlacedTwice covers an answer lost in a
// timeout: the order is looked up by its client id and adopted.
func TestRouted_ALostAnswerIsLookedUpNotPlacedTwice(t *testing.T) {
	ctx := context.Background()
	w := newRoutedWorld(t, "60000", "a")
	venue := w.names[0]
	tr := w.trade(t, SideBuy, "0.1", "60500")
	o := w.orders(t, tr.ID)[0]
	d := w.driver(venue, "node-a", nil)

	w.venues[venue].LoseNextAnswer()
	require.NoError(t, d.Drive(ctx, o.ID))
	o, err := routed.Read(ctx, w.DB, o.ID)
	require.NoError(t, err)
	assert.Equal(t, routed.StatusPlanned, o.Status, "the outcome is unknown")
	require.NotNil(t, o.UncertainSince)

	require.NoError(t, d.Drive(ctx, o.ID))
	o, err = routed.Read(ctx, w.DB, o.ID)
	require.NoError(t, err)
	assert.Equal(t, routed.StatusFilled, o.Status, "found by its client id")
	assert.Equal(t, "94000", w.stubBalance(t, venue, w.market.Quote), "placed once")
	assert.Equal(t, "0.1", w.holdings(t, venue, w.market.BaseHK))
}

// stealing is a venue during whose placement another node takes over the
// order, as one does after this node's lease expired.
type stealing struct {
	venues.Venue
	steal func()
}

func (s stealing) PlaceOrder(ctx context.Context, req venues.OrderRequest) (venues.Order, error) {
	o, err := s.Venue.PlaceOrder(ctx, req)
	s.steal()
	return o, err
}

// TestRouted_TwoNodesOnOneOrder covers the lease of an order: a node that
// holds it keeps another out, and a node whose lease was taken over writes
// nothing, while the node that took it over adopts the order it placed.
func TestRouted_TwoNodesOnOneOrder(t *testing.T) {
	ctx := context.Background()
	w := newRoutedWorld(t, "60000", "a")
	venue := w.names[0]
	tr := w.trade(t, SideBuy, "0.1", "60500")
	o := w.orders(t, tr.ID)[0]

	take := func() {
		_, err := w.DB.Exec(`UPDATE otc_routed_orders SET lease_token = lease_token + 1, lease_holder = 'node-b',
			lease_until = clock_timestamp() + interval '1 minute' WHERE id = $1`, o.ID)
		require.NoError(t, err)
	}
	take()
	require.NoError(t, w.driver(venue, "node-a", nil).Drive(ctx, o.ID), "node-b holds it")
	_, err := w.venues[venue].GetOrder(ctx, o.Symbol, o.ClientID)
	require.ErrorIs(t, err, venues.ErrNotFound, "node-a did nothing")

	expire := func() {
		_, err := w.DB.Exec(`UPDATE otc_routed_orders SET lease_until = '-infinity' WHERE id = $1`, o.ID)
		require.NoError(t, err)
	}
	expire()
	a := w.driver(venue, "node-a", stealing{Venue: w.venues[venue], steal: take})
	err = a.Drive(ctx, o.ID)
	require.ErrorIs(t, err, routed.ErrLeaseLost)
	o, err = routed.Read(ctx, w.DB, o.ID)
	require.NoError(t, err)
	assert.Equal(t, routed.StatusPlanned, o.Status, "node-a wrote nothing")
	assert.Equal(t, "0", w.holdings(t, venue, w.market.BaseHK))

	expire()
	require.NoError(t, w.driver(venue, "node-b", nil).Drive(ctx, o.ID))
	o, err = routed.Read(ctx, w.DB, o.ID)
	require.NoError(t, err)
	assert.Equal(t, routed.StatusFilled, o.Status)
	assert.Equal(t, "0.1", w.holdings(t, venue, w.market.BaseHK), "posted once")
}

// TestRouted_ARejectedOrderWithNoOtherVenueIsLeftToAReview covers a venue
// that refuses an order, and a remainder that nothing routes.
func TestRouted_ARejectedOrderWithNoOtherVenueIsLeftToAReview(t *testing.T) {
	ctx := context.Background()
	w := newRoutedWorld(t, "60000", "a")
	venue := w.names[0]
	tr := w.trade(t, SideBuy, "0.1", "60500")
	o := w.orders(t, tr.ID)[0]
	w.venues[venue].RejectNext("-1013")
	require.NoError(t, w.driver(venue, "node-a", nil).Drive(ctx, o.ID))
	o, err := routed.Read(ctx, w.DB, o.ID)
	require.NoError(t, err)
	assert.Equal(t, routed.StatusManualReview, o.Status)
	assert.Equal(t, routed.ReasonNoRoute, *o.Reason)
	assert.False(t, w.reserved(t, o.ID))
	assert.Equal(t, "-0.1", w.position(t, w.market.BaseHK), "the inventory keeps what nothing covered")
}

// TestRouted_NothingOfATradeCommitsWithoutItsOrders injects a failure after
// the orders are planned, and checks the lockdown of otc_routing.
func TestRouted_NothingOfATradeCommitsWithoutItsOrders(t *testing.T) {
	ctx := context.Background()
	w := newRoutedWorld(t, "60000", "a")
	w.exec.fail = func(step string) error {
		if step == stepRouted {
			return errors.New("injected")
		}
		return nil
	}
	q := w.quote(t, SideBuy, "0.1", "60500", "0")
	q.Components.RouteBPS = dec("100")
	q.Components.Route = &routing.Route{RuleVersion: 1, Rule: "venues", Lot: dec("0.001")}
	require.NoError(t, w.store.Save(ctx, q))
	_, err := w.execute(q, "fails")
	require.Error(t, err)
	var n int
	require.NoError(t, w.DB.QueryRow(`SELECT count(*) FROM venue_reservations WHERE venue = $1`, w.names[0]).Scan(&n))
	assert.Zero(t, n)
	require.NoError(t, w.DB.QueryRow(`SELECT count(*) FROM otc_routing_usage WHERE venue = $1`, w.names[0]).Scan(&n))
	assert.Zero(t, n)
	w.exec.fail = nil

	c, err := platform.Create(ctx, w.DB, platform.New{Kind: platform.KindLockdown, Feature: platform.FeatureOTCRouting,
		Reason: "routed orders test"}, audit.Event{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := platform.Lift(ctx, w.DB, platform.LiftRequest{HK: c.HK, Note: "done"}, audit.Event{})
		require.NoError(t, err)
	})
	q = w.quote(t, SideBuy, "0.1", "60500", "0")
	q.Components.RouteBPS = dec("100")
	q.Components.Route = &routing.Route{RuleVersion: 1, Rule: "venues", Lot: dec("0.001")}
	require.NoError(t, w.store.Save(ctx, q))
	_, err = w.execute(q, "locked")
	var locked *platform.Locked
	require.ErrorAs(t, err, &locked)
}
