// Package candles holds the jobs that keep the candles of the venues
// (#268): one job per venue and resolution, "candles:binance:1h", and the
// job "candles:partitions", which creates the partitions ahead and drops
// those past their retention.
//
// A run asks the venue for ranges of at most MaxCandles candles per pair,
// within a budget of requests: first forward, from what it last asked up
// to the last closed candle, so that the charts are current, then backward,
// to backfill the history down to the configured depth. What was asked is
// kept per pair (market_candle_cursors) with the candles, so that a restart
// resumes where the last run stopped, a repeated request is harmless, and
// a range the venue had no candles for is not asked again.
package candles

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/jobs"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// Options configure the job of one venue and resolution.
type Options struct {
	Resolution string
	// Quotes are the quote symbols of the pairs whose candles are kept,
	// such as USDT and IRT.
	Quotes []string
	// MaxPairs bounds the pairs of the venue, the best-ranked bases first.
	MaxPairs int
	// RequestsPerRun bounds the requests of one run.
	RequestsPerRun int
	// Backfill is how far back the history goes.
	Backfill time.Duration
	// Retention is how long candles are kept; 0 keeps them. The backfill
	// never goes past it.
	Retention time.Duration
	Now       func() time.Time
}

// Job keeps the candles of one venue at one resolution.
type Job struct {
	src   source.CandleSource
	store *marketdata.Store
	opts  Options
	step  time.Duration
}

