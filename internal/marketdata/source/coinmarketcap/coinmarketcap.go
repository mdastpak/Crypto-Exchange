// Package coinmarketcap is the CoinMarketCap catalog source (#266): the
// listing of /v1/cryptocurrency/listings/latest, ranked by market cap, and
// the records of /v2/cryptocurrency/info. Every plan, the free Basic plan
// too, needs a key, sent in X-CMC_PRO_API_KEY. The plans count credits: a
// listing costs one credit per 200 assets, and the records one credit per
// 100 assets, so Asset reads records in batches through Records.
package coinmarketcap

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// Name is the name of the source.
const Name = "coinmarketcap"

// Attribution is shown with the data, as CoinMarketCap's terms ask.
const Attribution = "Data provided by CoinMarketCap"

// Settings are how CoinMarketCap authenticates.
var Settings = source.Settings{DefaultBaseURL: "https://pro-api.coinmarketcap.com", KeyHeader: "X-CMC_PRO_API_KEY"}

// pageSize is the most assets of one listing request.
const pageSize = 5000

// maxIDsPerRequest is the most ids of one request of records.
const maxIDsPerRequest = 100

// DevelopmentIDs are the CoinMarketCap ids of the "list" coverage, the same
// assets as coingecko.DevelopmentIDs: BTC, ETH, USDT, USDC, BNB, SOL, XRP,
// TRX, TON, DOGE, MATIC, POL, LUNC, LUNA, UNI, WBTC.
var DevelopmentIDs = []string{
	"1", "1027", "825", "3408", "1839", "5426", "52", "1958",
	"11419", "74", "3890", "28321", "4172", "20314", "7083", "3717",
}

// Source is the CoinMarketCap catalog source.
type Source struct {
	client *source.Client
}

// New returns the source on client.
func New(client *source.Client) *Source { return &Source{client: client} }

// Name implements source.CatalogSource.
func (s *Source) Name() string { return Name }

