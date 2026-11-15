package exchange

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// tabdeal reads the Binance-like API of Tabdeal (#443): api/v1/exchangeInfo
// for the pairs, and api/v1/depth for the best bid and ask of each, since
// it documents no ticker of every pair. Its toman pairs are quoted in IRT
// already. A read of every pair costs one request per pair, so Tickers
// reads at most tabdealMaxPairs of the toman and USDT pairs, and the price
// engine reads only its own markets through PairTickers.
type tabdeal struct {
	base
	pairs pairCache
}

const (
	// tabdealMaxPairs bounds the depths that one read of the tickers asks
	// for.
	tabdealMaxPairs = 60
	// tabdealParallel bounds the depth requests in flight at once.
	tabdealParallel = 4
)

func (d *tabdeal) Name() string { return Tabdeal }

func (d *tabdeal) Venue() marketdata.Venue {
	return marketdata.Venue{ID: Tabdeal, Name: "Tabdeal", Kind: marketdata.VenueCEX, Region: marketdata.RegionIran,
		Country: "IR", Website: "https://tabdeal.org", Attribution: "Data from Tabdeal", Enabled: true}
}

func (d *tabdeal) Tickers(ctx context.Context) ([]source.Ticker, error) {
	pairs, err := d.loadPairs(ctx)
	if err != nil {
		return nil, err
	}
	symbols := make([]string, 0, len(pairs))
	for s, p := range pairs {
		if p[1] == Toman || p[1] == "USDT" {
			symbols = append(symbols, s)
		}
	}
	slices.Sort(symbols)
	if len(symbols) > tabdealMaxPairs {
		symbols = symbols[:tabdealMaxPairs]
	}
	return d.depths(ctx, symbols, pairs)
}

// PairTickers reads the tickers of the named pairs only, as base and quote,
// such as {"BTC", "IRT"}.
func (d *tabdeal) PairTickers(ctx context.Context, wanted [][2]string) ([]source.Ticker, error) {
	pairs, err := d.loadPairs(ctx)
	if err != nil {
		return nil, err
	}
	var symbols []string
	for s, p := range pairs {
		if slices.Contains(wanted, p) {
			symbols = append(symbols, s)
		}
	}
	slices.Sort(symbols)
	return d.depths(ctx, symbols, pairs)
}

func (d *tabdeal) loadPairs(ctx context.Context) (map[string][2]string, error) {
	return d.pairs.get(d.now(), func() (map[string][2]string, error) {
		var body []struct {
			Symbol     string `json:"symbol"`
			Status     string `json:"status"`
			BaseAsset  string `json:"baseAsset"`
			QuoteAsset string `json:"quoteAsset"`
		}
		if err := d.client.GetJSON(ctx, "api/v1/exchangeInfo", nil, &body); err != nil {
			return nil, err
		}
		pairs := map[string][2]string{}
		for _, m := range body {
			if m.Symbol == "" || m.BaseAsset == "" || m.QuoteAsset == "" || !strings.EqualFold(m.Status, "TRADING") {
				continue
			}
			pairs[m.Symbol] = [2]string{symbol(m.BaseAsset, nil), symbol(m.QuoteAsset, map[string]string{"TMN": Toman})}
		}
		if len(pairs) == 0 {
			return nil, errors.New("tabdeal: the exchange information has no trading pair")
		}
		return pairs, nil
	})
}

// depths reads the best bid and ask of each symbol. A pair whose depth
// fails or is empty is left out; the read fails only when every pair does.
func (d *tabdeal) depths(ctx context.Context, symbols []string, pairs map[string][2]string) ([]source.Ticker, error) {
	now := d.now().UTC()
	var (
		mu      sync.Mutex
		out     []source.Ticker
		lastErr error
		wg      sync.WaitGroup
	)
	slots := make(chan struct{}, tabdealParallel)
	for _, s := range symbols {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			bid, ask, err := d.depth(ctx, s)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				lastErr = err
				return
			}
			if bid == nil || ask == nil {
				return
			}
			p := pairs[s]
			t := source.Ticker{VenueSymbol: s, Base: p[0], Quote: p[1], Last: midOf(*bid, *ask), Bid: bid, Ask: ask, ObservedAt: now}
			quoteSides(&t)
			if t.Bid != nil {
				out = append(out, t)
			}
		})
	}
	wg.Wait()
	if len(out) == 0 && lastErr != nil {
		return nil, lastErr
	}
	slices.SortFunc(out, func(a, b source.Ticker) int { return strings.Compare(a.VenueSymbol, b.VenueSymbol) })
	return out, nil
}

func (d *tabdeal) depth(ctx context.Context, venueSymbol string) (bid, ask *shared.Decimal, err error) {
	var body struct {
		Bids [][]any `json:"bids"`
		Asks [][]any `json:"asks"`
	}
	query := url.Values{"symbol": {venueSymbol}, "limit": {"5"}}
	if err := d.client.GetJSON(ctx, "api/v1/depth", query, &body); err != nil {
		return nil, nil, fmt.Errorf("tabdeal: depth of %s: %w", venueSymbol, err)
	}
	return bestLevel(body.Bids), bestLevel(body.Asks), nil
}

// bestLevel returns the price of the first level of a side, [price, size].
func bestLevel(levels [][]any) *shared.Decimal {
	if len(levels) == 0 || len(levels[0]) == 0 {
		return nil
	}
	return positive(levels[0][0])
}
