package otc

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/metrics"
	"github.com/mdastpak/crypto-exchange/internal/otc/routing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fixedRules struct{ set routing.RuleSet }

func (f fixedRules) Active(context.Context) (routing.RuleSet, error) { return f.set, nil }

// oneVenue is binance with a market of BTC/USDT at a fee and slippage of
// 30 basis points, and fundsUSDT of USDT.
type oneVenue struct{ fundsUSDT string }

func (v oneVenue) Market(context.Context, string, string, string) (routing.VenueMarket, error) {
	return routing.VenueMarket{Symbol: "BTCUSDT", Lot: dec("0.001"), FeeBPS: 20, SlippageBPS: 10}, nil
}

func (v oneVenue) Funds(context.Context, string, string, string) (routing.Funds, error) {
	return routing.Funds{Available: dec(v.fundsUSDT)}, nil
}

func withRouting(t *testing.T, f serviceFixture, funds string) *Routing {
	t.Helper()
	rules := []routing.Rule{{Name: "binance", Venues: []routing.VenueRule{{Venue: "binance"}}}}
	require.NoError(t, routing.Validate(rules, nil))
	r := &Routing{
		Router: &routing.Router{Rules: fixedRules{routing.RuleSet{Version: 3, Rules: rules}}, Venues: oneVenue{funds}},
		Level:  func(context.Context, string) (int, error) { return 2, nil },
	}
	f.Route(r)
	return r
}

// TestService_PricesTheRouteAndRefusesWithoutOne covers "no route, no
// quote" of #386, and a price that includes the cost of the route.
func TestService_PricesTheRouteAndRefusesWithoutOne(t *testing.T) {
	ctx := context.Background()
	f := newService(t)
	withRouting(t, f, "100000")
	alice := Account{UserHK: shared.NewUUID().String()}
	q, err := f.Quote(ctx, Request{Account: alice, Market: "BTC/USDT", Side: SideBuy, Quantity: ptr("0.5")})
	require.NoError(t, err)
	// 65000 × (1 + 50 + 30 bps) = 65520.
	assertDec(t, "65520", q.Price.Decimal())
	assertDec(t, "30", q.Components.RouteBPS)
	require.NotNil(t, q.Components.Route)
	assert.Equal(t, int64(3), q.Components.Route.RuleVersion)
	assert.Equal(t, "binance", q.Components.Route.Legs[0].Venue)
	assertDec(t, "0.5", q.Components.Route.Legs[0].Quantity)

	got, err := f.Store().Take(ctx, q.ID, alice.UserHK, nil, time.Now())
	require.NoError(t, err)
	require.NotNil(t, got.Components.Route, "the route is kept with the quote")
	want, err := json.Marshal(q.Components.Route)
	require.NoError(t, err)
	kept, err := json.Marshal(got.Components.Route)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(kept))

	// A quote by amount stays within the amount with the cost.
	byAmount, err := f.Quote(ctx, Request{Account: alice, Market: "BTC/USDT", Side: SideBuy, QuoteAmount: ptr("10000")})
	require.NoError(t, err)
	assert.False(t, byAmount.Total.Decimal().GreaterThan(dec("10000")))
	assertDec(t, "30", byAmount.Components.RouteBPS)

	// The venue's funds buy about 0.15 BTC: no route for 0.5.
	f2 := newService(t)
	withRouting(t, f2, "10000")
	_, err = f2.Quote(ctx, Request{Account: alice, Market: "BTC/USDT", Side: SideBuy, Quantity: ptr("0.5")})
	assert.Equal(t, CodeNoRoute, refusalCode(t, err))
	var r *Refusal
	require.ErrorAs(t, err, &r)
	assert.Equal(t, routing.ReasonInsufficientLiquidity, r.Limit)
}

