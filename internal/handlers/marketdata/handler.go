// Package marketdata serves the public read API of the global market data
// catalog (#269): the assets, the comparison of their prices across
// exchanges, and their charts. Every asset is labeled unverified unless an
// administrator linked it to a currency listed on this exchange; a hidden
// asset is never served.
//
// Answers are cached in Redis, shared by every instance, with an ETag, so
// that a client that has an answer gets 304 Not Modified, and with
// Cache-Control for clients and proxies. Without Redis the answers are
// built on every request.
package marketdata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/pagination"

	"github.com/gin-gonic/gin"
)

// readTimeout bounds the reads of one request.
const readTimeout = 5 * time.Second

// MaxPoints bounds the candles of one chart request.
const MaxPoints = 1000

// defaultPoints is the number of candles of a chart request without from.
const defaultPoints = 500

// maxQueryLength bounds the search text.
const maxQueryLength = 64

// The error codes of the API.
const (
	CodeAssetNotFound = "market_asset_not_found"
	CodePairNotFound  = "market_pair_not_found"
)

// Options configure the handler.
type Options struct {
	// Regions are the regions of the venues shown; none is every region.
	Regions []string
	// CacheTTL is how long an answer is cached, in Redis and by clients.
	CacheTTL time.Duration
	// Rules say when a ticker is stale or an outlier.
	Rules marketdata.QualityRules
	Now   func() time.Time
}

// Handler serves the routes of the API.
type Handler struct {
	store *marketdata.Store
	cache Cache
	opts  Options
}

// New returns the handler. cache may be nil: the answers are then built on
// every request.
func New(store *marketdata.Store, cache Cache, opts Options) *Handler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = 30 * time.Second
	}
	return &Handler{store: store, cache: cache, opts: opts}
}

// Register adds the routes under g, after the middlewares.
func (h *Handler) Register(g *gin.RouterGroup, middlewares ...gin.HandlerFunc) {
	group := g.Group("/market-data", middlewares...)
	group.GET("/assets", h.ListAssets)
	group.GET("/assets/:slug", h.GetAsset)
	group.GET("/assets/:slug/venues", h.GetVenues)
	group.GET("/assets/:slug/candles", h.GetCandles)
}

// Invalidate makes every instance build its answers again, such as after
// an administrator hid an asset (#270).
func (h *Handler) Invalidate(ctx context.Context) error {
	if h.cache == nil {
		return nil
	}
	return h.cache.Invalidate(ctx)
}

// AssetView is an asset of the catalog.
type AssetView struct {
	Slug   string `json:"slug"`
	Symbol string `json:"symbol"`
	Name   string `json:"name"`
	// Kind is coin, token, or unknown.
	Kind string `json:"kind"`
	// Verified is true only for an asset that an administrator linked to a
	// currency listed on this exchange; Label says the same for display.
	Verified          bool              `json:"verified"`
	Label             string            `json:"label"`
	Active            bool              `json:"active"`
	LogoURL           string            `json:"logo_url,omitempty"`
	MarketCapRank     *int              `json:"market_cap_rank,omitempty"`
	CirculatingSupply *shared.Decimal   `json:"circulating_supply,omitempty" swaggertype:"string"`
	TotalSupply       *shared.Decimal   `json:"total_supply,omitempty" swaggertype:"string"`
	MaxSupply         *shared.Decimal   `json:"max_supply,omitempty" swaggertype:"string"`
	Attribution       string            `json:"attribution,omitempty"`
	UpdatedAt         time.Time         `json:"updated_at"`
	Description       string            `json:"description,omitempty"`
	Links             map[string]string `json:"links,omitempty"`
	// Contracts are the contracts of a token, by network.
	Contracts []marketdata.Contract `json:"contracts,omitempty"`
}

func view(a marketdata.Asset) AssetView {
	verified := a.Status == marketdata.StatusLinked
	label := "unverified"
	if verified {
		label = "verified"
	}
	return AssetView{Slug: a.Slug, Symbol: a.Symbol, Name: a.Name, Kind: a.Kind, Verified: verified, Label: label,
		Active: a.Active, LogoURL: a.LogoURL, MarketCapRank: a.MarketCapRank, CirculatingSupply: a.CirculatingSupply,
		TotalSupply: a.TotalSupply, MaxSupply: a.MaxSupply, Attribution: a.Attribution, UpdatedAt: a.UpdatedAt.UTC()}
}

