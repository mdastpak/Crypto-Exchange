package price

import (
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// The reasons that a tick is rejected, the values of the reason label of
// price_ticks_rejected_total.
const (
	RejectUnknownMarket = "unknown_market"
	RejectInvalid       = "invalid"
	RejectCrossed       = "crossed_book"
	RejectOutOfOrder    = "out_of_order"
	RejectTooOld        = "too_old"
)

// Options configure an aggregator.
type Options struct {
	// Markets are the markets it prices; a tick of another is rejected.
	Markets []string
	// Windows are the lengths of the VWAP and TWAP windows.
	Windows []time.Duration
	// OutlierMADs is the number of median absolute deviations past which
	// a provider's mid is an outlier.
	OutlierMADs decimal.Decimal
	// OutlierFloor is the smallest deviation, as a fraction of the median,
	// that can make an outlier.
	OutlierFloor decimal.Decimal
	// MinProviders is the quorum of a live price.
	MinProviders int
	// Stale is the age past which a provider's book no longer counts, and
	// ProviderStale overrides it per provider, for a venue that is read
	// every few seconds through its REST API.
	Stale         time.Duration
	ProviderStale map[string]time.Duration
}

// Defaults of Options.
const (
	DefaultMinProviders = 2
	DefaultStale        = 5 * time.Second
	// bucketsPerWindow is the resolution of the shortest window.
	bucketsPerWindow = 10
	minBucket        = 10 * time.Millisecond
)

// DefaultOutlierMADs and DefaultOutlierFloor are the defaults of the
// outlier rule: 3 MADs, and no deviation under 0.1 % of the median.
var (
	DefaultOutlierMADs  = decimal.NewFromInt(3)
	DefaultOutlierFloor = decimal.New(1, -3)
)

// Aggregator keeps the latest book and the recent trades of each market on
// each provider, and computes the prices. It is safe for concurrent use:
// the providers ingest while the loop computes.
type Aggregator struct {
	opts   Options
	labels []string
	bucket time.Duration

	mu      sync.Mutex
	markets map[string]*marketState
}

type marketState struct {
	providers map[string]*providerState
	// twap holds the mid × time and the time of the live intervals.
	twap       *windows
	lastSample time.Time
	// last is the last live price.
	last *Price
}

type providerState struct {
	bid, ask decimal.Decimal
	bookAt   time.Time
	hasBook  bool
	trades   *windows
}

// NewAggregator returns an aggregator of the markets.
func NewAggregator(opts Options) (*Aggregator, error) {
	if len(opts.Markets) == 0 {
		return nil, errors.New("price: no market to aggregate")
	}
	if len(opts.Windows) == 0 {
		return nil, errors.New("price: no window")
	}
	if opts.MinProviders <= 0 {
		opts.MinProviders = DefaultMinProviders
	}
	if opts.Stale <= 0 {
		opts.Stale = DefaultStale
	}
	if !opts.OutlierMADs.IsPositive() {
		opts.OutlierMADs = DefaultOutlierMADs
	}
	if opts.OutlierFloor.IsNegative() {
		opts.OutlierFloor = DefaultOutlierFloor
	}
	shortest := slices.Min(opts.Windows)
	if shortest <= 0 {
		return nil, errors.New("price: a window must be positive")
	}
	a := &Aggregator{opts: opts, bucket: max(shortest/bucketsPerWindow, minBucket), markets: map[string]*marketState{}}
	for _, w := range opts.Windows {
		if w%a.bucket != 0 {
			return nil, errors.New("price: each window must be a multiple of a tenth of the shortest one")
		}
		a.labels = append(a.labels, windowLabel(w))
	}
	for _, m := range opts.Markets {
		a.markets[m] = &marketState{providers: map[string]*providerState{}, twap: newWindows(a.bucket, opts.Windows)}
	}
	return a, nil
}

// Ingest adds a tick, and returns the reason it was rejected, or "".
func (a *Aggregator) Ingest(t Tick) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	m, ok := a.markets[t.Market]
	if !ok {
		return RejectUnknownMarket
	}
	p := m.providers[t.Provider]
	if p == nil {
		p = &providerState{trades: newWindows(a.bucket, a.opts.Windows)}
		m.providers[t.Provider] = p
	}
	at := t.time()
	switch t.Kind {
	case KindBook:
		bid, ask := t.Bid.Decimal(), t.Ask.Decimal()
		if !bid.IsPositive() || !ask.IsPositive() {
			return RejectInvalid
		}
		if bid.GreaterThan(ask) {
			return RejectCrossed
		}
		if p.hasBook && at.Before(p.bookAt) {
			return RejectOutOfOrder
		}
		p.bid, p.ask, p.bookAt, p.hasBook = bid, ask, at, true
	case KindTrade:
		price, size := t.Price.Decimal(), t.Size.Decimal()
		if !price.IsPositive() || !size.IsPositive() {
			return RejectInvalid
		}
		if at.Before(t.Received.Add(-slices.Max(a.opts.Windows))) {
			return RejectTooOld
		}
		p.trades.add(at, price.Mul(size), size)
	default:
		return RejectInvalid
	}
	return ""
}

