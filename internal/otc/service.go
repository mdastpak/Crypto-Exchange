package otc

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/domain/market"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/price"

	"github.com/shopspring/decimal"
)

// Prices are the reference prices (price.Reader), which fail closed: a
// stale price is an error, never a price.
type Prices interface {
	Get(ctx context.Context, market string) (price.Price, error)
}

// Markets are the public markets (currencies.MarketList).
type Markets interface {
	List(ctx context.Context) ([]currencies.PublicMarket, error)
}

// Checker refuses a quote that the user may not take: their verification
// level, their restrictions and security hold, the capabilities of the
// two currencies, and a balance under need, of the currency that the user
// gives. It returns the refusal of the package that refuses, or
// ErrInsufficientBalance.
type Checker interface {
	Check(ctx context.Context, a Account, m currencies.PublicMarket, side Side, need decimal.Decimal) error
}

// Precisions returns the precision of a currency, as the ledger keeps it
// (ledger.Precision). A Checker that implements it bounds the quotes to
// it; DBChecks does.
type Precisions interface {
	Precision(ctx context.Context, currencyHK string) (int32, error)
}

// Account is the account that asks for a quote.
type Account struct {
	UserHK       string
	SubAccountID *string
}

// Request asks for a quote of Quantity of the base currency, or of what
// QuoteAmount of the quote currency buys or sells; exactly one is set.
type Request struct {
	Account     Account
	Market      string
	Side        Side
	Quantity    *decimal.Decimal
	QuoteAmount *decimal.Decimal
	// Network is our network of the user's destination (#57), when the
	// trade has one, which the routing rules match (#386).
	Network string
}

// Refusal is a quote that is not given, with the code of the API.
type Refusal struct {
	Code string
	// Limit is the bound that the request crossed, when it crossed one.
	Limit string
}

func (r *Refusal) Error() string {
	if r.Limit != "" {
		return fmt.Sprintf("otc: %s (%s)", r.Code, r.Limit)
	}
	return "otc: " + r.Code
}

// The codes of the refusals.
const (
	CodeMarketNotQuoted       = "otc_market_unavailable"
	CodeMarketInactive        = "market_inactive"
	CodePriceUnavailable      = "price_unavailable"
	CodeQuantityBelowMinimum  = "quantity_below_minimum"
	CodeQuantityAboveMaximum  = "quantity_above_maximum"
	CodeNotionalBelowMinimum  = "notional_below_minimum"
	CodeQuoteTooLarge         = "quote_too_large"
	CodeInsufficientBalance   = "insufficient_balance"
	CodeInvalidQuoteRequest   = "validation_failed"
	codeInvalidQuoteAmountMsg = "quantity or quote_amount, exactly one, positive"
)

// ErrInsufficientBalance is a balance under what the quote needs.
var ErrInsufficientBalance = &Refusal{Code: CodeInsufficientBalance}

// Service gives the quotes.
type Service struct {
	prices  Prices
	markets Markets
	checks  Checker
	store   *Store
	ttl     time.Duration
	pricing map[string]Pricing
	metrics *Metrics
	now     func() time.Time
	routing *Routing
}

// Route routes the quotes by r (#386): each quote is priced with the cost
// of its route, and refused when there is none. Nil quotes every trade
// from the inventory.
func (s *Service) Route(r *Routing) { s.routing = r }

// New returns the service of the quotes of the markets that settings
// prices. metrics may be nil.
func New(settings config.OTCConfig, prices Prices, markets Markets, checks Checker, store *Store, metrics *Metrics) (*Service, error) {
	s := &Service{
		prices: prices, markets: markets, checks: checks, store: store, metrics: metrics, now: time.Now,
		ttl: time.Duration(settings.QuoteTTLSeconds) * time.Second, pricing: map[string]Pricing{},
	}
	if s.ttl <= 0 {
		s.ttl = config.DefaultOTCQuoteTTLSeconds * time.Second
	}
	for _, symbol := range slices.Sorted(maps.Keys(settings.Markets)) {
		p, err := ParsePricing(symbol, settings.Markets[symbol])
		if err != nil {
			return nil, err
		}
		s.pricing[symbol] = p
	}
	return s, nil
}

// Markets returns the symbols of the quoted markets.
func (s *Service) Markets() []string { return slices.Sorted(maps.Keys(s.pricing)) }

// Store returns the store of the quotes, which the execution takes them
// from (#62).
func (s *Service) Store() *Store { return s.store }

// rules are the parsed rules of a market.
type rules struct {
	min, max, minNotional, tick, lot decimal.Decimal
}

