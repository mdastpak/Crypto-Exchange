package price

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/metrics"
)

// Sink receives the prices of each interval, such as the publisher of
// #59. It must return quickly: the next interval waits for it.
type Sink interface {
	Prices(ctx context.Context, prices []Price)
}

// DefaultInterval is the default time between two aggregations.
const DefaultInterval = 100 * time.Millisecond

// Service runs the feeds of the providers and the aggregation loop.
type Service struct {
	agg      *Aggregator
	feeds    []*Feed
	interval time.Duration
	sinks    []Sink
	now      func() time.Time

	mu     sync.RWMutex
	latest []Price
}

// NewService returns the service of the feeds, aggregated by agg every
// interval and passed to the sinks.
func NewService(agg *Aggregator, feeds []*Feed, interval time.Duration, sinks ...Sink) *Service {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Service{agg: agg, feeds: feeds, interval: interval, sinks: sinks, now: time.Now}
}

// Run follows the providers and aggregates until ctx ends.
func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, f := range s.feeds {
		wg.Go(func() { f.Run(ctx, s.ingest) })
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-ticker.C:
			s.aggregate(ctx)
		}
	}
}

func (s *Service) ingest(t Tick) {
	if reason := s.agg.Ingest(t); reason != "" {
		metrics.PriceTicksRejected.WithLabelValues(t.Provider, reason).Inc()
		return
	}
	metrics.PriceTicks.WithLabelValues(t.Provider, t.Kind.String()).Inc()
}

func (s *Service) aggregate(ctx context.Context) {
	start := time.Now()
	now := s.now()
	prices := s.agg.Compute(now)
	metrics.PriceAggregation.Observe(time.Since(start).Seconds())
	s.mu.Lock()
	previous := s.latest
	s.latest = prices
	s.mu.Unlock()
	for i, p := range prices {
		live := 0.0
		if p.Live() {
			live = 1
		}
		metrics.PriceMarketLive.WithLabelValues(p.Market).Set(live)
		metrics.PriceMarketSources.WithLabelValues(p.Market).Set(float64(len(p.Sources)))
		if i < len(previous) && previous[i].Market == p.Market && previous[i].Live() != p.Live() {
			log.Printf("price of %s is now %s (sources %v, outliers %v)", p.Market, p.Status, p.Sources, p.Outliers)
		}
	}
	for _, f := range s.feeds {
		h := f.Health()
		metrics.PriceProviderConnected.WithLabelValues(h.Provider).Set(boolGauge(h.Connected))
		metrics.PriceProviderFallback.WithLabelValues(h.Provider).Set(boolGauge(h.Fallback))
		metrics.PriceProviderLag.WithLabelValues(h.Provider).Set(h.Lag.Seconds())
		if !h.LastTick.IsZero() {
			metrics.PriceProviderAge.WithLabelValues(h.Provider).Set(now.Sub(h.LastTick).Seconds())
		}
	}
	for _, sink := range s.sinks {
		sink.Prices(ctx, prices)
	}
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Prices returns the prices of the last interval.
func (s *Service) Prices() []Price {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latest
}

// Health returns the state of each feed.
func (s *Service) Health() []Health {
	out := make([]Health, len(s.feeds))
	for i, f := range s.feeds {
		out[i] = f.Health()
	}
	return out
}