// AssetPage is a page of the list of assets, as pagination.Page writes it;
// it is spelled out for the OpenAPI document.
type AssetPage struct {
	Items []AssetView `json:"items"`
	// NextCursor asks for the next page. It is left out on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// ListAssets lists the assets of the catalog by market cap rank.
//
// @Summary List the assets of the market data catalog
// @Description The crypto assets of the global catalog, by market cap rank, the unranked last. Every asset is unverified unless it is linked to a currency listed on this exchange. Cached for market_data.api.cache_seconds, with an ETag.
// @Tags market data
// @Produce json
// @Param q query string false "The exact symbol, or a part of the name or slug, in any case" maxLength(64)
// @Param limit query int false "The number of assets, 50 by default" minimum(1) maximum(200)
// @Param cursor query string false "The next_cursor of the previous page"
// @Success 200 {object} models.APIResponse{info=AssetPage}
// @Success 304 "Not modified: the If-None-Match matches"
// @Failure 400 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/market-data/assets [get]
func (h *Handler) ListAssets(c *gin.Context) {
	page, errs := pagination.Parse(c)
	query := strings.TrimSpace(c.Query("q"))
	if len([]rune(query)) > maxQueryLength {
		if errs == nil {
			errs = map[string]string{}
		}
		errs["q"] = "must be at most 64 characters"
	}
	if len(errs) > 0 {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, "Validation failed", gin.H{"errors": errs})
		return
	}
	h.cached(c, func(ctx context.Context) (any, *apiError) {
		var after *marketdata.ListPosition
		var position marketdata.ListPosition
		if ok, err := page.After(&position); err != nil {
			return nil, badRequest(map[string]string{"cursor": "is not a cursor of this list"})
		} else if ok {
			after = &position
		}
		assets, err := h.store.PublicAssets(ctx, query, after, page.Limit+1)
		if err != nil {
			return nil, internal("listing the market assets", err)
		}
		found, err := pagination.NewPage(assets, page, func(a marketdata.Asset) interface{} { return marketdata.PositionOf(a) })
		if err != nil {
			return nil, internal("paging the market assets", err)
		}
		result := AssetPage{Items: make([]AssetView, len(found.Items)), NextCursor: found.NextCursor}
		for i, a := range found.Items {
			result.Items[i] = view(a)
		}
		return result, nil
	})
}

// GetAsset returns an asset with its description, links, and contracts.
//
// @Summary Get an asset of the market data catalog
// @Description The asset of the slug, with its description, links, supply, contracts, and the attribution of its source. A hidden asset is not found.
// @Tags market data
// @Produce json
// @Param slug path string true "The slug of the asset, such as bitcoin"
// @Success 200 {object} models.APIResponse{info=AssetView}
// @Success 304 "Not modified: the If-None-Match matches"
// @Failure 404 {object} models.APIResponse "market_asset_not_found"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/market-data/assets/{slug} [get]
func (h *Handler) GetAsset(c *gin.Context) {
	h.cached(c, func(ctx context.Context) (any, *apiError) {
		a, apiErr := h.asset(ctx, c.Param("slug"))
		if apiErr != nil {
			return nil, apiErr
		}
		contracts, err := h.store.ContractsOf(ctx, a.ID)
		if err != nil {
			return nil, internal("reading the contracts of a market asset", err)
		}
		v := view(a)
		v.Description, v.Links, v.Contracts = a.Description, a.Links, contracts
		return v, nil
	})
}

