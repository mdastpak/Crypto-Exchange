// Package coingecko is the CoinGecko catalog source (#266): the listing of
// /coins/markets, ranked by market cap, and the full record of /coins/{id}.
// The free public API and the demo plan answer at api.coingecko.com with
// the key in x-cg-demo-api-key; the paid plans answer at
// pro-api.coingecko.com with the key in x-cg-pro-api-key. The attribution
// that CoinGecko's terms ask for goes with every asset.
package coingecko

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// Name is the name of the source.
const Name = "coingecko"

// Attribution is shown with the data, as CoinGecko's terms ask.
const Attribution = "Data provided by CoinGecko"

// pageSize is the most assets of one page of /coins/markets.
const pageSize = 250

// maxIDsPerPage bounds the ids of one request, to keep the URL short.
const maxIDsPerPage = 100

// Settings are how CoinGecko authenticates: the public address and the
// demo key header. A paid plan sets base_url to the pro address, and its
// key goes in the pro header.
func Settings(baseURL string) source.Settings {
	if strings.Contains(baseURL, "pro-api.coingecko.com") {
		return source.Settings{DefaultBaseURL: "https://pro-api.coingecko.com/api/v3", KeyHeader: "x-cg-pro-api-key"}
	}
	return source.Settings{DefaultBaseURL: "https://api.coingecko.com/api/v3", KeyHeader: "x-cg-demo-api-key"}
}

// DevelopmentIDs are the assets of the "list" coverage, which development
// and test environments use: few requests, and the cases that matter. A
// coin, the main stablecoins on several networks, a rebrand (MATIC to POL),
// a chain split (LUNA and LUNC), and a symbol that two assets share (UNI).
var DevelopmentIDs = []string{
	"bitcoin", "ethereum", "tether", "usd-coin", "binancecoin", "solana", "ripple", "tron",
	"the-open-network", "dogecoin", "matic-network", "polygon-ecosystem-token",
	"terra-luna", "terra-luna-2", "uniswap", "wrapped-bitcoin",
}

// Source is the CoinGecko catalog source.
type Source struct {
	client *source.Client
}

// New returns the source on client.
func New(client *source.Client) *Source { return &Source{client: client} }

// Name implements source.CatalogSource.
func (s *Source) Name() string { return Name }

// marketRow is a row of /coins/markets.
type marketRow struct {
	ID                string      `json:"id"`
	Symbol            string      `json:"symbol"`
	Name              string      `json:"name"`
	Image             string      `json:"image"`
	MarketCapRank     json.Number `json:"market_cap_rank"`
	CirculatingSupply any         `json:"circulating_supply"`
	TotalSupply       any         `json:"total_supply"`
	MaxSupply         any         `json:"max_supply"`
}

