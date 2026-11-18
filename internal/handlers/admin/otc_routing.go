package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/otc/routing"
	"github.com/mdastpak/crypto-exchange/internal/region"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// RoutingRules keeps the versions of the OTC routing rules (#386):
// *routing.Store.
type RoutingRules interface {
	Latest(ctx context.Context) (routing.Version, error)
	Save(ctx context.Context, rules []routing.Rule, reason string, origin audit.Event) (routing.Version, error)
}

// RouteExplainer explains the route of a trade: *routing.Router.
type RouteExplainer interface {
	Explain(ctx context.Context, in routing.Input) (routing.Explanation, error)
}

var (
	_ RoutingRules   = (*routing.Store)(nil)
	_ RouteExplainer = (*routing.Router)(nil)
)

// RoutingRulesView is the active version of the rules, and whether the
// release routes by them.
type RoutingRulesView struct {
	// Enabled is otc.routing.enabled: off, the inventory takes every trade
	// and the rules only explain.
	Enabled bool `json:"enabled"`
	// CacheSeconds is how long a node keeps a version before it checks
	// for a new one.
	CacheSeconds int `json:"cache_seconds"`
	// Version is the highest version; none before the first save.
	Version *routing.Version `json:"version,omitempty"`
}

// SaveRoutingRulesRequest is a new version of the rules: all of them.
type SaveRoutingRulesRequest struct {
	Rules []routing.Rule `json:"rules"`
	// Reason says why the rules change.
	Reason string `json:"reason" binding:"required" maxLength:"1000"`
}

// ExplainRouteRequest is a trade to explain.
type ExplainRouteRequest struct {
	Market string `json:"market" binding:"required" example:"BTC/USDT"`
	Side   string `json:"side" binding:"required" enums:"buy,sell"`
	// Quantity is in the base currency, and Price in the quote currency.
	Quantity string `json:"quantity" binding:"required" example:"0.5"`
	Price    string `json:"price" binding:"required" example:"65000"`
	// Level is the user's verification level (#157).
	Level int `json:"level" example:"2"`
	// Network is our network of the user's destination, if any.
	Network string `json:"network,omitempty"`
	// Region is the user's region; empty is the release's.
	Region string `json:"region,omitempty" enums:"ir,international"`
}

const maxRoutingReason = 1000

func (h *AdminHandler) routingUnavailable(c *gin.Context) bool {
	if h.RoutingRules == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, "routing_unavailable", i18n.M("OTC_ROUTING_UNAVAILABLE"), nil)
		return true
	}
	return false
}

// GetRoutingRules shows the active version of the OTC routing rules.
//
// @Summary Show the OTC routing rules
// @Description The highest version of the rules that route the OTC trades to the exchange's own inventory or to external venues (#386), with who saved it and why, and whether otc.routing.enabled routes by them. A version changed outside the backend answers 409 row_unverified. See docs/otc-routing.md.
// @Tags admin otc
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=RoutingRulesView}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/otc/routing/rules [get]
func (h *AdminHandler) GetRoutingRules(c *gin.Context) {
	if h.routingUnavailable(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	v, err := h.RoutingRules.Latest(ctx)
	if errors.Is(err, rowseal.ErrUnverified) {
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
		return
	}
	if err != nil {
		log.Printf("Failed to read the OTC routing rules: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_ROUTING_RULES"), nil)
		return
	}
	settings := h.settings().OTC.Routing
	view := RoutingRulesView{Enabled: settings.Enabled, CacheSeconds: settings.CacheSeconds}
	if v.Version != 0 {
		view.Version = &v
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("ROUTING_RULES_RETRIEVED"), view)
}

