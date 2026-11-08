package exchange

import (
	"context"
	"errors"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// wallex reads /v1/markets, the markets with their 24-hour statistics.
// Toman markets (TMN) are reported as IRT. The statistics have no time,
// so the time they are read is theirs.
type wallex struct{ base }

func (w *wallex) Name() string { return Wallex }

func (w *wallex) Venue() marketdata.Venue {
	return marketdata.Venue{ID: Wallex, Name: "Wallex", Kind: marketdata.VenueCEX, Region: marketdata.RegionIran,
		Country: "IR", Website: "https://wallex.ir", Attribution: "Data from Wallex", Enabled: true}
}

func (w *wallex) Tickers(ctx context.Context) ([]source.Ticker, error) {
	now := w.now().UTC()
	var body struct {
		Success bool `json:"success"`
		Result  struct {
			Symbols map[string]struct {
				Symbol     string `json:"symbol"`
				BaseAsset  string `json:"baseAsset"`
				QuoteAsset string `json:"quoteAsset"`
				Stats      struct {
					BidPrice    any `json:"bidPrice"`
					AskPrice    any `json:"askPrice"`
					Volume      any `json:"24h_volume"`
					QuoteVolume any `json:"24h_quoteVolume"`
					High        any `json:"24h_highPrice"`
					Low         any `json:"24h_lowPrice"`
					LastPrice   any `json:"lastPrice"`
				} `json:"stats"`
			} `json:"symbols"`
		} `json:"result"`
	}
	if err := w.client.GetJSON(ctx, "v1/markets", nil, &body); err != nil {
		return nil, err
	}
	if !body.Success {
		return nil, errors.New("wallex: the answer is not a success")
	}
	out := make([]source.Ticker, 0, len(body.Result.Symbols))
	for key, m := range body.Result.Symbols {
		venueSymbol := m.Symbol
		if venueSymbol == "" {
			venueSymbol = key
		}
		last := positive(m.Stats.LastPrice)
		if m.BaseAsset == "" || m.QuoteAsset == "" || last == nil {
			continue
		}
		t := source.Ticker{VenueSymbol: venueSymbol, Base: symbol(m.BaseAsset, nil), Quote: symbol(m.QuoteAsset, map[string]string{"TMN": Toman}),
			Last: *last, Bid: positive(m.Stats.BidPrice), Ask: positive(m.Stats.AskPrice),
			VolumeBase: nonNegative(m.Stats.Volume), VolumeQuote: nonNegative(m.Stats.QuoteVolume),
			High: positive(m.Stats.High), Low: positive(m.Stats.Low), ObservedAt: now}
		quoteSides(&t)
		out = append(out, t)
	}
	return out, nil
}
