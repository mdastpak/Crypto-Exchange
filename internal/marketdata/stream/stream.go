// Package stream follows the live tickers of the hot pairs of a venue
// through its public WebSocket stream (#281), so that the prices that users
// watch most are seconds old instead of minutes.
//
// A stream is a job of the job runner, "stream:binance", so that one
// worker at a time holds it, through its lease, and another worker takes
// it over when that worker dies. A run holds a connection for up to RunFor,
// then ends, so that the workers can share the streams; it reconnects with
// backoff when the connection drops, and resubscribes when the hot pairs
// change. The REST ticker job of the venue (#267) keeps running for every
// pair, so a hot pair still gets its ticker every few minutes while its
// stream is down. A ticker replaces only an older one, so the two never
// put an older price back.
//
// Writes are throttled: the tickers received are kept per pair and written
// together every Flush, so that a pair is written at most once per Flush,
// whatever the venue sends.
package stream

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/jobs"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// Protocol is the WebSocket protocol of a venue.
type Protocol interface {
	// Venue is the id of the venue, which is also its source's name.
	Venue() string
	// URL is the address of the stream.
	URL() string
	// Key names a pair in the stream's messages.
	Key(p marketdata.StreamPair) string
	// Subscribe returns the messages that subscribe to the pairs.
	Subscribe(pairs []marketdata.StreamPair) ([][]byte, error)
	// Parse returns the tickers of a message, with VenueSymbol set to the
	// key of the pair; a message that holds no ticker returns none.
	Parse(message []byte, now time.Time) ([]source.Ticker, error)
	// Ping is the message that keeps the connection open, sent every
	// PingEvery; nil when the venue needs none.
	Ping() []byte
}

// Conn is a WebSocket connection.
type Conn interface {
	// ReadMessage returns the next message; it fails once the stream has
	// been quiet too long.
	ReadMessage() ([]byte, error)
	WriteMessage(message []byte) error
	Close() error
}

// Dialer opens a connection.
type Dialer func(ctx context.Context, url string) (Conn, error)

// Defaults of Options.
const (
	DefaultRunFor      = time.Hour
	DefaultFlush       = time.Second
	DefaultRefresh     = 5 * time.Minute
	DefaultPingEvery   = 20 * time.Second
	DefaultBackoffMin  = time.Second
	DefaultBackoffMax  = time.Minute
	DefaultMaxPairs    = 200
	DefaultMaxFailures = 5
)

// Options configure the stream of a venue.
type Options struct {
	// Tiers are the tiers of the pairs it follows; hot by default.
	Tiers []string
	// MaxPairs bounds the pairs, the better-ranked bases first.
	MaxPairs int
	// RunFor is how long a run holds the stream before it ends, so that
	// the workers share the streams.
	RunFor time.Duration
	// Flush is how often the tickers received are written: a pair is
	// written at most once per Flush.
	Flush time.Duration
	// Refresh is how often the pairs are read again, to follow the tiers.
	Refresh time.Duration
	// PingEvery is how often the protocol's ping is sent.
	PingEvery time.Duration
	// BackoffMin and BackoffMax bound the wait before a reconnection,
	// which doubles with each failure in a row, with jitter.
	BackoffMin, BackoffMax time.Duration
	// MaxFailures ends the run with an error after this many connections
	// in a row that failed before receiving a ticker, so that the runner
	// backs off, and another worker may try.
	MaxFailures int
	Logger      *log.Logger
	Now         func() time.Time
}

// Job follows the stream of one venue.
type Job struct {
	proto Protocol
	dial  Dialer
	store *marketdata.Store
	opts  Options
}

// New returns the stream job of a venue.
func New(proto Protocol, dial Dialer, store *marketdata.Store, opts Options) *Job {
	if len(opts.Tiers) == 0 {
		opts.Tiers = []string{marketdata.TierHot}
	}
	set := func(d *time.Duration, def time.Duration) {
		if *d <= 0 {
			*d = def
		}
	}
	set(&opts.RunFor, DefaultRunFor)
	set(&opts.Flush, DefaultFlush)
	set(&opts.Refresh, DefaultRefresh)
	set(&opts.PingEvery, DefaultPingEvery)
	set(&opts.BackoffMin, DefaultBackoffMin)
	set(&opts.BackoffMax, DefaultBackoffMax)
	if opts.MaxPairs <= 0 {
		opts.MaxPairs = DefaultMaxPairs
	}
	if opts.MaxFailures <= 0 {
		opts.MaxFailures = DefaultMaxFailures
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Job{proto: proto, dial: dial, store: store, opts: opts}
}

// Name implements jobs.Job.
func (j *Job) Name() string { return "stream:" + j.proto.Venue() }

// Sources implements jobs.Job: the stream goes the venue's route.
func (j *Job) Sources() []string { return []string{j.proto.Venue()} }

// Timeout is the timeout of a run of the job: RunFor, and time to finish.
func (j *Job) Timeout() time.Duration { return j.opts.RunFor + time.Minute }

// errDone ends a session at the end of the run; errResubscribe when the
// pairs changed.
var (
	errDone        = errors.New("stream: the run is over")
	errResubscribe = errors.New("stream: the pairs changed")
)

// Run implements jobs.Job.
func (j *Job) Run(ctx context.Context, run *jobs.Run) error {
	end := time.NewTimer(j.opts.RunFor)
	defer end.Stop()
	failures := 0
	backoff := j.opts.BackoffMin
	for {
		pairs, err := j.store.StreamPairs(ctx, j.proto.Venue(), j.opts.Tiers, j.opts.MaxPairs)
		if err != nil {
			return err
		}
		if len(pairs) == 0 {
			// Nothing to follow: wait for the tiers, or the end.
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case <-end.C:
				return nil
			case <-time.After(j.opts.Refresh):
				continue
			}
		}
		received, err := j.session(ctx, run, pairs, end.C)
		switch {
		case errors.Is(err, errDone):
			return nil
		case errors.Is(err, errResubscribe):
			continue
		case ctx.Err() != nil:
			return context.Cause(ctx)
		case errors.Is(err, jobs.ErrLeaseLost), errors.Is(err, jobs.ErrPaused):
			return err
		}
		if received {
			failures, backoff = 0, j.opts.BackoffMin
		}
		failures++
		if failures >= j.opts.MaxFailures {
			return fmt.Errorf("stream %s: %d connections in a row failed: %w", j.proto.Venue(), failures, err)
		}
		j.opts.Logger.Printf("market data stream %s: %v; reconnecting in %s", j.proto.Venue(), err, backoff)
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-end.C:
			return nil
		case <-time.After(jitter(backoff)):
		}
		backoff = min(2*backoff, j.opts.BackoffMax)
	}
}