// New returns the job of src.
func New(src source.CandleSource, store *marketdata.Store, opts Options) (*Job, error) {
	step := marketdata.ResolutionStep(opts.Resolution)
	switch {
	case step == 0:
		return nil, fmt.Errorf("candles %s: unknown resolution %q", src.Name(), opts.Resolution)
	case len(opts.Quotes) == 0 || opts.MaxPairs <= 0 || opts.RequestsPerRun <= 0 || opts.Backfill < step:
		return nil, fmt.Errorf("candles %s: quotes, pairs, requests, and backfill are required", src.Name())
	case src.MaxCandles() <= 0:
		return nil, fmt.Errorf("candles %s: no candles per request", src.Name())
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Job{src: src, store: store, opts: opts, step: step}, nil
}

// Name implements jobs.Job.
func (j *Job) Name() string { return "candles:" + j.src.Name() + ":" + j.opts.Resolution }

// Sources implements jobs.Job.
func (j *Job) Sources() []string { return []string{j.src.Name()} }

// cursor is what has been asked for a pair: [from, until).
type cursor struct {
	pair        marketdata.CandlePair
	from, until time.Time
	known       bool
	// gone: the venue no longer knows the pair.
	gone bool
}

// next is the next range to ask for a pair: forward to end first, then
// backward to start.
func (j *Job) next(c *cursor, start, end time.Time) (from, to time.Time, ok bool) {
	chunk := time.Duration(j.src.MaxCandles()) * j.step
	if c.gone {
		return time.Time{}, time.Time{}, false
	}
	if !c.known {
		// The latest candles first, so that a new pair charts at once.
		from = end.Add(-chunk)
		if from.Before(start) {
			from = start
		}
		return from, end, from.Before(end)
	}
	if c.until.Before(end) {
		from = c.until
		if from.Before(start) {
			from = start
		}
		to = from.Add(chunk)
		if to.After(end) {
			to = end
		}
		return from, to, from.Before(to)
	}
	if start.Before(c.from) {
		from = c.from.Add(-chunk)
		if from.Before(start) {
			from = start
		}
		return from, c.from, true
	}
	return time.Time{}, time.Time{}, false
}

// Run implements jobs.Job.
func (j *Job) Run(ctx context.Context, run *jobs.Run) error {
	// A candle that opens before end is closed.
	end := j.opts.Now().UTC().Truncate(j.step)
	depth := j.opts.Backfill
	if j.opts.Retention > 0 && j.opts.Retention < depth {
		depth = j.opts.Retention
	}
	start := end.Add(-depth).Truncate(j.step)

	pairs, err := j.store.CandlePairs(ctx, j.src.Name(), j.opts.Resolution, j.opts.Quotes, j.opts.MaxPairs)
	if err != nil {
		return err
	}
	cursors := make([]*cursor, len(pairs))
	for i, p := range pairs {
		c := &cursor{pair: p}
		if p.From != nil && p.Until != nil {
			c.from, c.until, c.known = p.From.UTC(), p.Until.UTC(), true
		}
		cursors[i] = c
	}

	budget := j.opts.RequestsPerRun
	for budget > 0 {
		progress := false
		for _, c := range cursors {
			if budget == 0 {
				break
			}
			from, to, ok := j.next(c, start, end)
			if !ok {
				continue
			}
			progress = true
			budget--
			if err := j.fetch(ctx, run, c, from, to); err != nil {
				return err
			}
		}
		if !progress {
			break
		}
	}
	return nil
}

// fetch asks for the candles of a pair in [from, to) and stores them.
func (j *Job) fetch(ctx context.Context, run *jobs.Run, c *cursor, from, to time.Time) error {
	candles, err := j.src.Candles(ctx, c.pair.VenueSymbol, source.Interval(j.opts.Resolution), from, to)
	if errors.Is(err, source.ErrNotFound) {
		// The venue no longer knows the pair: leave it for this run; the
		// ticker job marks it inactive once the venue stops listing it.
		c.gone = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("candles of %s: %w", c.pair.VenueSymbol, err)
	}
	run.AddRead(len(candles))
	if !c.known {
		c.from, c.until, c.known = from, to, true
	} else {
		if from.Before(c.from) {
			c.from = from
		}
		if to.After(c.until) {
			c.until = to
		}
	}
	if run.DryRun() {
		run.AddWritten(len(candles))
		return nil
	}
	if err := run.Fence(ctx); err != nil {
		return err
	}
	rows := make([]marketdata.Candle, len(candles))
	for i, cd := range candles {
		rows[i] = marketdata.Candle{OpenTime: cd.OpenTime, Open: cd.Open, High: cd.High, Low: cd.Low, Close: cd.Close, Volume: cd.Volume}
	}
	written, err := j.store.WriteCandles(ctx, c.pair.PairID, j.opts.Resolution, from, to, rows)
	if err != nil {
		return fmt.Errorf("candles of %s: %w", c.pair.VenueSymbol, err)
	}
	run.AddWritten(written)
	return nil
}

// Retention is the retention of a resolution; Keep 0 keeps its candles.
// Ensure creates its partitions ahead, for a resolution that is collected.
type Retention struct {
	Resolution string
	Keep       time.Duration
	Ensure     bool
}

// Partitions is the job that creates the partitions of the current and the
// next period of each resolution, and drops those past their retention.
type Partitions struct {
	store       *marketdata.Store
	resolutions []Retention
	now         func() time.Time
}

// NewPartitions returns the job of the partitions of the resolutions.
func NewPartitions(store *marketdata.Store, resolutions []Retention, now func() time.Time) *Partitions {
	if now == nil {
		now = time.Now
	}
	return &Partitions{store: store, resolutions: resolutions, now: now}
}

// Name implements jobs.Job.
func (p *Partitions) Name() string { return "candles:partitions" }

// Sources implements jobs.Job: it calls none, so every worker may run it.
func (p *Partitions) Sources() []string { return nil }

// Run implements jobs.Job.
func (p *Partitions) Run(ctx context.Context, run *jobs.Run) error {
	now := p.now().UTC()
	for _, r := range p.resolutions {
		if run.DryRun() {
			continue
		}
		if err := run.Fence(ctx); err != nil {
			return err
		}
		if r.Ensure {
			if err := p.store.EnsureCandlePartitions(ctx, r.Resolution, now); err != nil {
				return err
			}
		}
		if r.Keep > 0 {
			dropped, err := p.store.DropCandlePartitions(ctx, r.Resolution, now.Add(-r.Keep))
			if err != nil {
				return err
			}
			run.AddWritten(dropped)
		}
	}
	return nil
}
