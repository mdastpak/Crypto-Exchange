package price

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/stream"
)

// Protocol is the WebSocket protocol of a provider: how it names a market,
// subscribes, and reports books and trades.
type Protocol interface {
	// URL is the default address of the stream.
	URL() string
	// Native is the provider's name of a market.
	Native(market string) string
	// Subscribe returns the messages that subscribe to the books and the
	// trades of the markets, by their native names.
	Subscribe(natives []string) ([][]byte, error)
	// Parse returns the ticks of a message, with Market set to the native
	// name; a message that holds none returns none.
	Parse(message []byte, received time.Time) ([]Tick, error)
	// Ping is the message that keeps the connection open, sent every
	// pingEvery; nil when the provider needs none.
	Ping() []byte
}

// Tickers reads the latest tickers of a provider's REST API: the venue
// sources of the market data catalog (#267) are tickers.
type Tickers interface {
	Tickers(ctx context.Context) ([]source.Ticker, error)
}

// Defaults of FeedOptions.
const (
	DefaultBackoffMin    = 500 * time.Millisecond
	DefaultBackoffMax    = 30 * time.Second
	DefaultPollEvery     = 5 * time.Second
	DefaultFallbackAfter = 3 * time.Second
	pingEvery            = 20 * time.Second
)

// FeedOptions configure a feed.
type FeedOptions struct {
	// Name is the provider's name, which every tick carries.
	Name string
	// Markets are the markets it follows.
	Markets []string
	// Protocol and Dial follow its stream; nil for a provider that has
	// none, which is then read through REST only.
	Protocol Protocol
	Dial     stream.Dialer
	URL      string
	// REST reads its tickers while the stream is down; nil for none.
	REST Tickers
	// PollEvery is the time between two REST reads, and FallbackAfter how
	// long the stream may be silent before REST reads start.
	PollEvery, FallbackAfter time.Duration
	// BackoffMin and BackoffMax bound the wait before a reconnection,
	// which doubles with each failure in a row, with jitter.
	BackoffMin, BackoffMax time.Duration
	Logger                 *log.Logger
	Now                    func() time.Time
}

// Feed follows one provider: its stream, and its REST API while the stream
// is down. It reconnects forever, with backoff, until its context ends.
type Feed struct {
	opts    FeedOptions
	natives map[string]string // native name → market
	byBase  map[[2]string]string

	mu     sync.Mutex
	health Health
}

// Health is the state of a feed, for the probes and the metrics.
type Health struct {
	Provider string `json:"provider"`
	// Connected is whether the stream is open, and Fallback whether the
	// REST reads are on.
	Connected bool `json:"connected"`
	Fallback  bool `json:"fallback"`
	// LastTick is when the last tick came, through either; Lag is the
	// delay of the last tick that carried the provider's time.
	LastTick  time.Time     `json:"last_tick"`
	Lag       time.Duration `json:"lag_ns"`
	Reconnect int           `json:"reconnects"`
	LastError string        `json:"last_error,omitempty"`
}

// NewFeed returns the feed of a provider.
func NewFeed(opts FeedOptions) (*Feed, error) {
	if opts.Protocol == nil && opts.REST == nil {
		return nil, fmt.Errorf("price provider %s: neither a stream nor a REST API", opts.Name)
	}
	if opts.Protocol != nil && opts.Dial == nil {
		return nil, fmt.Errorf("price provider %s: a stream needs a dialer", opts.Name)
	}
	set := func(d *time.Duration, def time.Duration) {
		if *d <= 0 {
			*d = def
		}
	}
	set(&opts.PollEvery, DefaultPollEvery)
	set(&opts.FallbackAfter, DefaultFallbackAfter)
	set(&opts.BackoffMin, DefaultBackoffMin)
	set(&opts.BackoffMax, DefaultBackoffMax)
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.URL == "" && opts.Protocol != nil {
		opts.URL = opts.Protocol.URL()
	}
	f := &Feed{opts: opts, natives: map[string]string{}, byBase: map[[2]string]string{}, health: Health{Provider: opts.Name}}
	for _, m := range opts.Markets {
		if opts.Protocol != nil {
			f.natives[opts.Protocol.Native(m)] = m
		}
		base, quote := SplitMarket(m)
		f.byBase[[2]string{base, quote}] = m
	}
	return f, nil
}

// Name is the provider's name.
func (f *Feed) Name() string { return f.opts.Name }

// Health returns the state of the feed.
func (f *Feed) Health() Health {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.health
}

func (f *Feed) update(change func(h *Health)) {
	f.mu.Lock()
	change(&f.health)
	f.mu.Unlock()
}

// Run follows the provider until ctx ends, and passes each tick to sink.
func (f *Feed) Run(ctx context.Context, sink func(Tick)) {
	var wg sync.WaitGroup
	if f.opts.REST != nil {
		wg.Go(func() { f.poll(ctx, sink) })
	}
	if f.opts.Protocol != nil {
		wg.Go(func() { f.follow(ctx, sink) })
	}
	wg.Wait()
}

