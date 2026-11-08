// Package stub holds sources that answer from fixed data, for tests and for
// development without network access or API keys. Their data covers the
// cases that the jobs must handle: a coin, a token on several networks, two
// different assets with one symbol, a rebranded token, and pairs with and
// without the optional figures.
package stub

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

func dec(s string) *shared.Decimal {
	d := shared.MustNewDecimalFromString(s)
	return &d
}

func rank(n int) *int { return &n }

// assets are the stub catalog, by rank.
var assets = []source.Asset{
	{ExternalID: "bitcoin", Slug: "bitcoin", Details: marketdata.AssetDetails{
		Symbol: "BTC", Name: "Bitcoin", Kind: marketdata.KindCoin, MarketCapRank: rank(1),
		Description:       "The first decentralized digital currency.",
		Links:             map[string]string{"website": "https://bitcoin.org"},
		LogoURL:           "https://assets.example.com/bitcoin.png",
		CirculatingSupply: dec("19750000"), MaxSupply: dec("21000000"), Attribution: "Stub data"}},
	{ExternalID: "ethereum", Slug: "ethereum", Details: marketdata.AssetDetails{
		Symbol: "ETH", Name: "Ethereum", Kind: marketdata.KindCoin, MarketCapRank: rank(2),
		CirculatingSupply: dec("120000000.123456789012345678"), Attribution: "Stub data"}},
	{ExternalID: "tether", Slug: "tether", Details: marketdata.AssetDetails{
		Symbol: "USDT", Name: "Tether", Kind: marketdata.KindToken, MarketCapRank: rank(3), Attribution: "Stub data"},
		Contracts: []source.Contract{
			{Network: "ethereum", Address: "0xdAC17F958D2ee523a2206206994597C13D831ec7"},
			{Network: "tron", Address: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"},
		}},
	{ExternalID: "polygon-ecosystem-token", Slug: "polygon-ecosystem-token", Details: marketdata.AssetDetails{
		Symbol: "POL", Name: "POL (ex-MATIC)", Kind: marketdata.KindToken, MarketCapRank: rank(20), Attribution: "Stub data"},
		Contracts: []source.Contract{{Network: "ethereum", Address: "0x455e53CBB86018Ac2B8092FdCd39d8444aFFC3F6"}}},
	// Two different assets that share a symbol.
	{ExternalID: "uniswap", Slug: "uniswap", Details: marketdata.AssetDetails{
		Symbol: "UNI", Name: "Uniswap", Kind: marketdata.KindToken, MarketCapRank: rank(25), Attribution: "Stub data"},
		Contracts: []source.Contract{{Network: "ethereum", Address: "0x1f9840a85d5aF5bf1D1762F925BDADdC4201F984"}}},
	{ExternalID: "unicorn-token", Slug: "unicorn-token", Details: marketdata.AssetDetails{
		Symbol: "UNI", Name: "Unicorn Token", Kind: marketdata.KindToken, Attribution: "Stub data"}},
}

// Catalog is a catalog source with the stub assets.
type Catalog struct{ name string }

// NewCatalog returns a stub catalog that names itself name, such as
// "coingecko" in place of the real source.
func NewCatalog(name string) *Catalog { return &Catalog{name: name} }

func (c *Catalog) Name() string { return c.name }

func (c *Catalog) Assets(ctx context.Context, opts source.ListOptions) ([]source.Asset, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []source.Asset
	for _, a := range assets {
		if len(opts.IDs) > 0 && !slices.Contains(opts.IDs, a.ExternalID) {
			continue
		}
		listed := a
		listed.Details.Description, listed.Details.Links, listed.Contracts = "", nil, nil
		out = append(out, listed)
		if opts.Limit > 0 && len(out) == opts.Limit {
			break
		}
	}
	return out, nil
}

func (c *Catalog) Asset(ctx context.Context, externalID string) (source.Asset, error) {
	if err := ctx.Err(); err != nil {
		return source.Asset{}, err
	}
	for _, a := range assets {
		if a.ExternalID == externalID {
			return a, nil
		}
	}
	return source.Asset{}, fmt.Errorf("stub %s: %s: %w", c.name, externalID, source.ErrNotFound)
}

// Venue is a venue source with a few pairs, which is also a candle source.
type Venue struct {
	venue marketdata.Venue
	now   func() time.Time
}

// NewVenue returns a stub venue with the id and region, such as
// ("stub_global", marketdata.RegionGlobal).
func NewVenue(id, region string) *Venue {
	return &Venue{venue: marketdata.Venue{ID: id, Name: "Stub " + id, Kind: marketdata.VenueCEX, Region: region,
		Attribution: "Stub data"}, now: time.Now}
}

func (v *Venue) Name() string            { return v.venue.ID }
func (v *Venue) Venue() marketdata.Venue { return v.venue }

func (v *Venue) Tickers(ctx context.Context) ([]source.Ticker, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := v.now().UTC().Truncate(time.Second)
	return []source.Ticker{
		{VenueSymbol: "BTCUSDT", Base: "BTC", Quote: "USDT", Last: *dec("68000.12"), Bid: dec("68000.1"), Ask: dec("68000.2"),
			VolumeBase: dec("1234.5"), VolumeQuote: dec("83950148.14"), High: dec("69000"), Low: dec("67000"), Open: dec("67500"),
			ObservedAt: now},
		{VenueSymbol: "ETHUSDT", Base: "ETH", Quote: "USDT", Last: *dec("3500.5"), ObservedAt: now},
		// A pair with an amount that only a decimal keeps.
		{VenueSymbol: "SHIBUSDT", Base: "SHIB", Quote: "USDT", Last: *dec("0.000012345678901234"), ObservedAt: now},
	}, nil
}

func (v *Venue) MaxCandles() int { return 500 }

// Candles returns deterministic candles: a price that rises by one per
// candle from 100.
func (v *Venue) Candles(ctx context.Context, venueSymbol string, interval source.Interval, from, to time.Time) ([]source.Candle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	step := interval.Duration()
	if step == 0 {
		return nil, fmt.Errorf("stub %s: unknown interval %q", v.venue.ID, interval)
	}
	if venueSymbol != "BTCUSDT" && venueSymbol != "ETHUSDT" {
		return nil, fmt.Errorf("stub %s: %s: %w", v.venue.ID, venueSymbol, source.ErrNotFound)
	}
	closed := v.now().UTC().Truncate(step)
	var out []source.Candle
	for t := from.UTC().Truncate(step); t.Before(to) && t.Before(closed) && len(out) < v.MaxCandles(); t = t.Add(step) {
		if t.Before(from) {
			continue
		}
		n := shared.NewDecimalFromInt64(100 + t.Unix()/int64(step.Seconds())%1000)
		one := shared.NewDecimalFromInt64(1)
		out = append(out, source.Candle{OpenTime: t, Open: n, High: n.Add(one).Add(one), Low: n.Sub(one), Close: n.Add(one), Volume: *dec("10.5")})
	}
	return out, nil
}
