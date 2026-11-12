package source

import (
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
)

// Settings say how a source authenticates: its default address, and how
// its API key travels, which differs per source and plan.
type Settings struct {
	// DefaultBaseURL is the address of the API when the configuration sets
	// none.
	DefaultBaseURL string
	// KeyHeader is the header of the API key, such as
	// X-CMC_PRO_API_KEY; empty when the source takes none.
	KeyHeader string
}

// ClientFromConfig returns the client of the named source with the settings
// of c for this node: its route (direct, or through market_data.egress.proxy_url),
// its address, its key, its timeout, and the limits of its plan counted in
// db across every worker. It refuses a source that this node does not call.
func ClientFromConfig(c *config.Configuration, db *sql.DB, name string, s Settings) (*Client, error) {
	cfg := c.MarketData
	route := cfg.Route(name)
	if route == config.EgressNone {
		return nil, fmt.Errorf("source %s: this node does not call it (market_data.sources.%s)", name, name)
	}
	src := cfg.Sources[name]
	opts := ClientOptions{
		Source:  name,
		BaseURL: s.DefaultBaseURL,
		Timeout: time.Duration(src.TimeoutSeconds) * time.Second,
		Limiter: NewUsageLimiter(db, name, src.RequestsPerMinute, src.MonthlyRequests),
	}
	if src.BaseURL != "" {
		opts.BaseURL = src.BaseURL
	}
	if route == config.EgressProxy {
		opts.ProxyURL = cfg.Egress.ProxyURL
	}
	if s.KeyHeader != "" && src.APIKey != "" {
		opts.Header = http.Header{s.KeyHeader: []string{src.APIKey}}
	}
	return NewClient(opts)
}
