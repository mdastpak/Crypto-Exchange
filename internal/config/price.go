package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// PriceStreamProviders are the providers that cmd/price-aggregator follows
// through a stream, and PriceRESTProviders those it can read through REST,
// the venues of the market data catalog. The tests of internal/price check
// that the lists agree with its protocols and with the venue sources.
var (
	PriceStreamProviders = []string{"binance", "coinbase", "kraken"}
	PriceRESTProviders   = []string{"binance", "okx", "kraken", "coinbase", "nobitex", "wallex"}
)

// priceSymbol is a market of a provider: BASE/QUOTE or BASE-QUOTE.
var priceSymbol = regexp.MustCompile(`^[A-Za-z0-9]{1,15}[/-][A-Za-z0-9]{1,15}$`)

// ValidatePriceAggregator reports every setting that cmd/price-aggregator
// cannot start with, one per line.
func ValidatePriceAggregator() error {
	var p problems
	c := &Config
	c.priceAggregatorProblems(&p)
	c.regionProblems(&p, true)
	return p.err()
}

func (c *Configuration) priceAggregatorProblems(p *problems) {
	a := c.PriceAggregator
	if !validListenAddr(a.HealthAddr) {
		p.add("price_aggregator.health_addr", "is %q; it must be a port with an optional host, such as :8082", a.HealthAddr)
	}
	if a.AggregationInterval < 10 || a.AggregationInterval > 60_000 {
		p.add("price_aggregator.aggregation_interval_ms", "is %d; it must be between 10 and 60000", a.AggregationInterval)
	}
	if len(a.VWAPWindows) == 0 {
		p.add("price_aggregator.vwap_windows_seconds", "is empty")
	}
	for _, w := range a.VWAPWindows {
		if w < 1 || w > 86_400 {
			p.add("price_aggregator.vwap_windows_seconds", "holds %d; each window must be between 1 and 86400 seconds", w)
		}
	}
	if a.OutlierThreshold <= 0 {
		p.add("price_aggregator.outlier_threshold_mad", "is %v; it must be above 0", a.OutlierThreshold)
	}
	if a.OutlierFloorBPS != nil && (*a.OutlierFloorBPS < 0 || *a.OutlierFloorBPS > 10_000) {
		p.add("price_aggregator.outlier_floor_bps", "is %d; it must be between 0 and 10000", *a.OutlierFloorBPS)
	}
	if a.MinProviders < 1 {
		p.add("price_aggregator.min_providers", "is %d; it must be at least 1", a.MinProviders)
	}
	if a.StaleThreshold < 1 {
		p.add("price_aggregator.stale_threshold_ms", "is %d; it must be above 0", a.StaleThreshold)
	}
	enabled := 0
	seen := map[string]bool{}
	for i, pr := range a.Providers {
		key := fmt.Sprintf("price_aggregator.providers[%d]", i)
		if seen[pr.Name] {
			p.add(key+".name", "is %q, which another provider has", pr.Name)
		}
		seen[pr.Name] = true
		if !pr.Enabled {
			continue
		}
		enabled++
		switch pr.Type {
		case PriceProviderWebSocket:
			if !slices.Contains(PriceStreamProviders, pr.Name) {
				p.add(key+".type", "is websocket, but %q has no stream; the streams are %s", pr.Name, strings.Join(PriceStreamProviders, ", "))
			}
		case PriceProviderREST:
			if !slices.Contains(PriceRESTProviders, pr.Name) {
				p.add(key+".type", "is rest, but %q has no REST API here; those are %s", pr.Name, strings.Join(PriceRESTProviders, ", "))
			}
		default:
			p.add(key+".type", "is %q; it must be websocket or rest", pr.Type)
		}
		if len(pr.Symbols) == 0 {
			p.add(key+".symbols", "is empty")
		}
		for _, s := range pr.Symbols {
			if !priceSymbol.MatchString(strings.TrimSpace(s)) {
				p.add(key+".symbols", "holds %q; a symbol must be BASE/QUOTE, such as BTC/USDT", s)
			}
		}
		for _, e := range []struct{ name, value, scheme string }{
			{"ws_endpoint", pr.WSEndpoint, "wss://"},
			{"rest_endpoint", pr.RESTEndpoint, "https://"},
		} {
			if e.value != "" && !strings.HasPrefix(e.value, e.scheme) {
				p.add(key+"."+e.name, "is %q; it must be a %s address", e.value, e.scheme)
			}
		}
		if pr.PollMs < 0 || pr.StaleThresholdMs < 0 {
			p.add(key, "has a negative poll_ms or stale_threshold_ms")
		}
		switch pr.Egress {
		case "", EgressDirect:
		case EgressProxy:
			if c.MarketData.Egress.ProxyURL == "" {
				p.add(key+".egress", "is proxy, but market_data.egress.proxy_url (MARKET_DATA_PROXY_URL) is not set")
			}
		default:
			p.add(key+".egress", "is %q; it must be direct or proxy", pr.Egress)
		}
	}
	if a.Enabled && enabled == 0 {
		p.add("price_aggregator.providers", "has no enabled provider")
	}
}

// priceRoute returns how this node reaches a price provider.
func (c *Configuration) priceRoute(pr PriceProviderConfig) string {
	if pr.Egress != "" {
		return pr.Egress
	}
	if c.MarketData.Egress.Default == EgressProxy {
		return EgressProxy
	}
	return EgressDirect
}

// PriceProxyURL returns the proxy that this node reaches a price provider
// through, or "" for a direct route.
func (c *Configuration) PriceProxyURL(pr PriceProviderConfig) string {
	if c.priceRoute(pr) == EgressProxy {
		return c.MarketData.Egress.ProxyURL
	}
	return ""
}