// jitter spreads d by ±20 %, so that workers do not reconnect together.
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // G404: timing only.
}

// session connects, subscribes to pairs, and writes the tickers it
// receives until the connection fails, the pairs change, or the run ends.
// It reports whether any ticker came.
func (j *Job) session(ctx context.Context, run *jobs.Run, pairs []marketdata.StreamPair, end <-chan time.Time) (bool, error) {
	conn, err := j.dial(ctx, j.proto.URL())
	if err != nil {
		return false, fmt.Errorf("connecting: %w", err)
	}
	defer conn.Close()
	ids := make(map[string]int64, len(pairs))
	for _, p := range pairs {
		ids[j.proto.Key(p)] = p.PairID
	}
	messages, err := j.proto.Subscribe(pairs)
	if err != nil {
		return false, err
	}
	for _, m := range messages {
		if err := conn.WriteMessage(m); err != nil {
			return false, fmt.Errorf("subscribing: %w", err)
		}
	}

	type read struct {
		message []byte
		err     error
	}
	reads := make(chan read)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			m, err := conn.ReadMessage()
			select {
			case reads <- read{m, err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	flush := time.NewTicker(j.opts.Flush)
	defer flush.Stop()
	refresh := time.NewTicker(j.opts.Refresh)
	defer refresh.Stop()
	var ping <-chan time.Time
	if j.proto.Ping() != nil {
		t := time.NewTicker(j.opts.PingEvery)
		defer t.Stop()
		ping = t.C
	}
	pending := map[int64]marketdata.Ticker{}
	received := false
	write := func() error {
		if len(pending) == 0 {
			return nil
		}
		defer clear(pending)
		if run.DryRun() {
			run.AddWritten(len(pending))
			return nil
		}
		if err := run.Fence(ctx); err != nil {
			return err
		}
		now := j.opts.Now().UTC()
		rows := slices.Collect(maps.Values(pending))
		for i := range rows {
			rows[i].FetchedAt = now
		}
		written, err := j.store.UpsertTickers(ctx, rows)
		if err != nil {
			return fmt.Errorf("writing tickers: %w", err)
		}
		run.AddWritten(written)
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return received, context.Cause(ctx)
		case <-end:
			return received, errors.Join(errDone, write())
		case r := <-reads:
			if r.err != nil {
				return received, errors.Join(fmt.Errorf("reading: %w", r.err), write())
			}
			tickers, err := j.proto.Parse(r.message, j.opts.Now().UTC())
			if err != nil {
				return received, errors.Join(err, write())
			}
			for _, t := range tickers {
				id, ok := ids[t.VenueSymbol]
				if !ok {
					continue
				}
				received = true
				run.AddRead(1)
				if prev, ok := pending[id]; ok && prev.ObservedAt.After(t.ObservedAt) {
					continue
				}
				pending[id] = marketdata.Ticker{PairID: id, LastPrice: t.Last, Bid: t.Bid, Ask: t.Ask,
					VolumeBase: t.VolumeBase, VolumeQuote: t.VolumeQuote, High: t.High, Low: t.Low, Open: t.Open,
					ObservedAt: t.ObservedAt}
			}
		case <-flush.C:
			if err := write(); err != nil {
				return received, err
			}
		case <-ping:
			if err := conn.WriteMessage(j.proto.Ping()); err != nil {
				return received, errors.Join(fmt.Errorf("pinging: %w", err), write())
			}
		case <-refresh.C:
			current, err := j.store.StreamPairs(ctx, j.proto.Venue(), j.opts.Tiers, j.opts.MaxPairs)
			if err != nil {
				return received, errors.Join(err, write())
			}
			if !samePairs(current, pairs) {
				return received, errors.Join(errResubscribe, write())
			}
		}
	}
}

func samePairs(a, b []marketdata.StreamPair) bool {
	if len(a) != len(b) {
		return false
	}
	ids := map[int64]bool{}
	for _, p := range a {
		ids[p.PairID] = true
	}
	for _, p := range b {
		if !ids[p.PairID] {
			return false
		}
	}
	return true
}
