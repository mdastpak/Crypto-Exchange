package otc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/otc/routing"
	"github.com/mdastpak/crypto-exchange/internal/region"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/venues/state"

	"github.com/shopspring/decimal"
)

// The codes of the refusals of routing (#386).
const (
	// CodeNoRoute is a quote that no routing rule routes; its limit is the
	// reason.
	CodeNoRoute = "otc_no_route"
	// CodeQuoteNotRoutable is a quote whose route at execution is gone, or
	// costs more than the quote charged for it.
	CodeQuoteNotRoutable = "quote_not_routable"
)

// Routing routes the quotes and their executions by the rules (#386).
type Routing struct {
	Router *routing.Router
	// Region is the region of the release, which the rules match as the
	// region of the user.
	Region region.Region
	// Level returns the verification level of a user (#157).
	Level func(ctx context.Context, userHK string) (int, error)
	// Now is the clock of the venues' daily limits; nil is time.Now.
	Now func() time.Time
}

func (r *Routing) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// input is the routing input of a trade.
func (r *Routing) input(ctx context.Context, userHK string, m currencies.PublicMarket, side Side, quantity, ref, lot decimal.Decimal,
	network string) (routing.Input, error) {
	level := 0
	if r.Level != nil {
		l, err := r.Level(ctx, userHK)
		if err != nil {
			return routing.Input{}, err
		}
		level = l
	}
	return routing.Input{
		Market: m.Symbol, Base: m.Base, Quote: m.Quote, BaseHK: m.BaseHK, QuoteHK: m.QuoteHK,
		Side: string(side), Quantity: quantity, Price: ref, Lot: lot,
		Region: r.Region, Level: level, Network: network, Day: r.now(),
	}, nil
}

// plan returns the route of a trade, or a *Refusal with code when there is
// none.
func (r *Routing) plan(ctx context.Context, in routing.Input, code string) (routing.Route, error) {
	route, err := r.Router.Plan(ctx, in)
	var none *routing.NoRoute
	if errors.As(err, &none) {
		return routing.Route{}, &Refusal{Code: code, Limit: none.Reason}
	}
	return route, err
}

// NewRouter returns the router of the trades: the rules of rules, the
// venues from their state (#385), the inventory within its limits (#62),
// and the daily usage of store.
func NewRouter(db *sql.DB, settings func() *config.Configuration, rules routing.RuleSource, store *routing.Store) (*routing.Router, error) {
	c := settings()
	limits, err := ParseInventoryLimits(c.OTC.InventoryLimits)
	if err != nil {
		return nil, err
	}
	return &routing.Router{
		Rules:     rules,
		Venues:    routing.StateVenues{State: state.New(db, state.OptionsOf(c.Venues.State)), Config: settings},
		Inventory: HouseInventory{DB: db, Limits: limits, RequireLimits: c.Server.Mode == "release"},
		Usage:     store,
	}, nil
}

// maxRouteSteps bounds the re-pricing of a quote with the cost of its
// route, which converges in a step or two.
const maxRouteSteps = 4

// HouseInventory is the Inventory of the router: what the exchange's own
// inventory may still give of a currency before its limit (#62), from the
// sealed positions.
type HouseInventory struct {
	DB            *sql.DB
	Limits        map[string]InventoryLimit
	RequireLimits bool
}

// Capacity implements routing.Inventory.
func (h HouseInventory) Capacity(ctx context.Context, currencyHK, symbol string) (decimal.Decimal, bool, error) {
	l, ok := h.Limits[strings.ToUpper(symbol)]
	if !ok || l.MaxShort == nil {
		if h.RequireLimits {
			return decimal.Zero, true, nil
		}
		return decimal.Zero, false, nil
	}
	var position shared.Decimal
	sealed := rowseal.OTCHousePositions.NewRow()
	err := h.DB.QueryRowContext(ctx, `SELECT position, `+rowseal.OTCHousePositions.Fields("p")+`
		FROM otc_house_positions p WHERE currency_hk = $1`, currencyHK).Scan(append([]any{&position}, sealed.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return *l.MaxShort, true, nil
	}
	if err != nil {
		return decimal.Zero, false, fmt.Errorf("otc: read the position of %s: %w", symbol, err)
	}
	if err := rowseal.Check(ctx, rowseal.OTCHousePositions, sealed); err != nil {
		return decimal.Zero, false, err
	}
	return position.Decimal().Add(*l.MaxShort), true, nil
}

// route plans the route of a quote of quantity and prices the quote with
// its cost: the quote is sized again with the cost, and the route planned
// again for the new quantity, until the cost charged covers the route.
func (s *Service) route(ctx context.Context, req Request, m currencies.PublicMarket, r rules, mid decimal.Decimal,
	size func(routeBPS decimal.Decimal) (decimal.Decimal, decimal.Decimal, Components, error),
	quantity, px decimal.Decimal, parts Components) (decimal.Decimal, decimal.Decimal, Components, error) {
	charged := decimal.Zero
	for range maxRouteSteps {
		in, err := s.routing.input(ctx, req.Account.UserHK, m, req.Side, quantity, mid, r.lot, req.Network)
		if err != nil {
			return quantity, px, parts, err
		}
		route, err := s.routing.plan(ctx, in, CodeNoRoute)
		if err != nil {
			return quantity, px, parts, err
		}
		if !route.CostBPS.GreaterThan(charged) {
			parts.Route = &route
			return quantity, px, parts, nil
		}
		charged = route.CostBPS
		if quantity, px, parts, err = size(charged); err != nil {
			return quantity, px, parts, err
		}
	}
	return quantity, px, parts, &Refusal{Code: CodeNoRoute, Limit: routing.ReasonCostTooHigh}
}

// reroute plans the route of a quote again at its execution, against the
// state of now, and refuses the quote when there is none, or when it
// costs more than the quote charged. The trade records the route of the
// execution, which the routed orders (#387) place.
func (e *Executor) reroute(ctx context.Context, q *Quote, m tradedMarket) error {
	quoted := q.Components.Route
	if quoted == nil {
		// Quoted before routing was switched on.
		return &Refusal{Code: CodeQuoteNotRoutable, Limit: "not_routed"}
	}
	in, err := e.Routing.input(ctx, q.UserHK, currencies.PublicMarket{
		Symbol: q.Market, Base: m.base, Quote: m.quote, BaseHK: m.baseHK, QuoteHK: m.quoteHK,
	}, q.Side, q.Quantity.Decimal(), q.Components.Reference, quoted.Lot, quoted.Network)
	if err != nil {
		return err
	}
	route, err := e.Routing.plan(ctx, in, CodeQuoteNotRoutable)
	if err != nil {
		return err
	}
	if route.CostBPS.GreaterThan(q.Components.RouteBPS) {
		return &Refusal{Code: CodeQuoteNotRoutable, Limit: routing.ReasonCostTooHigh}
	}
	q.Components.Route = &route
	return nil
}