// Assets implements source.CatalogSource: pages of /coins/markets by
// market cap, until the limit or the end.
func (s *Source) Assets(ctx context.Context, opts source.ListOptions) ([]source.Asset, error) {
	if len(opts.IDs) > 0 {
		var out []source.Asset
		for start := 0; start < len(opts.IDs); start += maxIDsPerPage {
			ids := opts.IDs[start:min(start+maxIDsPerPage, len(opts.IDs))]
			page, err := s.page(ctx, 1, ids)
			if err != nil {
				return nil, err
			}
			out = append(out, page...)
		}
		return out, nil
	}
	var out []source.Asset
	for page := 1; ; page++ {
		rows, err := s.page(ctx, page, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if len(rows) < pageSize || (opts.Limit > 0 && len(out) >= opts.Limit) {
			break
		}
	}
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

func (s *Source) page(ctx context.Context, page int, ids []string) ([]source.Asset, error) {
	q := url.Values{
		"vs_currency": {"usd"},
		"order":       {"market_cap_desc"},
		"per_page":    {strconv.Itoa(pageSize)},
		"page":        {strconv.Itoa(page)},
		"sparkline":   {"false"},
	}
	if len(ids) > 0 {
		q.Set("ids", strings.Join(ids, ","))
	}
	var rows []marketRow
	if err := s.client.GetJSON(ctx, "coins/markets", q, &rows); err != nil {
		return nil, err
	}
	out := make([]source.Asset, 0, len(rows))
	for _, r := range rows {
		a, ok := r.asset()
		if ok {
			out = append(out, a)
		}
	}
	return out, nil
}

func (r marketRow) asset() (source.Asset, bool) {
	if r.ID == "" || strings.TrimSpace(r.Symbol) == "" || strings.TrimSpace(r.Name) == "" {
		return source.Asset{}, false
	}
	return source.Asset{
		ExternalID: r.ID,
		Slug:       r.ID,
		Details: marketdata.AssetDetails{
			Symbol:            marketdata.NormalizeSymbol(r.Symbol),
			Name:              strings.TrimSpace(r.Name),
			LogoURL:           source.HTTPSLink(r.Image),
			MarketCapRank:     source.Int(r.MarketCapRank),
			CirculatingSupply: nonNegative(source.Decimal(r.CirculatingSupply)),
			TotalSupply:       nonNegative(source.Decimal(r.TotalSupply)),
			MaxSupply:         nonNegative(source.Decimal(r.MaxSupply)),
			Attribution:       Attribution,
		},
	}, true
}

// coinRecord is the part of /coins/{id} that the catalog reads.
type coinRecord struct {
	ID              string            `json:"id"`
	Symbol          string            `json:"symbol"`
	Name            string            `json:"name"`
	AssetPlatformID *string           `json:"asset_platform_id"`
	Platforms       map[string]string `json:"platforms"`
	Description     map[string]string `json:"description"`
	Image           struct {
		Large string `json:"large"`
	} `json:"image"`
	MarketCapRank json.Number `json:"market_cap_rank"`
	Links         struct {
		Homepage          []string `json:"homepage"`
		Whitepaper        string   `json:"whitepaper"`
		BlockchainSite    []string `json:"blockchain_site"`
		SubredditURL      string   `json:"subreddit_url"`
		TwitterScreenName string   `json:"twitter_screen_name"`
		ReposURL          struct {
			GitHub []string `json:"github"`
		} `json:"repos_url"`
	} `json:"links"`
}

var twitterName = regexp.MustCompile(`^[A-Za-z0-9_]{1,15}$`)

// Asset implements source.CatalogSource with /coins/{id}, without its
// market data, tickers, and community data, which the catalog does not
// read.
func (s *Source) Asset(ctx context.Context, externalID string) (source.Asset, error) {
	if externalID == "" || strings.ContainsAny(externalID, "/?#") {
		return source.Asset{}, fmt.Errorf("coingecko: invalid id %q: %w", externalID, source.ErrNotFound)
	}
	q := url.Values{
		"localization": {"false"}, "tickers": {"false"}, "market_data": {"false"},
		"community_data": {"false"}, "developer_data": {"false"}, "sparkline": {"false"},
	}
	var r coinRecord
	if err := s.client.GetJSON(ctx, "coins/"+url.PathEscape(externalID), q, &r); err != nil {
		return source.Asset{}, err
	}
	if r.ID == "" || strings.TrimSpace(r.Symbol) == "" || strings.TrimSpace(r.Name) == "" {
		return source.Asset{}, errors.New("coingecko: the record has no id, symbol, or name")
	}
	links := map[string]string{}
	set := func(kind, link string) {
		if clean := source.HTTPSLink(link); clean != "" {
			links[kind] = clean
		}
	}
	set("website", source.FirstHTTPSLink(r.Links.Homepage))
	set("whitepaper", r.Links.Whitepaper)
	set("explorer", source.FirstHTTPSLink(r.Links.BlockchainSite))
	set("source_code", source.FirstHTTPSLink(r.Links.ReposURL.GitHub))
	set("reddit", r.Links.SubredditURL)
	if twitterName.MatchString(r.Links.TwitterScreenName) {
		set("twitter", "https://x.com/"+r.Links.TwitterScreenName)
	}

	kind := marketdata.KindCoin
	var contracts []source.Contract
	for platform, address := range r.Platforms {
		address = strings.TrimSpace(address)
		if platform == "" || address == "" {
			continue
		}
		contracts = append(contracts, source.Contract{Network: source.Network(platform), Address: address})
	}
	if r.AssetPlatformID != nil && *r.AssetPlatformID != "" {
		kind = marketdata.KindToken
	}
	return source.Asset{
		ExternalID: r.ID,
		Slug:       r.ID,
		Details: marketdata.AssetDetails{
			Symbol:        marketdata.NormalizeSymbol(r.Symbol),
			Name:          strings.TrimSpace(r.Name),
			Kind:          kind,
			Description:   source.PlainText(r.Description["en"]),
			Links:         links,
			LogoURL:       source.HTTPSLink(r.Image.Large),
			MarketCapRank: source.Int(r.MarketCapRank),
			Attribution:   Attribution,
		},
		Contracts: sortedContracts(contracts),
	}, nil
}
