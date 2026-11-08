package exchange

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// binance reads /api/v3/ticker/24hr for every symbol, and the base and
// quote of each symbol from /api/v3/exchangeInfo, kept for an hour.
type binance struct {
	base
	pairs pairCache
}

func (b *binance) Name() string { return Binance }

func (b *binance) Venue() marketdata.Venue {
	return marketdata.Venue{ID: Binance, Name: "Binance", Kind: marketdata.VenueCEX, Region: marketdata.RegionGlobal,
		Website: "https://www.binance.com", Attribution: "Data from Binance", Enabled: true}
}

func (b *binance) Tickers(ctx context.Context) ([]source.Ticker, error) {
	pairs, err := b.pairs.get(b.now(), func() (map[string][2]string, error) {
		var info struct {
			Symbols []struct {
				Symbol     string `json:"symbol"`
				Status     string `json:"status"`
				BaseAsset  string `json:"baseAsset"`
				QuoteAsset string `json:"quoteAsset"`
			} `json:"symbols"`
		}
		if err := b.client.GetJSON(ctx, "api/v3/exchangeInfo", url.Values{"permissions": {"SPOT"}}, &info); err != nil {
			return nil, err
		}
		out := map[string][2]string{}
		for _, s := range info.Symbols {
			if s.Status == "TRADING" && s.BaseAsset != "" && s.QuoteAsset != "" {
				out[s.Symbol] = [2]string{symbol(s.BaseAsset, nil), symbol(s.QuoteAsset, nil)}
			}
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Symbol      string      `json:"symbol"`
		LastPrice   any         `json:"lastPrice"`
		BidPrice    any         `json:"bidPrice"`
		AskPrice    any         `json:"askPrice"`
		OpenPrice   any         `json:"openPrice"`
		HighPrice   any         `json:"highPrice"`
		LowPrice    any         `json:"lowPrice"`
		Volume      any         `json:"volume"`
		QuoteVolume any         `json:"quoteVolume"`
		CloseTime   json.Number `json:"closeTime"`
	}
	if err := b.client.GetJSON(ctx, "api/v3/ticker/24hr", nil, &rows); err != nil {
		return nil, err
	}
	out := make([]source.Ticker, 0, len(rows))
	for _, r := range rows {
		pair, ok := pairs[r.Symbol]
		last := positive(r.LastPrice)
		if !ok || last == nil || strings.TrimSpace(r.Symbol) == "" {
			continue
		}
		observed, err := source.UnixMillis(r.CloseTime)
		if err != nil {
			continue
		}
		t := source.Ticker{VenueSymbol: r.Symbol, Base: pair[0], Quote: pair[1], Last: *last,
			Bid: positive(r.BidPrice), Ask: positive(r.AskPrice),
			VolumeBase: nonNegative(r.Volume), VolumeQuote: nonNegative(r.QuoteVolume),
			High: positive(r.HighPrice), Low: positive(r.LowPrice), Open: positive(r.OpenPrice), ObservedAt: observed}
		quoteSides(&t)
		out = append(out, t)
	}
	return out, nil
}
