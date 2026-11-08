// Package tickers is the job that collects the latest tickers of a venue
// (#267): one job per venue, "tickers:binance", "tickers:nobitex", so that
// the workers share the venues and one venue that fails or is out of reach
// stops none of the others.
//
// A run reads every ticker of the venue, adds the pairs it does not know,
// maps the base and quote of a pair to the catalog's assets, and writes the
// tickers. A pair keeps its mapping once set, so that an administrator's
// mapping stays; a pair whose symbol names no asset, or several, stays
// unmapped for review. Pairs that the venue stopped listing become
// inactive. Whether a ticker is stale or an outlier is decided when it is
// read (marketdata.Flag), since both change with time and with the other
// venues.
package tickers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/jobs"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// batchSize is how many tickers a run writes between two checks of its
// lease, in one statement each for the pairs and the tickers.
const batchSize = 500

// Defaults of Options.
const (
	DefaultMapRankLimit    = 1000
	DefaultDeactivateAfter = 24 * time.Hour
)

// Options configure the job of one venue.
type Options struct {
	// MapRankLimit: when several assets of the catalog have the symbol of
	// a pair, the pair maps to the one ranked within this limit by market
	// cap, if it is the only one; otherwise it stays unmapped.
	MapRankLimit int
	// DeactivateAfter marks inactive the pairs that the venue has not
	// listed for this long. It is long against the clocks of the workers
	// and the database, and against a venue that drops a pair for a
	// moment.
	DeactivateAfter time.Duration
	Now             func() time.Time
}

// Job collects the tickers of one venue.
type Job struct {
	src   source.VenueSource
	store *marketdata.Store
	opts  Options
}

