package exchange

import (
	"context"
	"fmt"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// ramzinex reads exchange/api/v1.0/exchange/pairs, every pair with its best
// buy and sell and its 24-hour figures (#443). Rial pairs (irr) are
// reported in tomans (IRT). The figures have no time, so the time they are
// read is theirs.
type ramzinex struct{ base }

func (r *ramzinex) Name() string { return Ramzinex }

func (r *ramzinex) Venue() marketdata.Venue {
	return marketdata.Venue{ID: Ramzinex, Name: "Ramzinex", Kind: marketdata.VenueCEX, Region: marketdata.RegionIran,
		Country: "IR", Website: "https://ramzinex.com", Attribution: "Data from Ramzinex", Enabled: true}
}

func (r *ramzinex) Tickers(ctx context.Context) ([]source.Ticker, error) {
	now := r.now().UTC()
	var body struct {
		Status any `json:"status"`
		Data   []struct {
			PairID    any `json:"pair_id"`
			URLName   any `json:"url_name"`
			Base      any `json:"base_currency_symbol"`
			Quote     any `json:"quote_currency_symbol"`
			Buy       any `json:"buy"`
			Sell      any `json:"sell"`
			ShowOrder any `json:"show_order"`
			Financial struct {
				Last24h struct {
					Open        any `json:"open"`
					Close       any `json:"close"`
					Highest     any `json:"highest"`
					Lowest      any `json:"lowest"`
					BaseVolume  any `json:"base_volume"`
					QuoteVolume any `json:"quote_volume"`
				} `json:"last24h"`
			} `json:"financial"`
		} `json:"data"`
	}
	if err := r.client.GetJSON(ctx, "exchange/api/v1.0/exchange/pairs", nil, &body); err != nil {
		return nil, err
	}
	if s := fmt.Sprint(body.Status); s != "0" && s != "<nil>" {
		return nil, fmt.Errorf("ramzinex: status %s", s)
	}
	out := make([]source.Ticker, 0, len(body.Data))
	for _, p := range body.Data {
		baseSym, quoteSym := currencySymbol(p.Base), currencySymbol(p.Quote)
		day := p.Financial.Last24h
		bid, ask := positive(p.Buy), positive(p.Sell)
		last := positive(day.Close)
		if last == nil && bid != nil && ask != nil {
			mid := midOf(*bid, *ask)
			last = &mid
		}
		if baseSym == "" || quoteSym == "" || last == nil {
			continue
		}
		venueSymbol := fmt.Sprint(p.PairID)
		if p.PairID == nil {
			venueSymbol = strings.ToLower(baseSym + quoteSym)
		}
		t := source.Ticker{VenueSymbol: venueSymbol, Base: symbol(baseSym, nil), Quote: symbol(quoteSym, nil), Last: *last,
			Bid: bid, Ask: ask, VolumeBase: nonNegative(day.BaseVolume), VolumeQuote: nonNegative(day.QuoteVolume),
			High: positive(day.Highest), Low: positive(day.Lowest), Open: positive(day.Open), ObservedAt: now}
		if t.Quote == "IRR" {
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

// currencySymbol reads a symbol of Ramzinex, which is a string or an object
// of its names by language, such as {"fa": "بیت کوین", "en": "btc"}.
func currencySymbol(v any) string {
	switch s := v.(type) {
	case string:
		return strings.TrimSpace(s)
	case map[string]any:
		if en, ok := s["en"].(string); ok {
			return strings.TrimSpace(en)
		}
	}
	return ""
}
