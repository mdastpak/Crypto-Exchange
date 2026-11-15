package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/price"
	"github.com/mdastpak/crypto-exchange/internal/priceoverride"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// The reference prices of the markets (#59): what the price engine gives
// now, and the prices that administrators set in its place until a time.

const (
	codePricesUnavailable    = "prices_unavailable"
	codePriceUnavailable     = "price_unavailable"
	codePriceOverrideExists  = "price_override_exists"
	codePriceOverrideMissing = "price_override_not_found"
	codePriceOverrideEnded   = "price_override_ended"
)

// priceOverrideRequest is a price that an administrator sets.
type priceOverrideRequest struct {
	// Mid is the reference price; Bid and Ask default to it.
	Mid string `json:"mid" example:"64050"`
	Bid string `json:"bid,omitempty" example:"64000"`
	Ask string `json:"ask,omitempty" example:"64100"`
	// ExpiresInSeconds is how long it lasts: 1 to 86400.
	ExpiresInSeconds int `json:"expires_in_seconds" example:"900"`
	// Reason is why, 1 to 500 characters.
	Reason string `json:"reason" example:"Binance and Kraken are down"`
}

func (h *AdminHandler) pricesConfigured(c *gin.Context) bool {
	if h.PriceOverrides == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codePricesUnavailable, i18n.M("PRICES_UNAVAILABLE"), nil)
		return false
	}
	return true
}

// @Summary Show the reference price of a market
// @Description The price that quotes use now (#59): an administrator's override in force (status override), or the price engine's aggregated price when it is live and fresh (status live), with its sources and time. 404 price_unavailable when there is none: the engine has no quorum for the market, or Redis has no fresh price.
// @Tags admin prices
// @Produce json
// @Security BearerAuth
// @Param base path string true "The symbol of the base currency" example(BTC)
// @Param quote path string true "The symbol of the quote currency" example(USDT)
// @Success 200 {object} models.APIResponse{info=price.Price}
// @Failure 401 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "price_unavailable"
// @Failure 503 {object} models.APIResponse "prices_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/markets/{base}/{quote}/price [get]
func (h *AdminHandler) GetMarketPrice(c *gin.Context) {
	if h.Prices == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codePricesUnavailable, i18n.M("PRICES_UNAVAILABLE"), nil)
		return
	}
	base, quote := pair(c)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	p, err := h.Prices.Get(ctx, base+"/"+quote)
	switch {
	case errors.Is(err, price.ErrNoPrice), errors.Is(err, price.ErrStale):
		helpers.SendError(c, http.StatusNotFound, codePriceUnavailable, i18n.M("PRICE_UNAVAILABLE"), nil)
	case err != nil:
		log.Printf("Failed to read the price of %s/%s: %v", base, quote, err)
		helpers.SendError(c, http.StatusServiceUnavailable, codePricesUnavailable, i18n.M("PRICES_UNAVAILABLE"), nil)
	default:
		helpers.SendResponse(c, http.StatusOK, i18n.M("PRICE_RETRIEVED"), p)
	}
}

// @Summary Set the price of a market
// @Description Sets the reference price of a market in place of the price engine's until expires_in_seconds pass (#59), for when the providers are wrong or down. Bid and ask default to mid; bid <= mid <= ask. The market must exist. A market has one override in force at most: 409 price_override_exists until it ends. Every instance of the server serves it within a second, and stops at its expiry. A sensitive route: it needs a step-up. Audited as price.override_set, with the prices, the expiry, and the reason. The row is sealed: an override changed in SQL is not served.
// @Tags admin prices
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param base path string true "The symbol of the base currency" example(BTC)
// @Param quote path string true "The symbol of the quote currency" example(USDT)
// @Param body body priceOverrideRequest true "The price"
// @Success 201 {object} models.APIResponse{info=priceoverride.Override}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 404 {object} models.APIResponse "market_not_found"
// @Failure 409 {object} models.APIResponse "price_override_exists or row_unverified"
// @Failure 503 {object} models.APIResponse "prices_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/markets/{base}/{quote}/price-override [post]
func (h *AdminHandler) SetPriceOverride(c *gin.Context) {
	if !h.pricesConfigured(c) {
		return
	}
	var req priceOverrideRequest
	if !decodeStrict(c, &req) {
		return
	}
	errs := fieldErrors{}
	amount := func(field, raw string) shared.Decimal {
		d, err := shared.NewDecimalFromString(raw)
		if err != nil || !d.IsPositive() {
			errs[field] = "must be a decimal above 0"
		}
		return d
	}
	mid := amount("mid", req.Mid)
	bid, ask := mid, mid
	if req.Bid != "" {
		bid = amount("bid", req.Bid)
	}
	if req.Ask != "" {
		ask = amount("ask", req.Ask)
	}
	if len(errs) == 0 && (bid.GreaterThan(mid) || mid.GreaterThan(ask)) {
		errs["mid"] = "must be bid <= mid <= ask"
	}
	if req.ExpiresInSeconds < 1 || time.Duration(req.ExpiresInSeconds)*time.Second > priceoverride.MaxFor {
		errs["expires_in_seconds"] = "must be from 1 to 86400"
	}
	if len(req.Reason) == 0 || len(req.Reason) > 500 {
		errs["reason"] = "must be 1 to 500 characters"
	}
	if len(errs) > 0 {
		sendValidationErrors(c, errs)
		return
	}
	origin, adminHK, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	base, quote := pair(c)
	if h.Registry != nil {
		_, err := h.Registry.Market(ctx, base, quote)
		switch {
		case errors.Is(err, currencies.ErrMarketNotFound):
			helpers.SendError(c, http.StatusNotFound, codeMarketNotFound, i18n.M("MARKET_NOT_FOUND"), nil)
			return
		case err != nil && !errors.Is(err, rowseal.ErrUnverified):
			log.Printf("Failed to read the market %s/%s: %v", base, quote, err)
			helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("PRICE_OVERRIDE_FAILED"), nil)
			return
		}
	}
	o, err := h.PriceOverrides.Set(ctx, priceoverride.New{
		Market: base + "/" + quote, Bid: bid, Ask: ask, Mid: mid, Reason: req.Reason,
		For: time.Duration(req.ExpiresInSeconds) * time.Second,
	}, adminHK, origin)
	if err != nil {
		sendPriceOverrideError(c, err)
		return
	}
	h.refreshOverrides(ctx)
	helpers.SendResponse(c, http.StatusCreated, i18n.M("PRICE_OVERRIDE_SET"), o)
}

