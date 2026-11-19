package otc

import (
	"context"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/otc/routed"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/venues"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (w *routedWorld) review() *routed.Review {
	return &routed.Review{DB: w.DB, Planner: w.planner, Reroute: RerouteOf(w.DB, w.exec.Routing)}
}

// leftForNoRoute trades quantity at the only venue, which refuses the
// order: it is left to a review for no_route.
func (w *routedWorld) leftForNoRoute(t *testing.T, quantity string) (Trade, routed.Order) {
	t.Helper()
	ctx := context.Background()
	venue := w.names[0]
	tr := w.trade(t, SideBuy, quantity, "60500")
	o := w.orders(t, tr.ID)[0]
	w.venues[venue].RejectNext("-1013")
	require.NoError(t, w.driver(venue, "node-a", nil).Drive(ctx, o.ID))
	o, err := routed.Read(ctx, w.DB, o.ID)
	require.NoError(t, err)
	require.Equal(t, routed.StatusManualReview, o.Status)
	return tr, o
}

func (w *routedWorld) audited(t *testing.T, orderID string) (n int) {
	t.Helper()
	require.NoError(t, w.DB.QueryRow(`SELECT count(*) FROM audit_events WHERE action = $1 AND target_type = $2 AND target_id = $3`,
		audit.ActionAdminOTCRoutedOrderResolve, audit.TargetOTCRoutedOrder, orderID).Scan(&n))
	return n
}

// TestReview_ListsAndRetriesAnOrderLeftForNoRoute covers #513: an order
// left to a review is listed with why and its transitions, a retry routes
// its remainder again as an order of a new attempt, audited, and the new
// order hedges the inventory.
func TestReview_ListsAndRetriesAnOrderLeftForNoRoute(t *testing.T) {
	ctx := context.Background()
	w := newRoutedWorld(t, "60000", "a")
	venue := w.names[0]
	tr, o := w.leftForNoRoute(t, "0.1")
	r := w.review()

	items, err := r.List(ctx, routed.Filter{Status: routed.StatusManualReview, Venue: venue})
	require.NoError(t, err)
	require.Len(t, items, 1)
	it := items[0]
	assert.Equal(t, o.ID, it.ID)
	assert.True(t, it.Sealed)
	assert.Nil(t, it.FindingID)
	assert.Equal(t, routed.ReasonNoRoute, *it.Reason)
	assertDec(t, "0.1", it.Remaining)
	var to []string
	for _, e := range it.Events {
		to = append(to, e.To)
	}
	assert.Equal(t, []string{routed.StatusPlanned, routed.StatusManualReview}, to)
	assert.False(t, it.Unsealed.NextAttemptAt.IsZero())

	items, err = r.List(ctx, routed.Filter{TradeID: tr.ID, Status: routed.StatusFilled})
	require.NoError(t, err)
	assert.Empty(t, items)

	origin := audit.Event{ActorType: audit.ActorAdmin, ActorID: w.user}
	res, err := r.Resolve(ctx, o.ID, routed.ResolveRetry, "venue back after maintenance", origin)
	require.NoError(t, err)
	assert.Equal(t, routed.StatusResolved, res.Order.Status)
	assert.Equal(t, routed.ReasonNoRoute, *res.Order.Reason, "the order keeps why it needed a person")
	require.Len(t, res.Children, 1)
	child := res.Children[0]
	assert.Equal(t, venue, child.Venue, "a retry may go to a venue the leg already went to")
	assert.Equal(t, 1, child.Attempt)
	assert.Equal(t, o.ID, *child.ParentID)
	assert.Equal(t, routed.StatusPlanned, child.Status)
	assert.Equal(t, 1, w.audited(t, o.ID))
	last := res.Order.Events[len(res.Order.Events)-1]
	assert.Equal(t, routed.StatusResolved, last.To)
	assert.Equal(t, routed.ResolveRetry, *last.Reason)
	assert.Equal(t, "admin:"+w.user, last.Node)

	_, err = r.Resolve(ctx, o.ID, routed.ResolveRetry, "twice", origin)
	require.ErrorIs(t, err, routed.ErrNotInReview, "a resolved order is not resolved again")
	assert.Equal(t, 1, w.audited(t, o.ID))

	require.NoError(t, w.driver(venue, "node-a", nil).Drive(ctx, child.ID))
	c, err := routed.Read(ctx, w.DB, child.ID)
	require.NoError(t, err)
	assert.Equal(t, routed.StatusFilled, c.Status)
	assert.Equal(t, "0", w.position(t, w.market.BaseHK), "the retry hedged what the trade took")
}

// TestReview_AnOrderThatMayBeOpenIsOnlyHandled covers the resolutions that
// a retry must not take: an order whose fills do not add up may still be
// open at the venue.
func TestReview_AnOrderThatMayBeOpenIsOnlyHandled(t *testing.T) {
	ctx := context.Background()
	w := newRoutedWorld(t, "60000", "a")
	venue := w.names[0]
	tr := w.trade(t, SideBuy, "0.1", "60500")
	o := w.orders(t, tr.ID)[0]
	require.NoError(t, w.driver(venue, "node-a", beyondLimit{w.venues[venue]}).Drive(ctx, o.ID))
	o, err := routed.Read(ctx, w.DB, o.ID)
	require.NoError(t, err)
	require.Equal(t, routed.StatusManualReview, o.Status)
	require.Equal(t, routed.ReasonBeyondLimit, *o.Reason)

	r := w.review()
	origin := audit.Event{ActorType: audit.ActorAdmin, ActorID: w.user}
	_, err = r.Resolve(ctx, o.ID, routed.ResolveRetry, "retry", origin)
	require.ErrorIs(t, err, routed.ErrNotRetryable)
	assert.Zero(t, w.audited(t, o.ID))

	res, err := r.Resolve(ctx, o.ID, routed.ResolveHandled, "cancelled at the venue and hedged by hand", origin)
	require.NoError(t, err)
	assert.Equal(t, routed.StatusResolved, res.Order.Status)
	assert.Empty(t, res.Children)
	assert.Equal(t, 1, w.audited(t, o.ID))

	// Without a planner, as an instance that does not route, a retry is
	// unavailable; handled still works.
	_, err = (&routed.Review{DB: w.DB}).Resolve(ctx, o.ID, routed.ResolveRetry, "x", origin)
	require.ErrorIs(t, err, routed.ErrRetryUnavailable)
}

// beyondLimit fills every order above its limit.
type beyondLimit struct{ venues.Venue }

func (b beyondLimit) GetOrder(ctx context.Context, symbol, clientID string) (venues.Order, error) {
	o, err := b.Venue.GetOrder(ctx, symbol, clientID)
	if err != nil {
		return o, err
	}
	o.Status = venues.OrderFilled
	o.Filled = o.Quantity
	o.FilledQuote = o.Quantity.Mul(o.Price).Mul(shared.MustNewDecimalFromString("2"))
	return o, nil
}

func (b beyondLimit) PlaceOrder(ctx context.Context, req venues.OrderRequest) (venues.Order, error) {
	if _, err := b.Venue.PlaceOrder(ctx, req); err != nil {
		return venues.Order{}, err
	}
	return b.GetOrder(ctx, req.Symbol, req.ClientID)
}

// TestReview_ATamperedOrderIsListedAndNotResolved covers the seal: an
// order changed in SQL is listed, flagged with its finding, and refused.
func TestReview_ATamperedOrderIsListedAndNotResolved(t *testing.T) {
	ctx := context.Background()
	w := newRoutedWorld(t, "60000", "a")
	_, o := w.leftForNoRoute(t, "0.1")
	_, err := w.DB.Exec(`UPDATE otc_routed_orders SET reason = 'attempts_exhausted' WHERE id = $1`, o.ID)
	require.NoError(t, err)

	r := w.review()
	items, err := r.List(ctx, routed.Filter{Venue: w.names[0]})
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.False(t, items[0].Sealed)
	require.NotNil(t, items[0].FindingID, "the read opened a finding")

	_, err = r.Resolve(ctx, o.ID, routed.ResolveHandled, "x", audit.Event{ActorType: audit.ActorAdmin, ActorID: w.user})
	require.ErrorIs(t, err, rowseal.ErrUnverified)
	assert.Zero(t, w.audited(t, o.ID))

	_, err = r.Resolve(ctx, "00000000-0000-0000-0000-000000000000", routed.ResolveHandled, "x", audit.Event{})
	require.ErrorIs(t, err, routed.ErrNotFound)
}

// TestReview_ALockdownRefusesARetry covers otc_routing: a retry places
// orders, which a lockdown stops.
func TestReview_ALockdownRefusesARetry(t *testing.T) {
	ctx := context.Background()
	w := newRoutedWorld(t, "60000", "a")
	_, o := w.leftForNoRoute(t, "0.1")
	c, err := platform.Create(ctx, w.DB, platform.New{Kind: platform.KindLockdown, Feature: platform.FeatureOTCRouting,
		Reason: "review test"}, audit.Event{})
	require.NoError(t, err)
	defer func() {
		_, err := platform.Lift(ctx, w.DB, platform.LiftRequest{HK: c.HK, Note: "done"}, audit.Event{})
		require.NoError(t, err)
	}()
	_, err = w.review().Resolve(ctx, o.ID, routed.ResolveRetry, "x", audit.Event{ActorType: audit.ActorAdmin, ActorID: w.user})
	var locked *platform.Locked
	require.ErrorAs(t, err, &locked)
	o, err = routed.Read(ctx, w.DB, o.ID)
	require.NoError(t, err)
	assert.Equal(t, routed.StatusManualReview, o.Status, "nothing changed")
}

// TestReview_Metrics covers the gauges of #513, by the venue of the test.
func TestReview_Metrics(t *testing.T) {
	ctx := context.Background()
	w := newRoutedWorld(t, "60000", "a")
	venue := w.names[0]
	_, o := w.leftForNoRoute(t, "0.1")
	filled := w.trade(t, SideBuy, "0.2", "60500")
	require.NoError(t, w.driver(venue, "node-a", nil).Drive(ctx, w.orders(t, filled.ID)[0].ID))
	w.trade(t, SideBuy, "0.3", "60500")

	gather := func() map[string][]*dto.Metric {
		reg := prometheus.NewRegistry()
		c := routed.NewCollector(w.DB)
		c.CacheFor = -1
		require.NoError(t, reg.Register(c))
		families, err := reg.Gather()
		require.NoError(t, err)
		out := map[string][]*dto.Metric{}
		for _, f := range families {
			for _, m := range f.GetMetric() {
				mine := len(m.GetLabel()) == 0
				for _, l := range m.GetLabel() {
					mine = mine || (l.GetName() == "venue" && l.GetValue() == venue)
				}
				if mine {
					out[f.GetName()] = append(out[f.GetName()], m)
				}
			}
		}
		return out
	}
	label := func(m *dto.Metric, name string) string {
		for _, l := range m.GetLabel() {
			if l.GetName() == name {
				return l.GetValue()
			}
		}
		return ""
	}
	got := gather()
	byStatus := map[string]float64{}
	for _, m := range got["otc_routed_orders"] {
		byStatus[label(m, "status")] = m.GetGauge().GetValue()
	}
	assert.Equal(t, map[string]float64{"manual_review": 1, "planned": 1}, byStatus)
	require.Len(t, got["otc_routed_order_oldest_seconds"], 2)
	unhedged := map[string]float64{}
	for _, m := range got["otc_routed_order_unhedged"] {
		assert.Equal(t, w.market.Quote, label(m, "quote"))
		unhedged[label(m, "status")] = m.GetGauge().GetValue()
	}
	// The remainder at the reference price of the quote.
	assert.InDelta(t, 0.1*60500, unhedged["manual_review"], 1)
	assert.InDelta(t, 0.3*60500, unhedged["planned"], 1)
	assert.Len(t, got["otc_routed_order_fill_seconds"], 3, "one fill in the window: 0.5, 0.95, and 1")
	require.Len(t, got["otc_routed_orders_unverified"], 1)
	before := got["otc_routed_orders_unverified"][0].GetGauge().GetValue()

	_, err := w.DB.Exec(`UPDATE otc_routed_orders SET quantity = 1 WHERE id = $1`, o.ID)
	require.NoError(t, err)
	_, err = routed.Read(ctx, w.DB, o.ID)
	require.ErrorIs(t, err, rowseal.ErrUnverified)
	assert.Equal(t, before+1, gather()["otc_routed_orders_unverified"][0].GetGauge().GetValue())
}
