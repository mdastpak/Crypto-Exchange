// Package registry builds the venues of the configuration (#384): each
// enabled venue of venues.exchanges, with its connector, its mode, its
// route, its credentials, and its rate limit, which is counted in Postgres
// across every node, so that two instances of the router together stay in
// the venue's limit.
package registry

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
	"github.com/mdastpak/crypto-exchange/internal/venues"
	"github.com/mdastpak/crypto-exchange/internal/venues/binance"
	"github.com/mdastpak/crypto-exchange/internal/venues/nobitex"
)

// Venue is a configured venue: its connector, and its settings.
type Venue struct {
	venues.Venue
	Mapping        venues.Mapping
	SubAccount     string
	WithdrawTarget string
}

// UsageSource is the name of the rate limit of a venue in
// market_source_usage, apart from the limit of its market data.
func UsageSource(name string) string { return "venue_" + name }

// FromConfig returns the enabled venues of c, by their name. db counts the
// requests of each venue; nil leaves them uncounted, as in tests.
func FromConfig(c *config.Configuration, db *sql.DB, logger *log.Logger) (map[string]Venue, error) {
	v := c.Venues
	names := v.EnabledVenues()
	out := make(map[string]Venue, len(names))
	if len(names) == 0 {
		return out, nil
	}
	creds, err := credentialSource(v.Credentials)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		e := v.Exchanges[name]
		mapping := venues.Mapping{}
		for asset, a := range e.Assets {
			mapping[asset] = venues.AssetMapping{Currency: a.Currency, Networks: a.Networks}
		}
		if err := mapping.Check(); err != nil {
			return nil, fmt.Errorf("venue %s: %w", name, err)
		}
		connector, err := build(name, e, v.ProxyURL, db, creds)
		if err != nil {
			return nil, err
		}
		if e.Mode == config.VenueModeDryRun {
			connector = venues.NewDryRun(connector, logger)
		}
		out[name] = Venue{Venue: connector, Mapping: mapping, SubAccount: e.SubAccount, WithdrawTarget: e.WithdrawTarget}
	}
	return out, nil
}

func credentialSource(c config.VenueCredentialsConfig) (venues.CredentialSource, error) {
	var src venues.CredentialSource
	switch c.Provider {
	case config.VenueCredentialsFile:
		src = venues.File{Path: c.File}
	case config.VenueCredentialsOpenBao:
		b := c.OpenBao
		bao, err := venues.NewOpenBao(venues.OpenBaoConfig{Address: b.Address, Token: b.Token, TokenFile: b.TokenFile, Mount: b.Mount,
			Prefix: b.Prefix, CACert: b.CACert, Timeout: time.Duration(b.TimeoutSeconds) * time.Second})
		if err != nil {
			return nil, err
		}
		src = bao
	default:
		return nil, fmt.Errorf("venues: unknown credentials provider %q", c.Provider)
	}
	return venues.NewCached(src, time.Duration(c.CacheSeconds)*time.Second), nil
}

// build returns the connector of name, in the mode that it calls: a dry
// run reads the live venue.
func build(name string, e config.VenueConfig, proxyURL string, db *sql.DB, creds venues.CredentialSource) (venues.Venue, error) {
	mode := venues.ModeLive
	if e.Mode == config.VenueModeTestnet {
		mode = venues.ModeTestnet
	}
	opts := source.ClientOptions{Source: UsageSource(name), BaseURL: e.BaseURL, Timeout: time.Duration(e.TimeoutSeconds) * time.Second}
	if e.Egress == config.EgressProxy {
		opts.ProxyURL = proxyURL
	}
	if db != nil {
		opts.Limiter = source.NewUsageLimiter(db, UsageSource(name), e.RequestsPerMinute, 0)
	}
	switch name {
	case config.VenueBinance:
		if opts.BaseURL == "" {
			opts.BaseURL = binance.LiveBaseURL
			if mode == venues.ModeTestnet {
				opts.BaseURL = binance.TestnetBaseURL
			}
		}
		client, err := source.NewClient(opts)
		if err != nil {
			return nil, err
		}
		return binance.New(binance.Options{Name: name, Mode: mode, Client: client, Credentials: creds,
			RecvWindow: time.Duration(e.RecvWindowMs) * time.Millisecond})
	case config.VenueNobitex:
		if mode == venues.ModeTestnet {
			return nil, fmt.Errorf("venue %s: Nobitex has no testnet", name)
		}
		if opts.BaseURL == "" {
			opts.BaseURL = nobitex.BaseURL
		}
		client, err := source.NewClient(opts)
		if err != nil {
			return nil, err
		}
		return nobitex.New(nobitex.Options{Name: name, Client: client, Credentials: creds})
	}
	return nil, fmt.Errorf("venue %s has no connector", name)
}