func parseRules(m currencies.PublicMarket) (rules, error) {
	var r rules
	for _, f := range []struct {
		s    string
		into *decimal.Decimal
	}{{m.MinOrderSize, &r.min}, {m.MaxOrderSize, &r.max}, {m.MinNotional, &r.minNotional}, {m.TickSize, &r.tick}, {m.LotSize, &r.lot}} {
		if f.s == "" {
			continue
		}
		d, err := decimal.NewFromString(f.s)
		if err != nil {
			return r, fmt.Errorf("otc: the market %s has the rule %q: %w", m.Symbol, f.s, err)
		}
		*f.into = d
	}
	return r, nil
}

// Quote prices a request and stores the quote, which the account may take
// until it expires. It refuses with a *Refusal, or with the refusal of the
// checks.
func (s *Service) Quote(ctx context.Context, req Request) (q Quote, err error) {
	start := time.Now()
	defer func() { s.metrics.observe(req.Market, start, err) }()
	if !req.Side.Valid() || (req.Quantity == nil) == (req.QuoteAmount == nil) ||
		(req.Quantity != nil && !req.Quantity.IsPositive()) || (req.QuoteAmount != nil && !req.QuoteAmount.IsPositive()) {
		return Quote{}, &Refusal{Code: CodeInvalidQuoteRequest, Limit: codeInvalidQuoteAmountMsg}
	}
	symbol, err := price.NormalizeMarket(req.Market)
	if err != nil {
		return Quote{}, &Refusal{Code: CodeMarketNotQuoted}
	}
	pricing, ok := s.pricing[symbol]
	if !ok {
		return Quote{}, &Refusal{Code: CodeMarketNotQuoted}
	}
	m, err := s.market(ctx, symbol)
	if err != nil {
		return Quote{}, err
	}
	r, err := parseRules(m)
	if err != nil {
		return Quote{}, err
	}
	ref, err := s.prices.Get(ctx, pricing.Reference)
	if errors.Is(err, price.ErrNoPrice) || errors.Is(err, price.ErrStale) {
		return Quote{}, &Refusal{Code: CodePriceUnavailable}
	}
	if err != nil {
		return Quote{}, err
	}
	mid := ref.Mid.Decimal().Mul(pricing.ReferenceMultiplier)
	if !mid.IsPositive() {
		return Quote{}, &Refusal{Code: CodePriceUnavailable}
	}
	feePlaces := int32(max(0, -r.tick.Exponent()))
	var basePlaces, quotePlaces *int32
	if p, ok := s.checks.(Precisions); ok {
		// The amounts stay within the precision of their currencies
		// (#422), which the ledger refuses finer: the quantity is cut to
		// the base currency's, and the total rounded against the user to
		// the quote currency's.
		b, err := p.Precision(ctx, m.BaseHK)
		if err != nil {
			return Quote{}, err
		}
		places, err := p.Precision(ctx, m.QuoteHK)
		if err != nil {
			return Quote{}, err
		}
		basePlaces, quotePlaces, feePlaces = &b, &places, min(feePlaces, places)
	}
	size := func(routeBPS decimal.Decimal) (decimal.Decimal, decimal.Decimal, Components, error) {
		quantity, px, parts, err := solve(pricing, r, req, mid, routeBPS)
		if err != nil {
			return quantity, px, parts, err
		}
		if basePlaces != nil {
			if quantity = quantity.Truncate(*basePlaces); !quantity.IsPositive() {
				return quantity, px, parts, &Refusal{Code: CodeQuantityBelowMinimum, Limit: decimal.New(1, -*basePlaces).String()}
			}
		}
		return quantity, px, parts, nil
	}
	quantity, px, parts, err := size(decimal.Zero)
	if err != nil {
		return Quote{}, err
	}
	if s.routing != nil {
		if quantity, px, parts, err = s.route(ctx, req, m, r, mid, size, quantity, px, parts); err != nil {
			return Quote{}, err
		}
	}
	if ref.Override != nil {
		parts.OverrideID = ref.Override.ID
	}
	if err := checkSize(r, quantity, px); err != nil {
		return Quote{}, err
	}
	total := quantity.Mul(px)
	if quotePlaces != nil {
		if req.Side == SideBuy {
			total = total.RoundUp(*quotePlaces)
		} else {
			total = total.Truncate(*quotePlaces)
		}
	}
	fee := total.Mul(decimal.NewFromInt(int64(m.TakerFeeBPS))).Div(bpsUnit).RoundUp(feePlaces)
	if req.Side == SideSell && !total.GreaterThan(fee) {
		return Quote{}, &Refusal{Code: CodeNotionalBelowMinimum, Limit: fee.String()}
	}
	need := quantity
	if req.Side == SideBuy {
		need = total.Add(fee)
	}
	if s.checks != nil {
		if err := s.checks.Check(ctx, req.Account, m, req.Side, need); err != nil {
			return Quote{}, err
		}
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	q = Quote{
		ID: shared.NewUUID().String(), UserHK: req.Account.UserHK, SubAccountID: req.Account.SubAccountID,
		Market: symbol, Side: req.Side, Quantity: shared.Decimal(quantity), Price: shared.Decimal(px),
		Total: shared.Decimal(total), Fee: shared.Decimal(fee), FeeBPS: m.TakerFeeBPS, FeeCurrency: m.Quote,
		CreatedAt: now, ExpiresAt: now.Add(s.ttl), Components: parts,
	}
	if err := s.store.Save(ctx, q); err != nil {
		return Quote{}, err
	}
	s.metrics.quoted(symbol, req.Side, parts)
	return q, nil
}

// market returns the public market symbol, which must trade.
func (s *Service) market(ctx context.Context, symbol string) (currencies.PublicMarket, error) {
	list, err := s.markets.List(ctx)
	if err != nil {
		return currencies.PublicMarket{}, fmt.Errorf("otc: reading the markets: %w", err)
	}
	for _, m := range list {
		if m.Symbol != symbol {
			continue
		}
		if m.Status != string(market.MarketStatusActive) {
			return m, &Refusal{Code: CodeMarketInactive}
		}
		return m, nil
	}
	return currencies.PublicMarket{}, &Refusal{Code: CodeMarketNotQuoted}
}

// solve returns the quantity, the rounded price, and its parts. A request
// by quantity rounds it down to the lot. A request by quote amount buys or
// sells the most lots whose total stays within it: the quantity at the
// reference is a bound, since the price of a buy only rises with the
// quantity, and it is lowered by a lot while the total is above.
func solve(p Pricing, r rules, req Request, mid, routeBPS decimal.Decimal) (decimal.Decimal, decimal.Decimal, Components, error) {
	price := func(q decimal.Decimal) (decimal.Decimal, Components, error) {
		parts, err := p.Components(mid, q)
		parts.RouteBPS = routeBPS
		if errors.Is(err, ErrTooLarge) {
			return decimal.Zero, parts, &Refusal{Code: CodeQuoteTooLarge}
		}
		if err != nil {
			return decimal.Zero, parts, err
		}
		px, err := parts.Price(req.Side)
		if err != nil {
			return decimal.Zero, parts, &Refusal{Code: CodePriceUnavailable}
		}
		return RoundPrice(px, r.tick, req.Side), parts, nil
	}
	if req.Quantity != nil {
		q := FloorTo(*req.Quantity, r.lot)
		if !q.IsPositive() {
			return q, decimal.Zero, Components{}, &Refusal{Code: CodeQuantityBelowMinimum, Limit: r.lot.String()}
		}
		px, parts, err := price(q)
		return q, px, parts, err
	}
	amount := *req.QuoteAmount
	// A first estimate at the reference, then at the price of that
	// quantity, which converges in a step or two since the parts change
	// slowly with the quantity.
	q := FloorTo(amount.Div(mid), r.lot)
	var px decimal.Decimal
	var parts Components
	for range 4 {
		if !q.IsPositive() {
			return q, decimal.Zero, Components{}, &Refusal{Code: CodeQuantityBelowMinimum, Limit: r.lot.String()}
		}
		var err error
		if px, parts, err = price(q); err != nil {
			return q, px, parts, err
		}
		next := FloorTo(amount.Div(px), r.lot)
		if next.Equal(q) {
			break
		}
		q = next
	}
	// The rounding of the lot and the price may leave the total a lot
	// above the amount; step down until it fits.
	step := r.lot
	if !step.IsPositive() {
		step = decimal.New(1, q.Exponent())
	}
	for i := 0; q.Mul(px).GreaterThan(amount) && i < 4; i++ {
		q = q.Sub(step)
		if !q.IsPositive() {
			return q, decimal.Zero, Components{}, &Refusal{Code: CodeQuantityBelowMinimum, Limit: r.lot.String()}
		}
		var err error
		if px, parts, err = price(q); err != nil {
			return q, px, parts, err
		}
	}
	if q.Mul(px).GreaterThan(amount) {
		return q, px, parts, &Refusal{Code: CodeQuantityBelowMinimum, Limit: r.lot.String()}
	}
	return q, px, parts, nil
}

// checkSize refuses a quantity outside the bounds of the market, and a
// notional under its minimum.
func checkSize(r rules, quantity, px decimal.Decimal) error {
	if r.min.IsPositive() && quantity.LessThan(r.min) {
		return &Refusal{Code: CodeQuantityBelowMinimum, Limit: r.min.String()}
	}
	if r.max.IsPositive() && quantity.GreaterThan(r.max) {
		return &Refusal{Code: CodeQuantityAboveMaximum, Limit: r.max.String()}
	}
	if r.minNotional.IsPositive() && quantity.Mul(px).LessThan(r.minNotional) {
		return &Refusal{Code: CodeNotionalBelowMinimum, Limit: r.minNotional.String()}
	}
	return nil
}