type status struct {
	ErrorCode    int    `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}

type listingRow struct {
	ID                json.Number `json:"id"`
	Name              string      `json:"name"`
	Symbol            string      `json:"symbol"`
	Slug              string      `json:"slug"`
	CMCRank           json.Number `json:"cmc_rank"`
	CirculatingSupply any         `json:"circulating_supply"`
	TotalSupply       any         `json:"total_supply"`
	MaxSupply         any         `json:"max_supply"`
	Platform          *struct {
		Slug         string `json:"slug"`
		TokenAddress string `json:"token_address"`
	} `json:"platform"`
}

// Assets implements source.CatalogSource. With ids, it reads their
// records instead, since the listing cannot select ids.
func (s *Source) Assets(ctx context.Context, opts source.ListOptions) ([]source.Asset, error) {
	if len(opts.IDs) > 0 {
		records, err := s.Records(ctx, opts.IDs)
		if err != nil {
			return nil, err
		}
		out := make([]source.Asset, 0, len(opts.IDs))
		for _, id := range opts.IDs {
			if a, ok := records[id]; ok {
				out = append(out, a)
			}
		}
		return out, nil
	}
	var out []source.Asset
	for start := 1; ; start += pageSize {
		limit := pageSize
		if opts.Limit > 0 {
			limit = min(pageSize, opts.Limit-len(out))
		}
		q := url.Values{"start": {strconv.Itoa(start)}, "limit": {strconv.Itoa(limit)}, "sort": {"market_cap"}, "convert": {"USD"}}
		var body struct {
			Status status       `json:"status"`
			Data   []listingRow `json:"data"`
		}
		if err := s.client.GetJSON(ctx, "v1/cryptocurrency/listings/latest", q, &body); err != nil {
			return nil, err
		}
		if body.Status.ErrorCode != 0 {
			return nil, fmt.Errorf("coinmarketcap: %d %s", body.Status.ErrorCode, body.Status.ErrorMessage)
		}
		for _, r := range body.Data {
			if a, ok := r.asset(); ok {
				out = append(out, a)
			}
		}
		if len(body.Data) < limit || (opts.Limit > 0 && len(out) >= opts.Limit) {
			break
		}
	}
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

func (r listingRow) asset() (source.Asset, bool) {
	id := r.ID.String()
	if id == "" || strings.TrimSpace(r.Symbol) == "" || strings.TrimSpace(r.Name) == "" {
		return source.Asset{}, false
	}
	a := source.Asset{
		ExternalID: id,
		Slug:       r.Slug,
		Details: marketdata.AssetDetails{
			Symbol:            marketdata.NormalizeSymbol(r.Symbol),
			Name:              strings.TrimSpace(r.Name),
			Kind:              marketdata.KindCoin,
			MarketCapRank:     source.Int(r.CMCRank),
			CirculatingSupply: nonNegative(source.Decimal(r.CirculatingSupply)),
			TotalSupply:       nonNegative(source.Decimal(r.TotalSupply)),
			MaxSupply:         nonNegative(source.Decimal(r.MaxSupply)),
			Attribution:       Attribution,
		},
	}
	if r.Platform != nil && strings.TrimSpace(r.Platform.TokenAddress) != "" {
		a.Details.Kind = marketdata.KindToken
		a.Contracts = []source.Contract{{Network: source.Network(r.Platform.Slug), Address: strings.TrimSpace(r.Platform.TokenAddress)}}
	}
	if a.Slug == "" {
		a.Slug = a.Details.Name
	}
	return a, true
}

type infoRecord struct {
	ID          json.Number `json:"id"`
	Name        string      `json:"name"`
	Symbol      string      `json:"symbol"`
	Slug        string      `json:"slug"`
	Category    string      `json:"category"`
	Description string      `json:"description"`
	Logo        string      `json:"logo"`
	URLs        struct {
		Website      []string `json:"website"`
		TechnicalDoc []string `json:"technical_doc"`
		Explorer     []string `json:"explorer"`
		SourceCode   []string `json:"source_code"`
		Reddit       []string `json:"reddit"`
		Twitter      []string `json:"twitter"`
	} `json:"urls"`
	ContractAddress []struct {
		ContractAddress string `json:"contract_address"`
		Platform        struct {
			Name string `json:"name"`
			Coin struct {
				Slug string `json:"slug"`
			} `json:"coin"`
		} `json:"platform"`
	} `json:"contract_address"`
}

// Asset implements source.CatalogSource with one record.
func (s *Source) Asset(ctx context.Context, externalID string) (source.Asset, error) {
	records, err := s.Records(ctx, []string{externalID})
	if err != nil {
		return source.Asset{}, err
	}
	a, ok := records[externalID]
	if !ok {
		return source.Asset{}, fmt.Errorf("coinmarketcap: %s: %w", externalID, source.ErrNotFound)
	}
	return a, nil
}

// Records reads the records of ids, 100 per request, by id. An id that
// CoinMarketCap does not know is left out.
func (s *Source) Records(ctx context.Context, ids []string) (map[string]source.Asset, error) {
	out := map[string]source.Asset{}
	for start := 0; start < len(ids); start += maxIDsPerRequest {
		batch := ids[start:min(start+maxIDsPerRequest, len(ids))]
		for _, id := range batch {
			if _, err := strconv.ParseUint(id, 10, 64); err != nil {
				return nil, fmt.Errorf("coinmarketcap: invalid id %q: %w", id, source.ErrNotFound)
			}
		}
		q := url.Values{"id": {strings.Join(batch, ",")}, "skip_invalid": {"true"}}
		var body struct {
			Status status                `json:"status"`
			Data   map[string]infoRecord `json:"data"`
		}
		if err := s.client.GetJSON(ctx, "v2/cryptocurrency/info", q, &body); err != nil {
			return nil, err
		}
		if body.Status.ErrorCode != 0 {
			return nil, fmt.Errorf("coinmarketcap: %d %s", body.Status.ErrorCode, body.Status.ErrorMessage)
		}
		for id, r := range body.Data {
			if a, ok := r.asset(); ok && a.ExternalID == id {
				out[id] = a
			}
		}
	}
	return out, nil
}

func (r infoRecord) asset() (source.Asset, bool) {
	id := r.ID.String()
	if id == "" || strings.TrimSpace(r.Symbol) == "" || strings.TrimSpace(r.Name) == "" {
		return source.Asset{}, false
	}
	links := map[string]string{}
	set := func(kind string, candidates []string) {
		if clean := source.FirstHTTPSLink(candidates); clean != "" {
			links[kind] = clean
		}
	}
	set("website", r.URLs.Website)
	set("whitepaper", r.URLs.TechnicalDoc)
	set("explorer", r.URLs.Explorer)
	set("source_code", r.URLs.SourceCode)
	set("reddit", r.URLs.Reddit)
	set("twitter", r.URLs.Twitter)

	kind := marketdata.KindCoin
	if r.Category == "token" {
		kind = marketdata.KindToken
	}
	var contracts []source.Contract
	for _, c := range r.ContractAddress {
		address := strings.TrimSpace(c.ContractAddress)
		network := c.Platform.Coin.Slug
		if network == "" {
			network = c.Platform.Name
		}
		if address == "" || network == "" {
			continue
		}
		contracts = append(contracts, source.Contract{Network: source.Network(network), Address: address})
	}
	slices.SortFunc(contracts, func(a, b source.Contract) int { return strings.Compare(a.Network+a.Address, b.Network+b.Address) })
	slug := r.Slug
	if slug == "" {
		slug = r.Name
	}
	return source.Asset{
		ExternalID: id,
		Slug:       slug,
		Details: marketdata.AssetDetails{
			Symbol:      marketdata.NormalizeSymbol(r.Symbol),
			Name:        strings.TrimSpace(r.Name),
			Kind:        kind,
			Description: source.PlainText(r.Description),
			Links:       links,
			LogoURL:     source.HTTPSLink(r.Logo),
			Attribution: Attribution,
		},
		Contracts: contracts,
	}, true
}

func nonNegative(d *shared.Decimal) *shared.Decimal {
	if d == nil || d.IsNegative() {
		return nil
	}
	return d
}