// New returns the job of src.
func New(src source.VenueSource, store *marketdata.Store, opts Options) *Job {
	if opts.MapRankLimit <= 0 {
		opts.MapRankLimit = DefaultMapRankLimit
	}
	if opts.DeactivateAfter <= 0 {
		opts.DeactivateAfter = DefaultDeactivateAfter
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Job{src: src, store: store, opts: opts}
}

// Name implements jobs.Job.
func (j *Job) Name() string { return "tickers:" + j.src.Name() }

// Sources implements jobs.Job.
func (j *Job) Sources() []string { return []string{j.src.Name()} }

// Run implements jobs.Job.
func (j *Job) Run(ctx context.Context, run *jobs.Run) error {
	venue := j.src.Venue()
	if venue.ID != j.src.Name() {
		return fmt.Errorf("the venue %q of source %q", venue.ID, j.src.Name())
	}
	enabled, err := j.enabled(ctx, run, venue)
	if err != nil || !enabled {
		return err
	}

	fetched, err := j.src.Tickers(ctx)
	if err != nil {
		return fmt.Errorf("tickers: %w", err)
	}
	now := j.opts.Now().UTC()
	run.AddRead(len(fetched))
	// A venue that lists a pair twice keeps the latest ticker, since one
	// statement cannot write a row twice.
	latest := map[string]source.Ticker{}
	var order []string
	for _, t := range fetched {
		prev, seen := latest[t.VenueSymbol]
		if !seen {
			order = append(order, t.VenueSymbol)
		}
		if !seen || t.ObservedAt.After(prev.ObservedAt) {
			latest[t.VenueSymbol] = t
		}
	}

	if run.DryRun() {
		run.AddWritten(len(order))
		return nil
	}
	known, err := j.store.VenuePairs(ctx, venue.ID)
	if err != nil {
		return err
	}
	resolve, err := j.resolver(ctx, known, latest)
	if err != nil {
		return err
	}
	for start := 0; start < len(order); start += batchSize {
		if err := run.Fence(ctx); err != nil {
			return err
		}
		batch := order[start:min(start+batchSize, len(order))]
		pairs := make([]marketdata.VenuePair, len(batch))
		for i, sym := range batch {
			t := latest[sym]
			pairs[i] = marketdata.VenuePair{VenueSymbol: sym, BaseSymbol: t.Base, QuoteSymbol: t.Quote}
			if p, ok := known[sym]; !ok || p.BaseAssetID == nil {
				pairs[i].BaseAssetID = resolve(t.Base)
			}
			if p, ok := known[sym]; !ok || p.QuoteAssetID == nil {
				pairs[i].QuoteAssetID = resolve(t.Quote)
			}
		}
		ids, err := j.store.UpsertVenuePairs(ctx, venue.ID, pairs)
		if err != nil {
			return fmt.Errorf("pairs: %w", err)
		}
		rows := make([]marketdata.Ticker, 0, len(batch))
		for _, sym := range batch {
			t := latest[sym]
			rows = append(rows, marketdata.Ticker{PairID: ids[sym], LastPrice: t.Last, Bid: t.Bid, Ask: t.Ask,
				VolumeBase: t.VolumeBase, VolumeQuote: t.VolumeQuote, High: t.High, Low: t.Low, Open: t.Open,
				ObservedAt: t.ObservedAt, FetchedAt: now})
		}
		written, err := j.store.UpsertTickers(ctx, rows)
		if err != nil {
			return fmt.Errorf("tickers: %w", err)
		}
		run.AddWritten(written)
	}
	if err := run.Fence(ctx); err != nil {
		return err
	}
	if _, err := j.store.DeactivateVenuePairs(ctx, venue.ID, now.Add(-j.opts.DeactivateAfter)); err != nil {
		return fmt.Errorf("deactivating pairs: %w", err)
	}
	return nil
}

// enabled refreshes the venue's description and reports whether the jobs
// collect it, which is the administrators' setting; a new venue takes the
// source's default. A dry run writes nothing, and reads the setting only.
func (j *Job) enabled(ctx context.Context, run *jobs.Run, venue marketdata.Venue) (bool, error) {
	if !run.DryRun() {
		if err := j.store.UpsertVenue(ctx, venue); err != nil {
			return false, fmt.Errorf("venue: %w", err)
		}
	}
	enabled, err := j.store.VenueEnabled(ctx, venue.ID)
	if errors.Is(err, marketdata.ErrNotFound) && run.DryRun() {
		return venue.Enabled, nil
	}
	return enabled, err
}

// resolver returns the function that maps a symbol to the catalog's asset,
// for the symbols of the pairs that are not mapped yet.
func (j *Job) resolver(ctx context.Context, known map[string]marketdata.VenuePair, latest map[string]source.Ticker) (func(string) *int64, error) {
	need := map[string]bool{}
	for sym, t := range latest {
		p, ok := known[sym]
		if !ok || p.BaseAssetID == nil {
			need[marketdata.NormalizeSymbol(t.Base)] = true
		}
		if !ok || p.QuoteAssetID == nil {
			need[marketdata.NormalizeSymbol(t.Quote)] = true
		}
	}
	if len(need) == 0 {
		return func(string) *int64 { return nil }, nil
	}
	symbols := make([]string, 0, len(need))
	for s := range need {
		symbols = append(symbols, s)
	}
	candidates, err := j.store.SymbolCandidates(ctx, symbols)
	if err != nil {
		return nil, err
	}
	return func(sym string) *int64 {
		return pick(candidates[marketdata.NormalizeSymbol(sym)], j.opts.MapRankLimit)
	}, nil
}

// pick is the asset that a symbol names: the only candidate, or the only
// candidate ranked within rankLimit. Otherwise the symbol is ambiguous.
func pick(candidates []marketdata.SymbolCandidate, rankLimit int) *int64 {
	if len(candidates) == 1 {
		id := candidates[0].ID
		return &id
	}
	var chosen *int64
	for _, c := range candidates {
		if c.Rank == nil || *c.Rank > rankLimit {
			continue
		}
		if chosen != nil {
			return nil
		}
		id := c.ID
		chosen = &id
	}
	return chosen
}