// SaveRoutingRules saves a new version of the OTC routing rules.
//
// @Summary Save the OTC routing rules
// @Description Saves the whole set of rules as a new version (#386), which every node applies within otc.routing.cache_seconds, without a restart. The rules are checked first: a venue without a connector, an Iranian venue in an international release, or an amount that is not one answers 400 with the field of each problem. The reason is required. Audited as admin.otc_routing_rules. The route needs a step-up. See docs/otc-routing.md for the format.
// @Tags admin otc
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body SaveRoutingRulesRequest true "The rules, and why"
// @Success 200 {object} models.APIResponse{info=routing.Version}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/otc/routing/rules [put]
func (h *AdminHandler) SaveRoutingRules(c *gin.Context) {
	if h.routingUnavailable(c) {
		return
	}
	var req SaveRoutingRulesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" || utf8.RuneCountInString(req.Reason) > maxRoutingReason || strings.ContainsRune(req.Reason, 0) {
		sendValidationErrors(c, fieldErrors{"reason": "is required, at most 1000 characters"})
		return
	}
	origin, _, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	v, err := h.RoutingRules.Save(ctx, req.Rules, req.Reason, origin)
	var problems routing.Problems
	switch {
	case errors.As(err, &problems):
		sendValidationErrors(c, fieldErrors(problems))
	case errors.Is(err, rowseal.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
	case err != nil:
		log.Printf("Failed to save the OTC routing rules: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_SAVE_ROUTING_RULES"), nil)
	default:
		helpers.SendResponse(c, http.StatusOK, i18n.M("ROUTING_RULES_SAVED"), v)
	}
}

// ExplainRoute explains how the rules route a trade.
//
// @Summary Explain the route of an OTC trade
// @Description Given a market, a side, a quantity at a price, and the user's verification level, network, and region, shows each rule in order, why it matched or not, what each source of a matching rule can fill and at what cost, and the route of the first rule that gives one (#386), or why there is none. It reads the latest version of the rules and the state of the venues now, and places nothing.
// @Tags admin otc
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body ExplainRouteRequest true "The trade"
// @Success 200 {object} models.APIResponse{info=routing.Explanation}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/otc/routing/explain [post]
func (h *AdminHandler) ExplainRoute(c *gin.Context) {
	if h.RouteExplainer == nil || h.Registry == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, "routing_unavailable", i18n.M("OTC_ROUTING_UNAVAILABLE"), nil)
		return
	}
	var req ExplainRouteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	invalid := fieldErrors{}
	base, quote, ok := strings.Cut(strings.ToUpper(strings.TrimSpace(req.Market)), "/")
	if !ok || base == "" || quote == "" {
		invalid["market"] = "must be a market such as BTC/USDT"
	}
	if req.Side != "buy" && req.Side != "sell" {
		invalid["side"] = "must be buy or sell"
	}
	quantity, err := decimal.NewFromString(req.Quantity)
	if err != nil || !quantity.IsPositive() {
		invalid["quantity"] = "must be a positive amount"
	}
	price, err := decimal.NewFromString(req.Price)
	if err != nil || !price.IsPositive() {
		invalid["price"] = "must be a positive amount"
	}
	if req.Level < 0 || req.Level > 4 {
		invalid["level"] = "must be from 0 to 4"
	}
	rg := h.settings().Region()
	if req.Region != "" {
		if rg, err = region.Parse(req.Region); err != nil {
			invalid["region"] = "must be ir or international"
		}
	}
	if len(invalid) > 0 {
		sendValidationErrors(c, invalid)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	m, err := h.Registry.Market(ctx, base, quote)
	if errors.Is(err, currencies.ErrMarketNotFound) {
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("MARKET_NOT_FOUND"), nil)
		return
	}
	if err == nil {
		var e routing.Explanation
		e, err = h.RouteExplainer.Explain(ctx, routing.Input{
			Market: base + "/" + quote, Base: base, Quote: quote,
			BaseHK: m.BaseCurrencyHK.String(), QuoteHK: m.QuoteCurrencyHK.String(),
			Side: req.Side, Quantity: quantity, Price: price, Lot: m.LotSize.Decimal(),
			Region: rg, Level: req.Level, Network: strings.TrimSpace(req.Network), Day: time.Now(),
		})
		if err == nil {
			helpers.SendResponse(c, http.StatusOK, i18n.M("ROUTE_EXPLAINED"), e)
			return
		}
	}
	if errors.Is(err, rowseal.ErrUnverified) {
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
		return
	}
	log.Printf("Failed to explain an OTC route: %v", err)
	helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_EXPLAIN_ROUTE"), nil)
}
