// Package price is the price engine of the exchange (#58): it follows the
// books and trades of each market on several providers (venues such as
// Binance, Coinbase, and Kraken), and aggregates them, every interval, into
// one reference price per market. The reference prices quote the OTC trades
// (#61) and feed the market-making bots (#369).
//
// Each provider is followed through its WebSocket stream, and through its
// REST API while the stream is down, or alone for a venue with no stream
// (the Iranian venues, which give the reference of the rial markets). At
// each interval the aggregator takes, per market, the book of every
// provider that is fresh (newer than the staleness cutoff), rejects the
// providers whose mid is an outlier by the median absolute deviation, and
// reports:
//
//   - the best bid and the best ask of the providers that remain;
//   - the mid, the median of their mids, which is the reference price;
//   - the VWAP of their trades and the TWAP of the mid over each window;
//   - the providers that make it, and those rejected.
//
// A market with fewer providers than the quorum (min_providers) is stale:
// it keeps the figures of its last live price, marked stale, so that no
// consumer takes them as current.
//
// Every amount is a decimal, never a floating-point number. Times are kept
// to the nanosecond, and reported to the microsecond.
//
// The engine keeps no shared state: every replica of cmd/price-aggregator
// follows the providers and computes the same prices on its own, so that a
// replica that dies loses nothing. See docs/price-engine.md.
package price

import (
	"fmt"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// Kind is what a tick reports.
type Kind int

// The kinds of ticks.
const (
	// KindBook is the best bid and ask of a market on a provider.
	KindBook Kind = iota + 1
	// KindTrade is a trade of a market on a provider.
	KindTrade
)

func (k Kind) String() string {
	switch k {
	case KindBook:
		return "book"
	case KindTrade:
		return "trade"
	}
	return "unknown"
}

// Tick is one observation of a provider.
type Tick struct {
	// Provider is the name of the provider.
	Provider string
	// Market is the market, BASE/QUOTE in upper case.
	Market string
	Kind   Kind
	// Bid and Ask are the best prices of a book tick.
	Bid, Ask shared.Decimal
	// Price and Size are the price and the base quantity of a trade tick.
	Price, Size shared.Decimal
	// At is the time of the observation at the provider; zero when the
	// provider gives none, and then Received counts.
	At time.Time
	// Received is when this process received it.
	Received time.Time
}

// time returns when the tick was observed: At, unless it is unknown or
// later than Received, since a provider's clock may run ahead of ours.
func (t Tick) time() time.Time {
	if t.At.IsZero() || t.At.After(t.Received) {
		return t.Received
	}
	return t.At
}

// Status is whether a price is current.
type Status string

// The statuses of a price.
const (
	// StatusLive is a price made by at least min_providers fresh providers.
	StatusLive Status = "live"
	// StatusStale is a market below the quorum: its figures are those of
	// its last live price, or zero when it never had one, and must not be
	// served as current.
	StatusStale Status = "stale"
	// StatusOverride is a price that an administrator set (#59). It is
	// declared with the others so that the OpenAPI document lists the
	// statuses in one order.
	StatusOverride Status = "override"
)

// Price is the reference price of a market at one interval.
type Price struct {
	Market string `json:"market"`
	Status Status `json:"status"`
	// Bid and Ask are the best bid and ask over the providers; they may
	// cross when the venues disagree.
	Bid shared.Decimal `json:"bid"`
	Ask shared.Decimal `json:"ask"`
	// Mid is the reference price: the median of the providers' mids.
	Mid shared.Decimal `json:"mid"`
	// VWAP and TWAP are by window, such as "60s"; a window without trades
	// has no VWAP.
	VWAP map[string]shared.Decimal `json:"vwap,omitempty"`
	TWAP map[string]shared.Decimal `json:"twap,omitempty"`
	// Sources are the providers of the figures, and Outliers those
	// rejected at this interval.
	Sources  []string `json:"sources"`
	Outliers []string `json:"outliers,omitempty"`
	// At is when the price was computed, and Updated when its newest input
	// was observed; for a stale price, Updated is that of its last live
	// price.
	At      time.Time `json:"at"`
	Updated time.Time `json:"updated"`
	// KeyID and Signature sign the price (#450): the id of the Ed25519 key
	// of the aggregator, and the signature of the canonical encoding of
	// every other field. A reader that verifies refuses a price without
	// them. See sign.go.
	KeyID     string `json:"kid,omitempty"`
	Signature string `json:"sig,omitempty"`
}

// Live reports whether the price may be served as current: a live price,
// or an administrator's override.
func (p Price) Live() bool { return p.Status == StatusLive || p.Status == StatusOverride }

// NormalizeMarket returns the market of a configured symbol: BASE/QUOTE or
// BASE-QUOTE in any case, with the aliases of the venues (XBT for BTC,
// XDG for DOGE) read as the assets they name.
func NormalizeMarket(symbol string) (string, error) {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	base, quote, ok := strings.Cut(s, "/")
	if !ok {
		base, quote, ok = strings.Cut(s, "-")
	}
	if !ok || !validAsset(base) || !validAsset(quote) || base == quote {
		return "", fmt.Errorf("symbol %q is not BASE/QUOTE or BASE-QUOTE", symbol)
	}
	return alias(base) + "/" + alias(quote), nil
}

// SplitMarket returns the base and the quote of a market.
func SplitMarket(market string) (base, quote string) {
	base, quote, _ = strings.Cut(market, "/")
	return base, quote
}

// aliases are the venues' own names of some assets.
var aliases = map[string]string{"XBT": "BTC", "XDG": "DOGE"}

func alias(asset string) string {
	if a, ok := aliases[asset]; ok {
		return a
	}
	return asset
}

// validAsset reports whether s is an asset symbol: 1 to 15 letters and
// digits.
func validAsset(s string) bool {
	if len(s) == 0 || len(s) > 15 {
		return false
	}
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// windowLabel names a window by its seconds, such as "60s", or its
// milliseconds below a second.
func windowLabel(d time.Duration) string {
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return fmt.Sprintf("%dms", d/time.Millisecond)
}
