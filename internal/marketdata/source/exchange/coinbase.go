package exchange

import (
	"context"
	"net/url"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// coinbase reads the public spot products of the Advanced Trade API,
// /api/v3/brokerage/market/products, which give the last price and the
// 24-hour volume of every product in one request. They have no time, so
// the time they are read is their time.
type coinbase struct{ base }

func (c *coinbase) Name() string { return Coinbase }

func (c *coinbase) Venue() marketdata.Venue {
	return marketdata.Venue{ID: Coinbase, Name: "Coinbase", Kind: marketdata.VenueCEX, Region: marketdata.RegionGlobal,
		Country: "US", Website: "https://www.coinbase.com", Attribution: "Data from Coinbase", Enabled: true}
}

func (c *coinbase) Tickers(ctx context.Context) ([]source.Ticker, error) {
	now := c.now().UTC()
	var body struct {
		Products []struct {
			ProductID       string `json:"product_id"`
			Price           any    `json:"price"`
			Volume24h       any    `json:"volume_24h"`
			BaseCurrencyID  string `json:"base_currency_id"`
			QuoteCurrencyID string `json:"quote_currency_id"`
			Status          string `json:"status"`
			TradingDisabled bool   `json:"trading_disabled"`
			IsDisabled      bool   `json:"is_disabled"`
		} `json:"products"`
	}
	if err := c.client.GetJSON(ctx, "api/v3/brokerage/market/products", url.Values{"product_type": {"SPOT"}}, &body); err != nil {
		return nil, err
	}
	out := make([]source.Ticker, 0, len(body.Products))
	for _, p := range body.Products {
		last := positive(p.Price)
		if p.ProductID == "" || p.BaseCurrencyID == "" || p.QuoteCurrencyID == "" || last == nil ||
			p.TradingDisabled || p.IsDisabled || (p.Status != "" && p.Status != "online") {
			continue
		}
		out = append(out, source.Ticker{VenueSymbol: p.ProductID, Base: symbol(p.BaseCurrencyID, nil),
			Quote: symbol(p.QuoteCurrencyID, nil), Last: *last, VolumeBase: nonNegative(p.Volume24h), ObservedAt: now})
	}
	return out, nil
}
