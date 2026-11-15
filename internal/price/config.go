package price

import (
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source/exchange"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/stream"
)

// streamQuiet is how long a provider's stream may stay silent before its
// connection counts as dead: every stream sends a tick or a heartbeat, or a
// ping that the dialer answers, well within it.
const streamQuiet = 30 * time.Second

// FromConfig returns the aggregator and the feeds of the enabled providers
// of price_aggregator, which ValidatePriceAggregator has checked.
func FromConfig(c *config.Configuration, logger *log.Logger) (*Aggregator, []*Feed, error) {
	a := c.PriceAggregator
	var markets []string
	var feeds []*Feed
	staleness := map[string]time.Duration{}
	protocols := Protocols()
	for _, pr := range a.Providers {
		if !pr.Enabled {
			continue
		}
		var symbols []string
		for _, s := range pr.Symbols {
			m, err := NormalizeMarket(s)
			if err != nil {
				return nil, nil, fmt.Errorf("price provider %s: %w", pr.Name, err)
			}
			if !slices.Contains(symbols, m) {
				symbols = append(symbols, m)
			}
			if !slices.Contains(markets, m) {
				markets = append(markets, m)
			}
		}
		opts := FeedOptions{
			Name:       pr.Name,
			Markets:    symbols,
			PollEvery:  time.Duration(pr.PollMs) * time.Millisecond,
			BackoffMin: time.Duration(pr.ReconnectDelay) * time.Second / 10,
			Logger:     logger,
		}
		proxy := c.PriceProxyURL(pr)
		if pr.Type == config.PriceProviderWebSocket {
			proto, ok := protocols[pr.Name]
			if !ok {
				return nil, nil, fmt.Errorf("price provider %s: no stream", pr.Name)
			}
			dial, err := stream.NewDialer(proxy, streamQuiet)
			if err != nil {
				return nil, nil, fmt.Errorf("price provider %s: %w", pr.Name, err)
			}
			opts.Protocol, opts.Dial, opts.URL = proto, dial, pr.WSEndpoint
		}
		if settings, ok := exchange.Settings(pr.Name); ok {
			base := settings.DefaultBaseURL
			if pr.RESTEndpoint != "" {
				base = pr.RESTEndpoint
			}
			timeout := time.Duration(pr.Timeout) * time.Second
			if timeout <= 0 {
				timeout = 5 * time.Second
			}
			// One attempt: the next poll is the retry, with a fresher
			// price.
			client, err := source.NewClient(source.ClientOptions{Source: pr.Name, BaseURL: base, Timeout: timeout, ProxyURL: proxy, MaxAttempts: 1})
			if err != nil {
				return nil, nil, err
			}
			if opts.REST, err = exchange.New(pr.Name, client); err != nil {
				return nil, nil, err
			}
		}
		feed, err := NewFeed(opts)
		if err != nil {
			return nil, nil, err
		}
		feeds = append(feeds, feed)
		if pr.StaleThresholdMs > 0 {
			staleness[pr.Name] = time.Duration(pr.StaleThresholdMs) * time.Millisecond
		}
	}
	windows := make([]time.Duration, len(a.VWAPWindows))
	for i, w := range a.VWAPWindows {
		windows[i] = time.Duration(w) * time.Second
	}
	floor := config.DefaultPriceOutlierFloorBPS
	if a.OutlierFloorBPS != nil {
		floor = *a.OutlierFloorBPS
	}
	agg, err := NewAggregator(Options{
		Markets:       markets,
		Windows:       windows,
		OutlierMADs:   decimal.NewFromFloat(a.OutlierThreshold),
		OutlierFloor:  decimal.New(int64(floor), -4),
		MinProviders:  a.MinProviders,
		Stale:         time.Duration(a.StaleThreshold) * time.Millisecond,
		ProviderStale: staleness,
	})
	if err != nil {
		return nil, nil, err
	}
	return agg, feeds, nil
}