// emit stamps and passes the ticks.
func (f *Feed) emit(ticks []Tick, sink func(Tick)) {
	if len(ticks) == 0 {
		return
	}
	var lag time.Duration
	var last time.Time
	for _, t := range ticks {
		t.Provider = f.opts.Name
		if !t.At.IsZero() {
			lag = t.Received.Sub(t.At)
		}
		last = t.Received
		sink(t)
	}
	f.update(func(h *Health) {
		h.LastTick = last
		if lag != 0 {
			h.Lag = lag
		}
	})
}

// poll reads the REST tickers every PollEvery while the stream is down, or
// always for a provider without one.
func (f *Feed) poll(ctx context.Context, sink func(Tick)) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		timer.Reset(f.opts.PollEvery)
		if f.opts.Protocol != nil && !f.streamSilent() {
			f.update(func(h *Health) { h.Fallback = false })
			continue
		}
		f.update(func(h *Health) { h.Fallback = true })
		readCtx, cancel := context.WithTimeout(ctx, max(f.opts.PollEvery, time.Second))
		tickers, err := f.opts.REST.Tickers(readCtx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				f.update(func(h *Health) { h.LastError = "rest: " + err.Error() })
				f.opts.Logger.Printf("price provider %s: reading the REST tickers: %v", f.opts.Name, err)
			}
			continue
		}
		f.emit(f.fromTickers(tickers, f.opts.Now()), sink)
	}
}

// streamSilent reports whether the stream sent no tick for FallbackAfter.
func (f *Feed) streamSilent() bool {
	h := f.Health()
	return !h.Connected || f.opts.Now().Sub(h.LastTick) > f.opts.FallbackAfter
}

// fromTickers returns the book ticks of the followed markets; a ticker
// without both sides of its book gives none.
func (f *Feed) fromTickers(tickers []source.Ticker, now time.Time) []Tick {
	var out []Tick
	for _, t := range tickers {
		market, ok := f.byBase[[2]string{alias(strings.ToUpper(t.Base)), alias(strings.ToUpper(t.Quote))}]
		if !ok || t.Bid == nil || t.Ask == nil {
			continue
		}
		out = append(out, Tick{Market: market, Kind: KindBook, Bid: *t.Bid, Ask: *t.Ask, At: t.ObservedAt, Received: now})
	}
	return out
}

// follow holds the stream, reconnecting with backoff.
func (f *Feed) follow(ctx context.Context, sink func(Tick)) {
	backoff := f.opts.BackoffMin
	for ctx.Err() == nil {
		received, err := f.session(ctx, sink)
		f.update(func(h *Health) { h.Connected = false })
		if ctx.Err() != nil {
			return
		}
		if received {
			backoff = f.opts.BackoffMin
		}
		f.update(func(h *Health) {
			h.Reconnect++
			if err != nil {
				h.LastError = "stream: " + err.Error()
			}
		})
		f.opts.Logger.Printf("price provider %s: stream: %v; reconnecting in %s", f.opts.Name, err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter(backoff)):
		}
		backoff = min(2*backoff, f.opts.BackoffMax)
	}
}

// jitter spreads d by ±20 %, so that the replicas do not reconnect
// together.
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // G404: timing only.
}

// errClosed ends a session whose connection returned no message.
var errClosed = errors.New("the connection closed")

// session connects, subscribes, and passes the ticks until the connection
// fails. It reports whether any tick came.
func (f *Feed) session(ctx context.Context, sink func(Tick)) (bool, error) {
	conn, err := f.opts.Dial(ctx, f.opts.URL)
	if err != nil {
		return false, fmt.Errorf("connecting: %w", err)
	}
	// Closing the connection ends the read below when ctx ends.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer conn.Close()

	natives := make([]string, 0, len(f.natives))
	for _, m := range f.opts.Markets {
		natives = append(natives, f.opts.Protocol.Native(m))
	}
	messages, err := f.opts.Protocol.Subscribe(natives)
	if err != nil {
		return false, err
	}
	for _, m := range messages {
		if err := conn.WriteMessage(m); err != nil {
			return false, fmt.Errorf("subscribing: %w", err)
		}
	}
	f.update(func(h *Health) { h.Connected = true })

	if ping := f.opts.Protocol.Ping(); ping != nil {
		pinger := time.NewTicker(pingEvery)
		done := make(chan struct{})
		defer close(done)
		go func() {
			defer pinger.Stop()
			for {
				select {
				case <-done:
					return
				case <-pinger.C:
					if conn.WriteMessage(ping) != nil {
						return
					}
				}
			}
		}()
	}

	received := false
	for {
		message, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return received, ctx.Err()
			}
			return received, err
		}
		if message == nil {
			return received, errClosed
		}
		ticks, err := f.opts.Protocol.Parse(message, f.opts.Now())
		if err != nil {
			return received, err
		}
		kept := ticks[:0]
		for _, t := range ticks {
			if market, ok := f.natives[t.Market]; ok {
				t.Market = market
				kept = append(kept, t)
			}
		}
		if len(kept) > 0 {
			received = true
			f.emit(kept, sink)
		}
	}
}