// TestService_WithoutRoutingQuotesAsBefore leaves the components of a
// quote without routing as they were, so that the trades recorded before
// keep their seal.
func TestService_WithoutRoutingQuotesAsBefore(t *testing.T) {
	f := newService(t)
	q, err := f.Quote(context.Background(), Request{Account: Account{UserHK: shared.NewUUID().String()}, Market: "BTC/USDT",
		Side: SideBuy, Quantity: ptr("0.5")})
	require.NoError(t, err)
	assert.True(t, q.Components.RouteBPS.IsZero())
	assert.Nil(t, q.Components.Route)
	assertDec(t, "65325", q.Price.Decimal())
}

func TestHouseInventory_UnboundedWithoutALimit(t *testing.T) {
	ctx := context.Background()
	h := HouseInventory{}
	_, bounded, err := h.Capacity(ctx, shared.NewUUID().String(), "BTC")
	require.NoError(t, err)
	assert.False(t, bounded)
	h.RequireLimits = true
	c, bounded, err := h.Capacity(ctx, shared.NewUUID().String(), "BTC")
	require.NoError(t, err)
	assert.True(t, bounded)
	assert.True(t, c.Equal(decimal.Zero))
}

// TestExecutor_RoutesAgainAtExecution covers the execution of #386: the
// route is planned again against the state of now, the trade records it,
// and a quote is refused when its route is gone or costs more than it
// charged.
func TestExecutor_RoutesAgainAtExecution(t *testing.T) {
	f := newExecFixture(t)
	short := dec("0.5")
	limits := map[string]InventoryLimit{f.market.Base: {MaxShort: &short}}
	f.exec.Limits = limits
	rules := []routing.Rule{{Name: "own", UseInventory: true}}
	require.NoError(t, routing.Validate(rules, nil))
	f.exec.Routing = &Routing{Router: &routing.Router{
		Rules:     fixedRules{routing.RuleSet{Version: 9, Rules: rules}},
		Inventory: HouseInventory{DB: f.DB, Limits: limits},
	}}
	routedQuote := func(quantity, fee string, routeBPS string) Quote {
		q := f.quote(t, SideBuy, quantity, "60000", fee)
		q.Components.RouteBPS = dec(routeBPS)
		q.Components.Route = &routing.Route{RuleVersion: 9, Rule: "own", Lot: dec("0.001")}
		require.NoError(t, f.store.Save(context.Background(), q))
		return q
	}

	out, err := f.execute(routedQuote("0.4", "48", "0"), "routed")
	require.NoError(t, err)
	var route routing.Route
	var raw []byte
	require.NoError(t, f.DB.QueryRow(`SELECT components->'route' FROM otc_trades WHERE id = $1`, out.Trade.ID).Scan(&raw))
	require.NoError(t, json.Unmarshal(raw, &route))
	assert.Equal(t, int64(9), route.RuleVersion)
	require.Len(t, route.Legs, 1)
	assert.Equal(t, routing.SourceInventory, route.Legs[0].Source)
	assertDec(t, "0.4", route.Legs[0].Quantity)

	// The inventory has 0.1 left under its limit.
	_, err = f.execute(routedQuote("0.2", "24", "0"), "gone")
	assert.Equal(t, CodeQuoteNotRoutable, refusalCode(t, err))

	// A quote given before routing was on.
	_, err = f.execute(f.quote(t, SideBuy, "0.05", "60000", "6"), "unrouted")
	assert.Equal(t, CodeQuoteNotRoutable, refusalCode(t, err))

	// A route that costs more now than the quote charged.
	f.exec.Routing.Router = &routing.Router{
		Rules:  fixedRules{routing.RuleSet{Version: 10, Rules: []routing.Rule{{Name: "binance", Venues: []routing.VenueRule{{Venue: "binance"}}}}}},
		Venues: oneVenue{"100000"},
	}
	require.NoError(t, routing.Validate(f.exec.Routing.Router.Rules.(fixedRules).set.Rules, nil))
	_, err = f.execute(routedQuote("0.05", "6", "10"), "dearer")
	var r *Refusal
	require.ErrorAs(t, err, &r)
	assert.Equal(t, CodeQuoteNotRoutable, r.Code)
	assert.Equal(t, routing.ReasonCostTooHigh, r.Limit)
	// Covered, and refused while nothing places its venue leg (#387).
	_, err = f.execute(routedQuote("0.05", "6", "30"), "covered")
	require.ErrorAs(t, err, &r)
	assert.Equal(t, CodeQuoteNotRoutable, r.Code)
	assert.Equal(t, "no_routed_orders", r.Limit)
}

