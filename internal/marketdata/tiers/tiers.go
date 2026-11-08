// Package tiers is the job that puts the pairs of the venues in collection
// tiers (#279): hot, active, and other. The tier of a pair decides at which
// candle resolutions it is collected, so that the pairs that users watch
// most get fine candles, and the rest only hourly and daily ones, within
// the venues' request limits and the size of the database.
//
//   - hot: the pairs whose base is one of the configured assets, such as
//     bitcoin, or is listed on this exchange, with a quote whose candles
//     are kept.
//   - active: the next pairs by 24-hour volume in USD, over every venue.
//   - other: the rest.
//
// An active pair stays active until its rank passes the limit by the
// hysteresis, so that a pair near the boundary does not move every day.
// An administrator's tier, on the pair or its base asset, overrides the
// automatic one where the tiers are read; the job still records the
// automatic tier, which applies again once the override is cleared.
package tiers

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/shopspring/decimal"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/jobs"
)

// Options configure the job.
type Options struct {
	// HotAssets are the slugs of the assets whose pairs are hot.
	HotAssets []string
	// HotListed makes hot the pairs whose base is listed on this exchange.
	HotListed bool
	// Quotes are the quote symbols of the pairs whose candles are kept;
	// only their pairs are hot or active.
	Quotes []string
	// ActiveTop is the number of active pairs, over every venue.
	ActiveTop int
	// HysteresisPercent: an active pair stays active up to the rank
	// ActiveTop plus this share of it.
	HysteresisPercent int
}

// Job puts the pairs in tiers.
type Job struct {
	store *marketdata.Store
	opts  Options
}

// New returns the job.
func New(store *marketdata.Store, opts Options) (*Job, error) {
	if opts.ActiveTop < 0 || opts.HysteresisPercent < 0 {
		return nil, fmt.Errorf("tiers: negative active_top or hysteresis")
	}
	return &Job{store: store, opts: opts}, nil
}

// Name implements jobs.Job.
func (j *Job) Name() string { return "tiers" }

// Sources implements jobs.Job: it calls none, so every worker may run it.
func (j *Job) Sources() []string { return nil }

// Run implements jobs.Job.
func (j *Job) Run(ctx context.Context, run *jobs.Run) error {
	pairs, err := j.store.TierPairs(ctx)
	if err != nil {
		return err
	}
	run.AddRead(len(pairs))
	tiers := Assign(pairs, j.opts)
	changes := map[int64]string{}
	for _, p := range pairs {
		if tiers[p.PairID] != p.Tier {
			changes[p.PairID] = tiers[p.PairID]
		}
	}
	if run.DryRun() {
		run.AddWritten(len(changes))
		return nil
	}
	if err := run.Fence(ctx); err != nil {
		return err
	}
	written, err := j.store.SetTiers(ctx, changes)
	if err != nil {
		return fmt.Errorf("tiers: %w", err)
	}
	run.AddWritten(written)
	return nil
}

// usdQuotes are the quotes worth one US dollar, for the volume in USD.
var usdQuotes = []string{"USD", "USDT", "USDC", "FDUSD", "DAI", "TUSD", "BUSD"}

// Assign returns the automatic tier of each pair.
func Assign(pairs []marketdata.TierPair, opts Options) map[int64]string {
	quotes := make([]string, len(opts.Quotes))
	for i, q := range opts.Quotes {
		quotes[i] = marketdata.NormalizeSymbol(q)
	}
	usd := usdPrices(pairs)
	out := make(map[int64]string, len(pairs))
	type ranked struct {
		pair   marketdata.TierPair
		volume float64
	}
	var candidates []ranked
	for _, p := range pairs {
		out[p.PairID] = marketdata.TierOther
		if !slices.Contains(quotes, p.QuoteSymbol) {
			continue
		}
		if slices.Contains(opts.HotAssets, p.BaseSlug) || (opts.HotListed && p.Listed) {
			out[p.PairID] = marketdata.TierHot
			continue
		}
		if v := volumeUSD(p, usd); v > 0 {
			candidates = append(candidates, ranked{pair: p, volume: v})
		}
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		if candidates[a].volume != candidates[b].volume {
			return candidates[a].volume > candidates[b].volume
		}
		return candidates[a].pair.PairID < candidates[b].pair.PairID
	})
	keep := opts.ActiveTop + opts.ActiveTop*opts.HysteresisPercent/100
	for i, c := range candidates {
		rank := i + 1
		if rank <= opts.ActiveTop || (c.pair.Tier == marketdata.TierActive && rank <= keep) {
			out[c.pair.PairID] = marketdata.TierActive
		}
	}
	return out
}

// usdPrices returns the price in USD of the quotes of each venue: the
// stable coins at one dollar, and another quote, such as IRT or BTC, at the
// price of its pair with a dollar quote at the same venue, or else at the
// median over the venues.
func usdPrices(pairs []marketdata.TierPair) map[string]map[string]float64 {
	perVenue := map[string]map[string]float64{}
	across := map[string][]float64{}
	for _, p := range pairs {
		if p.Last == nil {
			continue
		}
		price := p.Last.Decimal().InexactFloat64()
		if price <= 0 {
			continue
		}
		stableBase, stableQuote := slices.Contains(usdQuotes, p.BaseSymbol), slices.Contains(usdQuotes, p.QuoteSymbol)
		switch {
		case stableBase && p.QuoteSymbol == "IRT":
			// USDT/IRT prices the toman.
			add(perVenue, across, p.VenueID, "IRT", 1/price)
		case stableQuote && !stableBase:
			// BTC/USDT prices BTC, as a quote of other pairs.
			add(perVenue, across, p.VenueID, p.BaseSymbol, price)
		}
	}
	medians := map[string]float64{}
	for symbol, prices := range across {
		medians[symbol] = median(prices)
	}
	out := map[string]map[string]float64{"": medians}
	for venue, prices := range perVenue {
		out[venue] = prices
	}
	return out
}

func add(perVenue map[string]map[string]float64, across map[string][]float64, venue, symbol string, price float64) {
	if perVenue[venue] == nil {
		perVenue[venue] = map[string]float64{}
	}
	if _, seen := perVenue[venue][symbol]; !seen {
		perVenue[venue][symbol] = price
		across[symbol] = append(across[symbol], price)
	}
}

func median(values []float64) float64 {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// volumeUSD is the 24-hour volume of a pair in USD, or 0 when its quote
// has no price in USD.
func volumeUSD(p marketdata.TierPair, usd map[string]map[string]float64) float64 {
	price := 1.0
	if !slices.Contains(usdQuotes, p.QuoteSymbol) {
		var ok bool
		if price, ok = usd[p.VenueID][p.QuoteSymbol]; !ok {
			if price, ok = usd[""][p.QuoteSymbol]; !ok {
				return 0
			}
		}
	}
	var volume decimal.Decimal
	switch {
	case p.VolumeQuote != nil:
		volume = p.VolumeQuote.Decimal()
	case p.VolumeBase != nil && p.Last != nil:
		volume = p.VolumeBase.Decimal().Mul(p.Last.Decimal())
	default:
		return 0
	}
	return volume.InexactFloat64() * price
}
