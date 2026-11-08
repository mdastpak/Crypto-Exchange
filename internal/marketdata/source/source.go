// Package source defines the external sources of the market data catalog
// (#265): catalog sources, which describe assets (CoinGecko,
// CoinMarketCap), venue sources, which report the tickers of an exchange,
// and candle sources, which report its candles. The jobs see only these
// interfaces, so that a source can be added, its plan changed, or a stub put
// in its place without touching them.
//
// Every source calls its API through a Client, which keeps the workers
// together within the limits of the plan, retries what is worth retrying,
// stops calling a source that keeps failing, and takes the route of the
// node: directly or through a proxy.
package source

import (
	"context"
	"errors"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
)

// ErrNotFound is returned for an asset or a pair that the source does not
// know.
var ErrNotFound = errors.New("source: not found")

// Contract is the contract of a token on a network.
type Contract struct {
	// Network is the catalog's name of the network, such as "ethereum" or
	// "tron"; each source maps its own names to these.
	Network string
	Address string
}

// Asset is an asset as a catalog source describes it.
type Asset struct {
	// ExternalID is the asset's id at the source, such as "bitcoin".
	ExternalID string
	// Slug is the suggested slug of a new asset.
	Slug      string
	Details   marketdata.AssetDetails
	Contracts []Contract
}

// ListOptions select the assets of a listing.
type ListOptions struct {
	// Limit bounds the assets by market cap rank; 0 is every asset.
	Limit int
	// IDs, when set, selects these assets only, such as the curated list
	// of a development environment.
	IDs []string
}

// CatalogSource describes assets.
type CatalogSource interface {
	// Name is the source's name, such as "coingecko"; it names the
	// identities of its assets and its settings.
	Name() string
	// Assets lists assets with what the listing gives in bulk: symbol,
	// name, rank, and supply.
	Assets(ctx context.Context, opts ListOptions) ([]Asset, error)
	// Asset returns one asset in full, with its description, links, and
	// contracts, or ErrNotFound.
	Asset(ctx context.Context, externalID string) (Asset, error)
}

// Ticker is the latest ticker of a pair at a venue. The figures other than
// the last price are nil when the venue does not report them.
type Ticker struct {
	// VenueSymbol is the pair as the venue names it, such as BTCUSDT.
	VenueSymbol string
	// Base and Quote are the symbols of the pair, in upper case.
	Base, Quote string
	Last        shared.Decimal
	Bid, Ask    *shared.Decimal
	// The figures of the last 24 hours.
	VolumeBase, VolumeQuote *shared.Decimal
	High, Low, Open         *shared.Decimal
	// ObservedAt is the time of the ticker at the venue.
	ObservedAt time.Time
}

// VenueSource reports the tickers of one venue.
type VenueSource interface {
	// Name is the source's name, which is also the id of its venue.
	Name() string
	// Venue describes the venue.
	Venue() marketdata.Venue
	// Tickers returns the latest ticker of every pair of the venue.
	Tickers(ctx context.Context) ([]Ticker, error)
}

// Interval is the length of a candle.
type Interval string

// The intervals of candles.
const (
	Interval1m  Interval = "1m"
	Interval5m  Interval = "5m"
	Interval15m Interval = "15m"
	Interval1h  Interval = "1h"
	Interval1d  Interval = "1d"
)

// Duration is the length of the interval.
func (i Interval) Duration() time.Duration {
	switch i {
	case Interval1m:
		return time.Minute
	case Interval5m:
		return 5 * time.Minute
	case Interval15m:
		return 15 * time.Minute
	case Interval1h:
		return time.Hour
	case Interval1d:
		return 24 * time.Hour
	}
	return 0
}

// Candle is one OHLCV candle, which opens at OpenTime, in UTC.
type Candle struct {
	OpenTime               time.Time
	Open, High, Low, Close shared.Decimal
	// Volume is in the base asset.
	Volume shared.Decimal
}

// CandleSource reports the candles of the pairs of one venue.
type CandleSource interface {
	Name() string
	// Candles returns the closed candles of a pair that open in [from, to),
	// oldest first, at most MaxCandles of them.
	Candles(ctx context.Context, venueSymbol string, interval Interval, from, to time.Time) ([]Candle, error)
	// MaxCandles is the most candles of one request.
	MaxCandles() int
}