// @Summary List the price overrides
// @Description The prices that administrators set (#59), newest first: those in force with in_force=true, or the last limit (100) of every state.
// @Tags admin prices
// @Produce json
// @Security BearerAuth
// @Param in_force query bool false "Only those in force"
// @Param limit query int false "At most 100" minimum(1) maximum(100)
// @Success 200 {object} models.APIResponse{info=[]priceoverride.Override}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "row_unverified"
// @Failure 503 {object} models.APIResponse "prices_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/price-overrides [get]
func (h *AdminHandler) ListPriceOverrides(c *gin.Context) {
	if !h.pricesConfigured(c) {
		return
	}
	inForce := false
	if raw := c.Query("in_force"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			sendValidationErrors(c, fieldErrors{"in_force": "must be true or false"})
			return
		}
		inForce = v
	}
	limit := priceoverride.MaxList
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > priceoverride.MaxList {
			sendValidationErrors(c, fieldErrors{"limit": "must be from 1 to 100"})
			return
		}
		limit = n
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	list, err := h.PriceOverrides.List(ctx, inForce, limit)
	if err != nil {
		sendPriceOverrideError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("PRICE_OVERRIDES_RETRIEVED"), list)
}

// @Summary End a price override
// @Description Ends an override in force before its expiry (#59): the market takes the price engine's price again, on every instance within a second. A sensitive route: it needs a step-up. Audited as price.override_end.
// @Tags admin prices
// @Produce json
// @Security BearerAuth
// @Param id path string true "The override" format(uuid)
// @Success 200 {object} models.APIResponse{info=priceoverride.Override}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 404 {object} models.APIResponse "price_override_not_found"
// @Failure 409 {object} models.APIResponse "price_override_ended or row_unverified"
// @Failure 503 {object} models.APIResponse "prices_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/price-overrides/{id}/end [post]
func (h *AdminHandler) EndPriceOverride(c *gin.Context) {
	if !h.pricesConfigured(c) {
		return
	}
	origin, adminHK, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	o, err := h.PriceOverrides.End(ctx, c.Param("id"), adminHK, origin)
	if err != nil {
		sendPriceOverrideError(c, err)
		return
	}
	h.refreshOverrides(ctx)
	helpers.SendResponse(c, http.StatusOK, i18n.M("PRICE_OVERRIDE_ENDED"), o)
}

// refreshOverrides applies a change on this instance at once; the others
// read it within a second.
func (h *AdminHandler) refreshOverrides(ctx context.Context) {
	if h.OverrideWatcher == nil {
		return
	}
	if err := h.OverrideWatcher.Refresh(ctx); err != nil {
		log.Printf("Failed to read the price overrides again: %v", err)
	}
}

func sendPriceOverrideError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, priceoverride.ErrInvalid):
		sendValidationErrors(c, fieldErrors{"override": err.Error()})
	case errors.Is(err, priceoverride.ErrExists):
		helpers.SendError(c, http.StatusConflict, codePriceOverrideExists, i18n.M("PRICE_OVERRIDE_EXISTS"), nil)
	case errors.Is(err, priceoverride.ErrNotFound):
		helpers.SendError(c, http.StatusNotFound, codePriceOverrideMissing, i18n.M("PRICE_OVERRIDE_NOT_FOUND"), nil)
	case errors.Is(err, priceoverride.ErrEnded):
		helpers.SendError(c, http.StatusConflict, codePriceOverrideEnded, i18n.M("PRICE_OVERRIDE_ENDED_ALREADY"), nil)
	case errors.Is(err, rowseal.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
	default:
		log.Printf("Failed to change a price override: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("PRICE_OVERRIDE_FAILED"), nil)
	}
}