// VenueQuote is the latest ticker of an asset at one venue.
type VenueQuote struct {
	Venue       string          `json:"venue"`
	VenueName   string          `json:"venue_name"`
	Region      string          `json:"region"`
	Pair        string          `json:"pair"`
	Quote       string          `json:"quote"`
	Last        shared.Decimal  `json:"last" swaggertype:"string"`
	Bid         *shared.Decimal `json:"bid,omitempty" swaggertype:"string"`
	Ask         *shared.Decimal `json:"ask,omitempty" swaggertype:"string"`
	VolumeBase  *shared.Decimal `json:"volume_base,omitempty" swaggertype:"string"`
	VolumeQuote *shared.Decimal `json:"volume_quote,omitempty" swaggertype:"string"`
	High        *shared.Decimal `json:"high,omitempty" swaggertype:"string"`
	Low         *shared.Decimal `json:"low,omitempty" swaggertype:"string"`
	Open        *shared.Decimal `json:"open,omitempty" swaggertype:"string"`
	ObservedAt  time.Time       `json:"observed_at"`
	// Stale: the ticker is older than the staleness limit.
	Stale bool `json:"stale"`
	// Outlier: the price is too far from the median of the venues.
	Outlier bool `json:"outlier"`
	// Median is the median of the fresh prices of the same quote, when
	// enough venues have one.
	Median *shared.Decimal `json:"median,omitempty" swaggertype:"string"`
}

// VenueComparison compares the prices of an asset across venues.
type VenueComparison struct {
	Asset             string       `json:"asset"`
	Verified          bool         `json:"verified"`
	Label             string       `json:"label"`
	StaleAfterSeconds int          `json:"stale_after_seconds"`
	OutlierPercent    string       `json:"outlier_percent"`
	Quotes            []VenueQuote `json:"quotes"`
}

// GetVenues compares the latest prices of an asset across the venues.
//
// @Summary Compare the prices of an asset across exchanges
// @Description The latest ticker of the asset on each enabled venue of the regions this release shows, by quote, with the flags of its quality: stale past the staleness limit, and outlier when further from the median of the venues than the configured share. Prices in different quotes are never compared.
// @Tags market data
// @Produce json
// @Param slug path string true "The slug of the asset, such as bitcoin"
// @Success 200 {object} models.APIResponse{info=VenueComparison}
// @Success 304 "Not modified: the If-None-Match matches"
// @Failure 404 {object} models.APIResponse "market_asset_not_found"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/market-data/assets/{slug}/venues [get]
func (h *Handler) GetVenues(c *gin.Context) {
	h.cached(c, func(ctx context.Context) (any, *apiError) {
		a, apiErr := h.asset(ctx, c.Param("slug"))
		if apiErr != nil {
			return nil, apiErr
		}
		quotes, err := h.store.QuotesOf(ctx, a.ID, h.opts.Regions)
		if err != nil {
			return nil, internal("reading the quotes of a market asset", err)
		}
		marketdata.Flag(quotes, h.opts.Now(), h.opts.Rules)
		v := view(a)
		out := VenueComparison{Asset: a.Slug, Verified: v.Verified, Label: v.Label,
			StaleAfterSeconds: int(h.opts.Rules.StaleAfter.Seconds()),
			OutlierPercent:    h.opts.Rules.MaxDeviation.Shift(2).String(), Quotes: []VenueQuote{}}
		for _, q := range quotes {
			t := q.Ticker
			out.Quotes = append(out.Quotes, VenueQuote{Venue: q.VenueID, VenueName: q.VenueName, Region: q.Region,
				Pair: q.VenueSymbol, Quote: q.QuoteSymbol, Last: t.LastPrice, Bid: t.Bid, Ask: t.Ask,
				VolumeBase: t.VolumeBase, VolumeQuote: t.VolumeQuote, High: t.High, Low: t.Low, Open: t.Open,
				ObservedAt: t.ObservedAt.UTC(), Stale: q.Stale, Outlier: q.Outlier, Median: q.Median})
		}
		return out, nil
	})
}

// CandlePoint is one candle of a chart.
type CandlePoint struct {
	// T is the open time of the candle.
	T time.Time      `json:"t"`
	O shared.Decimal `json:"o" swaggertype:"string"`
	H shared.Decimal `json:"h" swaggertype:"string"`
	L shared.Decimal `json:"l" swaggertype:"string"`
	C shared.Decimal `json:"c" swaggertype:"string"`
	// V is the volume in the base asset.
	V shared.Decimal `json:"v" swaggertype:"string"`
}

