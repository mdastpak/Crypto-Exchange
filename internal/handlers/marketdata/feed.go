package marketdata

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source/coingecko"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source/coinmarketcap"
	"github.com/mdastpak/crypto-exchange/internal/pagination"

	"github.com/gin-gonic/gin"
)

// attributionLinks are the pages that the licenses of the catalog sources
// ask a credit to link to, by the attribution text of the source.
var attributionLinks = map[string]string{
	coingecko.Attribution:     "https://www.coingecko.com",
	coinmarketcap.Attribution: "https://coinmarketcap.com",
}

// Attribution is the credit of the source of an asset's data, which its
// license asks to show with the data, as a link where it names one.
type Attribution struct {
	Text string `json:"text"`
	URL  string `json:"url,omitempty"`
}

// FeedItem is an asset that changed. A removed asset, one that an
// administrator hid, has only its slug and time: the blog takes its page
// down, and puts it back when the asset comes back in the feed.
type FeedItem struct {
	Slug      string    `json:"slug"`
	UpdatedAt time.Time `json:"updated_at"`
	Removed   bool      `json:"removed"`
	// Asset is the asset with its description, links, and contracts, as
	// GET /market-data/assets/{slug} serves it.
	Asset *AssetView `json:"asset,omitempty"`
	// Attribution must be shown with the data of the asset.
	Attribution *Attribution `json:"attribution,omitempty"`
	// StructuredData is a schema.org description of the asset, for the
	// JSON-LD of a page. The page adds its own url.
	StructuredData map[string]any `json:"structured_data,omitempty" swaggertype:"object"`
}

// FeedPage is a page of the feed.
type FeedPage struct {
	Items []FeedItem `json:"items"`
	// NextCursor resumes the feed after the last item. It is set on every
	// page, the last one too: keep it, and ask with it later for the
	// assets that change after.
	NextCursor string `json:"next_cursor"`
	// HasMore says that more assets changed already: ask again at once.
	HasMore bool `json:"has_more"`
}

// FeedAssets lists the assets that changed, oldest change first.
//
// @Summary Follow the changes of the market data catalog
// @Description The assets of the catalog in the order they last changed, for a service that keeps a copy, such as the blog. Start with updated_since, or from the beginning, then follow next_cursor, which every page has; keep the last one and ask with it later. An asset is listed again each time it changes, with all its fields; one that an administrator hid is listed as removed. An asset appears once its change is a minute old, so that no change is missed. Cached for market_data.api.cache_seconds, with an ETag.
// @Tags market data
// @Produce json
// @Param updated_since query string false "The first change to list, RFC 3339; ignored with a cursor" format(date-time)
// @Param limit query int false "The number of assets, 50 by default" minimum(1) maximum(200)
// @Param cursor query string false "The next_cursor of the previous page"
// @Success 200 {object} models.APIResponse{info=FeedPage}
// @Success 304 "Not modified: the If-None-Match matches"
// @Failure 400 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/market-data/feed/assets [get]
func (h *Handler) FeedAssets(c *gin.Context) {
	page, errs := pagination.Parse(c)
	if errs == nil {
		errs = map[string]string{}
	}
	var after marketdata.FeedPosition
	if raw := c.Query("updated_since"); raw != "" {
		since, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			errs["updated_since"] = "must be an RFC 3339 time"
		} else {
			// Every slug sorts after "", so the feed starts with the
			// changes at since itself.
			after.UpdatedAt = since.UTC()
		}
	}
	if len(errs) == 0 {
		var position marketdata.FeedPosition
		if ok, err := page.After(&position); err != nil {
			errs["cursor"] = "is not a cursor of this list"
		} else if ok {
			after = position
		}
	}
	if len(errs) > 0 {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": errs})
		return
	}
	h.cached(c, func(ctx context.Context) (any, *apiError) {
		assets, err := h.store.FeedAssets(ctx, after, h.opts.FeedDelay, page.Limit+1)
		if err != nil {
			return nil, internal("reading the market assets feed", err)
		}
		out := FeedPage{Items: []FeedItem{}, HasMore: len(assets) > page.Limit}
		if out.HasMore {
			assets = assets[:page.Limit]
		}
		ids := make([]int64, 0, len(assets))
		for _, a := range assets {
			if a.Status != marketdata.StatusHidden {
				ids = append(ids, a.ID)
			}
		}
		contracts, err := h.store.ContractsOfAssets(ctx, ids)
		if err != nil {
			return nil, internal("reading the contracts of market assets", err)
		}
		for _, a := range assets {
			out.Items = append(out.Items, feedItem(a, contracts[a.ID]))
		}
		last := after
		if len(assets) > 0 {
			last = marketdata.FeedPositionOf(assets[len(assets)-1])
		}
		if out.NextCursor, err = pagination.Cursor(last); err != nil {
			return nil, internal("paging the market assets feed", err)
		}
		return out, nil
	})
}

func feedItem(a marketdata.Asset, contracts []marketdata.Contract) FeedItem {
	item := FeedItem{Slug: a.Slug, UpdatedAt: a.UpdatedAt.UTC()}
	if a.Status == marketdata.StatusHidden {
		item.Removed = true
		return item
	}
	v := view(a)
	v.Description, v.Links, v.Contracts = a.Description, a.Links, contracts
	if v.Contracts == nil {
		v.Contracts = []marketdata.Contract{}
	}
	item.Asset = &v
	if a.Attribution != "" {
		item.Attribution = &Attribution{Text: a.Attribution, URL: attributionLinks[a.Attribution]}
	}
	item.StructuredData = StructuredData(a, contracts)
	return item
}

// StructuredData describes an asset in schema.org terms, for JSON-LD.
// schema.org has no type for a crypto asset, so it is a Thing, named by its
// name and symbol, with its official links as sameAs and its contracts as
// identifiers.
func StructuredData(a marketdata.Asset, contracts []marketdata.Contract) map[string]any {
	data := map[string]any{
		"@context":      "https://schema.org",
		"@type":         "Thing",
		"name":          a.Name,
		"alternateName": a.Symbol,
	}
	if a.Description != "" {
		data["description"] = a.Description
	}
	if a.LogoURL != "" {
		data["image"] = a.LogoURL
	}
	links := make([]string, 0, len(a.Links))
	for _, link := range a.Links {
		if !slices.Contains(links, link) {
			links = append(links, link)
		}
	}
	if len(links) > 0 {
		slices.Sort(links)
		data["sameAs"] = links
	}
	if len(contracts) > 0 {
		ids := make([]map[string]any, len(contracts))
		for i, c := range contracts {
			ids[i] = map[string]any{"@type": "PropertyValue", "propertyID": c.Network, "value": c.Address}
		}
		data["identifier"] = ids
	}
	return data
}
