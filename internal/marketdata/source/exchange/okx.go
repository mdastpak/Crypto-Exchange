package exchange

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// okx reads /api/v5/market/tickers of the spot instruments, whose ids name
// the base and quote (BTC-USDT).
type okx struct{ base }

func (o *okx) Name() string { return OKX }

func (o *okx) Venue() marketdata.Venue {
	return marketdata.Venue{ID: OKX, Name: "OKX", Kind: marketdata.VenueCEX, Region: marketdata.RegionGlobal,
		Website: "https://www.okx.com", Attribution: "Data from OKX", Enabled: true}
}

func (o *okx) Tickers(ctx context.Context) ([]source.Ticker, error) {
	var body struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			InstID    string `json:"instId"`
			Last      any    `json:"last"`
			BidPx     any    `json:"bidPx"`
			AskPx     any    `json:"askPx"`
			Open24h   any    `json:"open24h"`
			High24h   any    `json:"high24h"`
			Low24h    any    `json:"low24h"`
			Vol24h    any    `json:"vol24h"`
			VolCcy24h any    `json:"volCcy24h"`
			TS        any    `json:"ts"`
		} `json:"data"`
	}
	if err := o.client.GetJSON(ctx, "api/v5/market/tickers", url.Values{"instType": {"SPOT"}}, &body); err != nil {
		return nil, err
	}
	if body.Code != "0" {
		return nil, fmt.Errorf("okx: %s %s", body.Code, body.Msg)
	}
	out := make([]source.Ticker, 0, len(body.Data))
	for _, r := range body.Data {
		baseSym, quoteSym, ok := strings.Cut(r.InstID, "-")
		last := positive(r.Last)
		if !ok || baseSym == "" || quoteSym == "" || last == nil {
			continue
		}
		observed, err := source.UnixMillis(r.TS)
		if err != nil {
			continue
		}
		// For spot, vol24h is in the base currency and volCcy24h in the
		// quote currency.
		t := source.Ticker{VenueSymbol: r.InstID, Base: symbol(baseSym, nil), Quote: symbol(quoteSym, nil), Last: *last,
			Bid: positive(r.BidPx), Ask: positive(r.AskPx), VolumeBase: nonNegative(r.Vol24h), VolumeQuote: nonNegative(r.VolCcy24h),
			High: positive(r.High24h), Low: positive(r.Low24h), Open: positive(r.Open24h), ObservedAt: observed}
		quoteSides(&t)
		out = append(out, t)
	}
	return out, nil
}