// Chart is the candles of an asset at a venue.
type Chart struct {
	Asset    string        `json:"asset"`
	Verified bool          `json:"verified"`
	Label    string        `json:"label"`
	Venue    string        `json:"venue"`
	Quote    string        `json:"quote"`
	Interval string        `json:"interval"`
	From     time.Time     `json:"from"`
	To       time.Time     `json:"to"`
	Candles  []CandlePoint `json:"candles"`
}

// GetCandles returns the chart of an asset at a venue.
//
// @Summary Get the chart of an asset at an exchange
// @Description The closed candles of the asset's pair with the quote at the venue, oldest first, that open in [from, to): at most 1000. Without from, the last 500 candles before to; without to, up to now.
// @Tags market data
// @Produce json
// @Param slug path string true "The slug of the asset, such as bitcoin"
// @Param venue query string true "The venue, such as binance or nobitex"
// @Param quote query string true "The quote symbol, such as USDT or IRT"
// @Param interval query string true "The candle interval. The pairs of each collection tier are kept at some of them only: 1m for the hot pairs, 5m and 15m for the hot and active pairs." Enums(1m, 5m, 15m, 1h, 1d)
// @Param from query string false "The first open time, RFC 3339" format(date-time)
// @Param to query string false "The end, exclusive, RFC 3339" format(date-time)
// @Success 200 {object} models.APIResponse{info=Chart}
// @Success 304 "Not modified: the If-None-Match matches"
// @Failure 400 {object} models.APIResponse "validation_failed, such as a range of more than 1000 candles"
// @Failure 404 {object} models.APIResponse "market_asset_not_found or market_pair_not_found"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/market-data/assets/{slug}/candles [get]
func (h *Handler) GetCandles(c *gin.Context) {
	errs := map[string]string{}
	venue, quote, interval := strings.ToLower(strings.TrimSpace(c.Query("venue"))), strings.TrimSpace(c.Query("quote")), c.Query("interval")
	if venue == "" || len(venue) > 40 {
		errs["venue"] = "is required, such as binance"
	}
	if quote == "" || len(quote) > 32 {
		errs["quote"] = "is required, such as USDT"
	}
	step := marketdata.ResolutionStep(interval)
	if step == 0 {
		errs["interval"] = "must be 1m, 5m, 15m, 1h, or 1d"
	}
	parse := func(name string) (time.Time, bool) {
		raw := c.Query(name)
		if raw == "" {
			return time.Time{}, false
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			errs[name] = "must be an RFC 3339 time"
			return time.Time{}, false
		}
		return t.UTC(), true
	}
	from, hasFrom := parse("from")
	to, hasTo := parse("to")
	if len(errs) == 0 {
		if !hasTo {
			to = h.opts.Now().UTC().Truncate(step).Add(step)
		}
		if !hasFrom {
			from = to.Add(-defaultPoints * step)
		}
		switch {
		case !from.Before(to):
			errs["from"] = "must be before to"
		case to.Sub(from)/step > MaxPoints:
			errs["from"] = "the range holds more than 1000 candles"
		}
	}
	if len(errs) > 0 {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, "Validation failed", gin.H{"errors": errs})
		return
	}
	h.cached(c, func(ctx context.Context) (any, *apiError) {
		a, apiErr := h.asset(ctx, c.Param("slug"))
		if apiErr != nil {
			return nil, apiErr
		}
		pair, err := h.store.PairOf(ctx, a.ID, venue, quote, h.opts.Regions)
		if errors.Is(err, marketdata.ErrNotFound) {
			return nil, &apiError{status: http.StatusNotFound, code: CodePairNotFound, message: "The venue has no such pair of this asset."}
		}
		if err != nil {
			return nil, internal("finding a market pair", err)
		}
		candles, err := h.store.Candles(ctx, pair, interval, from, to)
		if err != nil {
			return nil, internal("reading market candles", err)
		}
		v := view(a)
		out := Chart{Asset: a.Slug, Verified: v.Verified, Label: v.Label, Venue: venue, Quote: marketdata.NormalizeSymbol(quote),
			Interval: interval, From: from, To: to, Candles: make([]CandlePoint, len(candles))}
		for i, cd := range candles {
			out.Candles[i] = CandlePoint{T: cd.OpenTime, O: cd.Open, H: cd.High, L: cd.Low, C: cd.Close, V: cd.Volume}
		}
		return out, nil
	})
}

