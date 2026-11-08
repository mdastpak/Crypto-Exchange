// Package exchange holds the venue sources of the market data worker
// (#267): the public ticker endpoints of global exchanges (Binance, OKX,
// Kraken, Coinbase) and Iranian exchanges (Nobitex, Wallex). None needs a
// key. Each reports the latest ticker of every spot pair in one or two
// requests, so that a run of a venue costs few requests of its limits.
//
// The Iranian exchanges quote in rials or tomans; their prices are reported
// in tomans, as the quote IRT, so that the venues compare. A rial price is
// divided by 10, exactly, in decimals.
package exchange

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// The names of the venue sources, which are also the ids of their venues.
const (
	Binance  = "binance"
	OKX      = "okx"
	Kraken   = "kraken"
	Coinbase = "coinbase"
	Nobitex  = "nobitex"
	Wallex   = "wallex"
)

// Names lists the venue sources.
var Names = []string{Binance, OKX, Kraken, Coinbase, Nobitex, Wallex}

// settings are the public API addresses of the venues; none takes a key.
var settings = map[string]source.Settings{
	Binance:  {DefaultBaseURL: "https://api.binance.com"},
	OKX:      {DefaultBaseURL: "https://www.okx.com"},
	Kraken:   {DefaultBaseURL: "https://api.kraken.com"},
	Coinbase: {DefaultBaseURL: "https://api.coinbase.com"},
	Nobitex:  {DefaultBaseURL: "https://api.nobitex.ir"},
	Wallex:   {DefaultBaseURL: "https://api.wallex.ir"},
}

// Settings returns the settings of the named venue source.
func Settings(name string) (source.Settings, bool) {
	s, ok := settings[name]
	return s, ok
}

// New returns the named venue source on client.
func New(name string, client *source.Client) (source.VenueSource, error) {
	b := base{client: client, now: time.Now}
	switch name {
	case Binance:
		return &binance{base: b}, nil
	case OKX:
		return &okx{base: b}, nil
	case Kraken:
		return &kraken{base: b}, nil
	case Coinbase:
		return &coinbase{base: b}, nil
	case Nobitex:
		return &nobitex{base: b}, nil
	case Wallex:
		return &wallex{base: b}, nil
	}
	return nil, fmt.Errorf("exchange: unknown venue %q", name)
}

type base struct {
	client *source.Client
	now    func() time.Time
}

// pairsTTL is how long a venue's list of pairs is kept before it is read
// again, for the venues whose tickers do not name the base and quote.
const pairsTTL = time.Hour

// pairCache keeps the base and quote of each pair of a venue.
type pairCache struct {
	mu      sync.Mutex
	pairs   map[string][2]string
	fetched time.Time
}

func (c *pairCache) get(now time.Time, load func() (map[string][2]string, error)) (map[string][2]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pairs != nil && now.Sub(c.fetched) < pairsTTL {
		return c.pairs, nil
	}
	pairs, err := load()
	if err != nil {
		return nil, err
	}
	c.pairs, c.fetched = pairs, now
	return pairs, nil
}

// symbol returns a symbol in upper case, with the aliases of a venue
// replaced, such as Kraken's XBT for BTC.
func symbol(s string, aliases map[string]string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if alias, ok := aliases[s]; ok {
		return alias
	}
	return s
}

// positive returns the amount when it is a positive number, else nil.
func positive(v any) *shared.Decimal {
	d := source.Decimal(v)
	if d == nil || !d.Decimal().IsPositive() {
		return nil
	}
	return d
}

// nonNegative returns the amount when it is a number of zero or more.
func nonNegative(v any) *shared.Decimal {
	d := source.Decimal(v)
	if d == nil || d.IsNegative() {
		return nil
	}
	return d
}

// quoteSides drops a bid and an ask that cross, which a venue reports
// between two updates of its book.
func quoteSides(t *source.Ticker) {
	if t.Bid != nil && t.Ask != nil && t.Bid.Decimal().GreaterThan(t.Ask.Decimal()) {
		t.Bid, t.Ask = nil, nil
	}
}

// tomans returns a figure in rials in tomans: divided by 10, exactly.
func tomans(d *shared.Decimal) *shared.Decimal {
	if d == nil {
		return nil
	}
	v := shared.Decimal(d.Decimal().Shift(-1))
	return &v
}

// Toman is the quote symbol of the prices in tomans of the Iranian venues.
const Toman = "IRT"
