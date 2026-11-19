package otc

import (
	"context"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/otc/routed"
	"github.com/mdastpak/crypto-exchange/internal/venues"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRouted_AnOrderPastTheBookIsRoutedElsewhereAtOnce covers the
// pre-check of #515: an order whose limit the venue's book already passed
// is never placed, and its quantity goes to the next venue without waiting
// for its deadline.
func TestRouted_AnOrderPastTheBookIsRoutedElsewhereAtOnce(t *testing.T) {
	ctx := context.Background()
	w := newRoutedWorld(t, "60000", "a", "b")
	a, b := w.names[0], w.names[1]
	symbol := w.market.Base + w.market.Quote
	tr := w.trade(t, SideBuy, "0.5", "60500")
	o := w.orders(t, tr.ID)[0]
	require.Equal(t, a, o.Venue)
	// a's book moved past the limit, 60439.56.
	w.venues[a].SetPrice(symbol, shared.MustNewDecimalFromString("61000"))
	w.venues[a].ShowBook()

	require.NoError(t, w.driver(a, "node-a", nil).Drive(ctx, o.ID))
	o, err := routed.Read(ctx, w.DB, o.ID)
	require.NoError(t, err)
	assert.Equal(t, routed.StatusFailed, o.Status)
	assert.Equal(t, routed.ReasonNotMarketable, *o.Reason)
	_, err = w.venues[a].GetOrder(ctx, symbol, o.ClientID)
	require.ErrorIs(t, err, venues.ErrNotFound, "nothing was placed at a")
	assert.False(t, w.reserved(t, o.ID))

	list := w.orders(t, tr.ID)
	require.Len(t, list, 2)
	child := list[1]
	assert.Equal(t, b, child.Venue)
	assertDec(t, "0.5", child.Quantity)
	w.venues[b].ShowBook()
	require.NoError(t, w.driver(b, "node-b", nil).Drive(ctx, child.ID))
	child, err = routed.Read(ctx, w.DB, child.ID)
	require.NoError(t, err)
	assert.Equal(t, routed.StatusFilled, child.Status, "b's book is within the limit")
	assert.Equal(t, "0", w.position(t, w.market.BaseHK))
}

// TestRouted_ASellPastTheBookIsLeftToAReview covers a sell with the bid
// below its limit at the only venue: no other venue takes it.
func TestRouted_ASellPastTheBookIsLeftToAReview(t *testing.T) {
	ctx := context.Background()
	w := newRoutedWorld(t, "60000", "a")
	a := w.names[0]
	bought := w.trade(t, SideBuy, "0.5", "60500")
	require.NoError(t, w.driver(a, "node-a", nil).Drive(ctx, w.orders(t, bought.ID)[0].ID))
	tr := w.trade(t, SideSell, "0.2", "59500")
	o := w.orders(t, tr.ID)[0]
	w.venues[a].SetPrice(w.market.Base+w.market.Quote, shared.MustNewDecimalFromString("59000"))
	w.venues[a].ShowBook()
	require.NoError(t, w.driver(a, "node-a", nil).Drive(ctx, o.ID))
	o, err := routed.Read(ctx, w.DB, o.ID)
	require.NoError(t, err)
	assert.Equal(t, routed.StatusManualReview, o.Status)
	assert.Equal(t, routed.ReasonNoRoute, *o.Reason)
}

// TestInventoryCollector_ShowsAPositionPastItsLimit covers the bound of
// #515: a venue leg left to the inventory puts its position past the
// limits, which the gauges show.
func TestInventoryCollector_ShowsAPositionPastItsLimit(t *testing.T) {
	ctx := context.Background()
	w := newRoutedWorld(t, "60000", "a")
	tr := w.trade(t, SideBuy, "0.1", "60500")
	o := w.orders(t, tr.ID)[0]
	w.venues[w.names[0]].RejectNext("-1013")
	require.NoError(t, w.driver(w.names[0], "node-a", nil).Drive(ctx, o.ID))
	require.Equal(t, "-0.1", w.position(t, w.market.BaseHK))

	short := decimal.RequireFromString("0.05")
	c := NewInventoryCollector(w.DB, map[string]InventoryLimit{w.market.Base: {MaxShort: &short}})
	c.CacheFor = -1
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(c))
	families, err := reg.Gather()
	require.NoError(t, err)
	values := map[string]float64{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			mine := false
			bound := ""
			for _, l := range m.GetLabel() {
				mine = mine || l.GetName() == "currency" && l.GetValue() == w.market.Base
				if l.GetName() == "bound" {
					bound = l.GetValue()
				}
			}
			if mine {
				values[f.GetName()+bound] = m.GetGauge().GetValue()
			}
		}
	}
	assert.InDelta(t, -0.1, values["otc_inventory_position"], 1e-9)
	assert.InDelta(t, 0.05, values["otc_inventory_limitmax_short"], 1e-9)
	assert.Equal(t, 1.0, values["otc_inventory_beyond_limit"])
}

func TestBeyond(t *testing.T) {
	one, two := decimal.NewFromInt(1), decimal.NewFromInt(2)
	l := InventoryLimit{MaxShort: &one, MaxLong: &two}
	assert.False(t, Beyond(decimal.NewFromInt(-1), l))
	assert.True(t, Beyond(decimal.RequireFromString("-1.5"), l))
	assert.False(t, Beyond(two, l))
	assert.True(t, Beyond(decimal.NewFromInt(3), l))
	assert.False(t, Beyond(decimal.NewFromInt(100), InventoryLimit{}), "no bound")
}