func (h *Handler) asset(ctx context.Context, slug string) (marketdata.Asset, *apiError) {
	if slug == "" || len(slug) > 120 {
		return marketdata.Asset{}, &apiError{status: http.StatusNotFound, code: CodeAssetNotFound, message: "No such asset."}
	}
	a, err := h.store.PublicAsset(ctx, slug)
	if errors.Is(err, marketdata.ErrNotFound) {
		return marketdata.Asset{}, &apiError{status: http.StatusNotFound, code: CodeAssetNotFound, message: "No such asset."}
	}
	if err != nil {
		return marketdata.Asset{}, internal("reading a market asset", err)
	}
	return a, nil
}

// apiError is an error answer.
type apiError struct {
	status  int
	code    string
	message string
	info    any
}

func badRequest(errs map[string]string) *apiError {
	return &apiError{status: http.StatusBadRequest, code: helpers.CodeValidationFailed, message: "Validation failed",
		info: gin.H{"errors": errs}}
}

func internal(what string, err error) *apiError {
	log.Printf("Market data API: %s: %v", what, err)
	return &apiError{status: http.StatusInternalServerError, message: "Internal error"}
}

// cached answers from the shared cache, or builds the answer, caches it,
// and answers. Only successful answers are cached. A failing cache is
// passed over, so that Redis being away costs speed, not answers.
func (h *Handler) cached(c *gin.Context, build func(ctx context.Context) (any, *apiError)) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), readTimeout)
	defer cancel()
	key := ""
	if h.cache != nil {
		if gen, err := h.cache.Generation(ctx); err != nil {
			log.Printf("Market data API: reading the cache generation: %v", err)
		} else {
			key = cacheKey(gen, c.Request.URL.Path, c.Request.URL.Query())
			if body, ok, err := h.cache.Get(ctx, key); err != nil {
				log.Printf("Market data API: reading the cache: %v", err)
				key = ""
			} else if ok {
				h.write(c, body)
				return
			}
		}
	}
	info, apiErr := build(ctx)
	if apiErr != nil {
		helpers.SendError(c, apiErr.status, apiErr.code, apiErr.message, apiErr.info)
		return
	}
	body, err := json.Marshal(models.APIResponse{Status: http.StatusOK, Message: "OK", Info: info})
	if err != nil {
		helpers.SendError(c, http.StatusInternalServerError, "", "Internal error", nil)
		return
	}
	if key != "" {
		if err := h.cache.Set(ctx, key, body, h.opts.CacheTTL); err != nil {
			log.Printf("Market data API: writing the cache: %v", err)
		}
	}
	h.write(c, body)
}

// write answers body with its ETag, or 304 when the client has it.
func (h *Handler) write(c *gin.Context, body []byte) {
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	c.Header("ETag", etag)
	c.Header("Cache-Control", "public, max-age="+strconv.Itoa(int(h.opts.CacheTTL.Seconds())))
	if matches(c.GetHeader("If-None-Match"), etag) {
		c.Status(http.StatusNotModified)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

// matches reports whether an If-None-Match header names etag, weakly.
func matches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate == etag || candidate == "*" {
			return true
		}
	}
	return false
}

// cacheKey is the key of an answer: the generation, the path, and the
// query in a canonical order.
func cacheKey(generation int64, path string, query map[string][]string) string {
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	b.WriteString("marketdata:api:")
	b.WriteString(strconv.FormatInt(generation, 10))
	b.WriteString(":")
	b.WriteString(path)
	for _, k := range keys {
		values := slices.Clone(query[k])
		slices.Sort(values)
		for _, v := range values {
			b.WriteString("&" + k + "=" + v)
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "marketdata:api:" + strconv.FormatInt(generation, 10) + ":" + hex.EncodeToString(sum[:])
}
