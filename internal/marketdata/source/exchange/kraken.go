package exchange

import (
	"context"
	"fmt"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// krakenAliases are Kraken's own names of assets.
var krakenAliases = map[string]string{"XBT": "BTC", "XDG": "DOGE"}

// kraken reads /0/public/Ticker for every pair, and the base and quote of
// each pair from the wsname of /0/public/AssetPairs, kept for an hour.
// Kraken's ticker has no time, so the time it is read is its time; its
// open is that of the day, not of the last 24 hours, and is left out.
type kraken struct {
	base
	pairs pairCache
}

func (k *kraken) Name() string { return Kraken }

func (k *kraken) Venue() marketdata.Venue {
	return marketdata.Venue{ID: Kraken, Name: "Kraken", Kind: marketdata.VenueCEX, Region: marketdata.RegionGlobal,
		Country: "US", Website: "https://www.kraken.com", Attribution: "Data from Kraken", Enabled: true}
}

type krakenAnswer[T any] struct {
	Error  []string     `json:"error"`
	Result map[string]T `json:"result"`
}

func (k *kraken) Tickers(ctx context.Context) ([]source.Ticker, error) {
	pairs, err := k.pairs.get(k.now(), func() (map[string][2]string, error) {
		var body krakenAnswer[struct {
			WSName string `json:"wsname"`
			Status string `json:"status"`
		}]
		if err := k.client.GetJSON(ctx, "0/public/AssetPairs", nil, &body); err != nil {
			return nil, err
		}
		if len(body.Error) > 0 {
			return nil, fmt.Errorf("kraken: %s", strings.Join(body.Error, "; "))
		}
		out := map[string][2]string{}
		for name, p := range body.Result {
			baseSym, quoteSym, ok := strings.Cut(p.WSName, "/")
			if !ok || (p.Status != "" && p.Status != "online") {
				continue
			}
			out[name] = [2]string{symbol(baseSym, krakenAliases), symbol(quoteSym, krakenAliases)}
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	now := k.now().UTC()
	var body krakenAnswer[struct {
		A []any `json:"a"`
		B []any `json:"b"`
		C []any `json:"c"`
		V []any `json:"v"`
		L []any `json:"l"`
		H []any `json:"h"`
	}]
	if err := k.client.GetJSON(ctx, "0/public/Ticker", nil, &body); err != nil {
		return nil, err
	}
	if len(body.Error) > 0 {
		return nil, fmt.Errorf("kraken: %s", strings.Join(body.Error, "; "))
	}
	at := func(values []any, i int) any {
		if i < len(values) {
			return values[i]
		}
		return nil
	}
	out := make([]source.Ticker, 0, len(body.Result))
	for name, r := range body.Result {
		pair, ok := pairs[name]
		last := positive(at(r.C, 0))
		if !ok || last == nil {
			continue
		}
		// Index 1 of v, l, and h is the last 24 hours.
		t := source.Ticker{VenueSymbol: name, Base: pair[0], Quote: pair[1], Last: *last,
			Bid: positive(at(r.B, 0)), Ask: positive(at(r.A, 0)), VolumeBase: nonNegative(at(r.V, 1)),
			High: positive(at(r.H, 1)), Low: positive(at(r.L, 1)), ObservedAt: now}
		quoteSides(&t)
		out = append(out, t)
	}
	return out, nil
}