// Compute returns the price of every market at now, in the order of the
// markets' names.
func (a *Aggregator) Compute(now time.Time) []Price {
	a.mu.Lock()
	defer a.mu.Unlock()
	names := slices.Sorted(maps.Keys(a.markets))
	out := make([]Price, 0, len(names))
	for _, name := range names {
		out = append(out, a.compute(name, a.markets[name], now))
	}
	return out
}

func (a *Aggregator) stale(provider string) time.Duration {
	if d, ok := a.opts.ProviderStale[provider]; ok && d > 0 {
		return d
	}
	return a.opts.Stale
}

func (a *Aggregator) compute(name string, m *marketState, now time.Time) Price {
	at := now.Truncate(time.Microsecond)
	// The fresh providers, in the order of their names.
	var fresh []string
	var mids []decimal.Decimal
	for _, provider := range slices.Sorted(maps.Keys(m.providers)) {
		p := m.providers[provider]
		p.trades.advance(now)
		if !p.hasBook || now.Sub(p.bookAt) > a.stale(provider) {
			continue
		}
		fresh = append(fresh, provider)
		mids = append(mids, p.bid.Add(p.ask).Div(two))
	}
	rejected := outliers(mids, a.opts.OutlierMADs, a.opts.OutlierFloor)
	var sources, outlying []string
	var accepted []decimal.Decimal
	var bid, ask decimal.Decimal
	var updated time.Time
	for i, provider := range fresh {
		if rejected[i] {
			outlying = append(outlying, provider)
			continue
		}
		p := m.providers[provider]
		if len(sources) == 0 || p.bid.GreaterThan(bid) {
			bid = p.bid
		}
		if len(sources) == 0 || p.ask.LessThan(ask) {
			ask = p.ask
		}
		if p.bookAt.After(updated) {
			updated = p.bookAt
		}
		sources = append(sources, provider)
		accepted = append(accepted, mids[i])
	}

	m.twap.advance(now)
	if len(sources) < a.opts.MinProviders {
		// Below the quorum: the last live figures, marked stale. The time
		// below the quorum is not part of the TWAP.
		m.lastSample = time.Time{}
		price := Price{Market: name, Status: StatusStale, Sources: nonNilList(sources), Outliers: outlying, At: at}
		if m.last != nil {
			price.Bid, price.Ask, price.Mid = m.last.Bid, m.last.Ask, m.last.Mid
			price.VWAP, price.TWAP, price.Updated = m.last.VWAP, m.last.TWAP, m.last.Updated
		}
		return price
	}

	mid := median(accepted)
	if !m.lastSample.IsZero() {
		dt := decimal.NewFromInt(int64(now.Sub(m.lastSample)))
		m.twap.add(now, mid.Mul(dt), dt)
	}
	m.lastSample = now
	price := Price{
		Market: name, Status: StatusLive,
		Bid: shared.Decimal(bid), Ask: shared.Decimal(ask), Mid: shared.Decimal(mid),
		Sources: sources, Outliers: outlying, At: at, Updated: updated.Truncate(time.Microsecond),
	}
	for k, label := range a.labels {
		var trades sum
		for _, provider := range sources {
			trades.add(m.providers[provider].trades.sum(k))
		}
		if v, ok := average(trades); ok {
			if price.VWAP == nil {
				price.VWAP = map[string]shared.Decimal{}
			}
			price.VWAP[label] = shared.Decimal(v)
		}
		if v, ok := average(m.twap.sum(k)); ok {
			if price.TWAP == nil {
				price.TWAP = map[string]shared.Decimal{}
			}
			price.TWAP[label] = shared.Decimal(v)
		}
	}
	last := price
	m.last = &last
	return price
}

func nonNilList(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
