package otc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/otc/routing"
	"github.com/mdastpak/crypto-exchange/internal/venues"
	"github.com/mdastpak/crypto-exchange/internal/venues/state"
	"github.com/mdastpak/crypto-exchange/internal/venues/stub"

	"github.com/stretchr/testify/require"
)

// TestService_LoadTestRouted is the load test of #61 with routing on
// (#509): each quote is routed over the inventory, bounded, and two venues
// read from their state in PostgreSQL, and priced again with the cost of
// its route, with a p99 under 25 ms. Its budget is its own: the baseline
// is the same test on the base branch.
func TestService_LoadTestRouted(t *testing.T) {
	if testing.Short() {
		t.Skip("the load test runs without -short")
	}
	ctx := context.Background()
	f := newDBFixture(t)
	f.DB.SetMaxOpenConns(50)
	f.DB.SetMaxIdleConns(50)
	store, err := NewStore(testRedis(t), f.keys)
	require.NoError(t, err)
	s, err := New(testSettings(), fakePrices{"BTC/USDT": live("65000")}, fakeMarkets{f.market}, f.checks, store, NewMetrics([]string{"BTC/USDT"}))
	require.NoError(t, err)

	venueState := state.New(f.DB, state.Options{MaxAge: time.Hour})
	settings := &config.Configuration{}
	settings.Venues.Exchanges = map[string]config.VenueConfig{}
	base, quote := f.market.Base, f.market.Quote
	rule := routing.Rule{Name: "load", UseInventory: true}
	for i, name := range []string{"binance", "nobitex"} {
		settings.Venues.Exchanges[name] = config.VenueConfig{Enabled: true, FeeBPS: 10, SlippageBPS: 5,
			Assets: map[string]config.VenueAsset{base: {Currency: base, Networks: map[string]string{"BTC": "bitcoin"}},
				quote: {Currency: quote, Networks: map[string]string{"TRX": "tron"}}}}
		v := stub.New(name)
		v.SetBalance(quote, shared.MustNewDecimalFromString("1000000"))
		v.SetBalance(base, shared.MustNewDecimalFromString("20"))
		v.AddInstrument(venues.Instrument{Symbol: base + quote, Base: base, Quote: quote, TickSize: shared.MustNewDecimalFromString("0.01"),
			LotSize: shared.MustNewDecimalFromString("0.001"), MinQuantity: shared.MustNewDecimalFromString("0.001"),
			MinNotional: shared.MustNewDecimalFromString("1"), Trading: true}, shared.MustNewDecimalFromString("65000"))
		v.AddNetwork(base, venues.Network{Network: "BTC", DepositEnabled: true, WithdrawEnabled: true,
			WithdrawFee: shared.MustNewDecimalFromString("0.00001")})
		v.AddNetwork(quote, venues.Network{Network: "TRX", DepositEnabled: true, WithdrawEnabled: true,
			WithdrawFee: shared.MustNewDecimalFromString("1")})
		balances, err := v.Balances(ctx)
		require.NoError(t, err)
		networks := map[string][]venues.Network{}
		for _, b := range balances {
			list, err := v.Networks(ctx, b.Asset)
			require.NoError(t, err)
			networks[b.Asset] = list
		}
		require.NoError(t, venueState.SaveNetworks(ctx, name, networks, time.Now()))
		instruments, err := v.Instruments(ctx)
		require.NoError(t, err)
		require.NoError(t, venueState.SaveInstruments(ctx, name, instruments, time.Now()))
		require.NoError(t, venueState.SaveBalances(ctx, name, balances, time.Now()))
		rule.Venues = append(rule.Venues, routing.VenueRule{Venue: name, Priority: i, MaxDailyNotional: "100000000"})
	}
	rules := []routing.Rule{rule}
	require.NoError(t, routing.Validate(rules, nil))
	// The inventory may sell a little of the base, so that the quotes take
	// the inventory and a venue, and are priced again with the venue's cost.
	short := shared.MustNewDecimalFromString("0.004").Decimal()
	router := &routing.Router{
		Rules:     fixedRules{routing.RuleSet{Version: 1, Rules: rules}},
		Venues:    routing.StateVenues{State: venueState, Config: func() *config.Configuration { return settings }},
		Inventory: HouseInventory{DB: f.DB, Limits: map[string]InventoryLimit{strings.ToUpper(base): {MaxShort: &short}}},
		Usage:     &routing.Store{DB: f.DB},
	}
	// As the server routes the quotes: on the venue state that each node
	// keeps for otc.routing.quote_state_ms.
	s.Route(&Routing{
		Router: router,
		Quotes: router.QuoteState(time.Duration(config.DefaultOTCRoutingQuoteStateMs) * time.Millisecond),
		Level:  func(context.Context, string) (int, error) { return 2, nil },
	})
	q, err := s.Quote(ctx, Request{Account: Account{UserHK: f.user}, Market: "BTC/USDT", Side: SideBuy, Quantity: ptr("0.01")})
	require.NoError(t, err)
	require.NotNil(t, q.Components.Route)
	require.Len(t, q.Components.Route.Legs, 2, "the quote takes the inventory and a venue")

	loadTest(t, "TestService_LoadTestRouted", s, Account{UserHK: f.user}, 25*time.Millisecond, false)
}