// countedVenue is oneVenue that counts its reads.
type countedVenue struct {
	oneVenue
	reads *atomic.Int64
}

func (v countedVenue) Market(ctx context.Context, venue, base, quote string) (routing.VenueMarket, error) {
	v.reads.Add(1)
	return v.oneVenue.Market(ctx, venue, base, quote)
}

func (v countedVenue) Funds(ctx context.Context, venue, currency, network string) (routing.Funds, error) {
	v.reads.Add(1)
	return v.oneVenue.Funds(ctx, venue, currency, network)
}

// TestService_AQuoteReadsTheStateOnce covers #509: a quote re-priced with
// the cost of its route reads the user's level and each venue's state
// once, and counts its routing on the metrics; a quote with no route
// counts its reason.
func TestService_AQuoteReadsTheStateOnce(t *testing.T) {
	ctx := context.Background()
	f := newService(t)
	r := withRouting(t, f, "100000")
	var reads, levels atomic.Int64
	r.Router.Venues = countedVenue{oneVenue: oneVenue{"100000"}, reads: &reads}
	r.Level = func(context.Context, string) (int, error) { levels.Add(1); return 2, nil }
	routed := testutil.ToFloat64(metrics.OTCRoutes.WithLabelValues(stageQuote, "binance", "routed"))
	legs := testutil.ToFloat64(metrics.OTCRouteLegs.WithLabelValues(stageQuote, routing.SourceVenue, "binance"))
	alice := Account{UserHK: shared.NewUUID().String()}

	q, err := f.Quote(ctx, Request{Account: alice, Market: "BTC/USDT", Side: SideBuy, Quantity: ptr("0.5")})
	require.NoError(t, err)
	assertDec(t, "30", q.Components.RouteBPS, "the quote was re-priced with its route")
	assert.Equal(t, int64(1), levels.Load(), "the level is read once per quote")
	// The market, the funds of the quote currency that it spends, and of the
	// base that it gets: once each, however many times the router ran.
	assert.LessOrEqual(t, reads.Load(), int64(3), "each venue's state is read once per quote")
	assert.Equal(t, routed+1, testutil.ToFloat64(metrics.OTCRoutes.WithLabelValues(stageQuote, "binance", "routed")))
	assert.Equal(t, legs+1, testutil.ToFloat64(metrics.OTCRouteLegs.WithLabelValues(stageQuote, routing.SourceVenue, "binance")))

	before := reads.Load()
	_, err = f.Quote(ctx, Request{Account: alice, Market: "BTC/USDT", Side: SideBuy, Quantity: ptr("0.5")})
	require.NoError(t, err)
	assert.Greater(t, reads.Load(), before, "the next quote reads the state again")

	none := testutil.ToFloat64(metrics.OTCRoutes.WithLabelValues(stageQuote, "none", routing.ReasonInsufficientLiquidity))
	r.Router.Venues = oneVenue{"10000"}
	_, err = f.Quote(ctx, Request{Account: alice, Market: "BTC/USDT", Side: SideBuy, Quantity: ptr("0.5")})
	assert.Equal(t, CodeNoRoute, refusalCode(t, err))
	assert.Equal(t, none+1, testutil.ToFloat64(metrics.OTCRoutes.WithLabelValues(stageQuote, "none", routing.ReasonInsufficientLiquidity)))
}
