package exchange

import (
	"context"
	"fmt"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// nobitex reads /market/stats, the 24-hour statistics of every market,
// keyed as btc-rls and btc-usdt. Rial markets are reported in tomans
// (IRT). The statistics have no time, so the time they are read is theirs.
type nobitex struct{ base }

func (n *nobitex) Name() string { return Nobitex }

func (n *nobitex) Venue() marketdata.Venue {
	return marketdata.Venue{ID: Nobitex, Name: "Nobitex", Kind: marketdata.VenueCEX, Region: marketdata.RegionIran,
		Country: "IR", Website: "https://nobitex.ir", Attribution: "Data from Nobitex", Enabled: true}
}

func (n *nobitex) Tickers(ctx context.Context) ([]source.Ticker, error) {
	now := n.now().UTC()
	var body struct {
		Status string `json:"status"`
		Stats  map[string]struct {
			IsClosed  bool `json:"isClosed"`
			BestSell  any  `json:"bestSell"`
			BestBuy   any  `json:"bestBuy"`
			VolumeSrc any  `json:"volumeSrc"`
			VolumeDst any  `json:"volumeDst"`
			Latest    any  `json:"latest"`
			DayLow    any  `json:"dayLow"`
			DayHigh   any  `json:"dayHigh"`
			DayOpen   any  `json:"dayOpen"`
		} `json:"stats"`
	}
	if err := n.client.GetJSON(ctx, "market/stats", nil, &body); err != nil {
		return nil, err
	}
	if body.Status != "ok" {
		return nil, fmt.Errorf("nobitex: status %q", body.Status)
	}
	out := make([]source.Ticker, 0, len(body.Stats))
	for market, s := range body.Stats {
		baseSym, quoteSym, ok := strings.Cut(market, "-")
		last := positive(s.Latest)
		if !ok || baseSym == "" || quoteSym == "" || s.IsClosed || last == nil {
			continue
		}
		t := source.Ticker{VenueSymbol: market, Base: symbol(baseSym, nil), Quote: symbol(quoteSym, nil), Last: *last,
			Bid: positive(s.BestBuy), Ask: positive(s.BestSell), VolumeBase: nonNegative(s.VolumeSrc),
			VolumeQuote: nonNegative(s.VolumeDst), High: positive(s.DayHigh), Low: positive(s.DayLow),
			Open: positive(s.DayOpen), ObservedAt: now}
		if t.Quote == "RLS" {
			t.Quote = Toman
			t.Last = *tomans(&t.Last)
			t.Bid, t.Ask, t.VolumeQuote = tomans(t.Bid), tomans(t.Ask), tomans(t.VolumeQuote)
			t.High, t.Low, t.Open = tomans(t.High), tomans(t.Low), tomans(t.Open)
		}
		quoteSides(&t)
		out = append(out, t)
	}
	return out, nil
}
