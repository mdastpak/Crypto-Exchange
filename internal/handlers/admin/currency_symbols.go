package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/gin-gonic/gin"
)

// The previous symbols of currencies (#215). A rebrand keeps the symbol
// before as a previous symbol of the currency, reserved for it. When another
// project must take the symbol, as the new LUNA took LUNA from the renamed
// LUNC, an administrator releases it.

// @Summary Release a previous symbol of a currency
// @Description A rebrand keeps the symbol before as a previous symbol of the currency: the lists find the currency by it, and no other currency can take it, which answers 409 symbol_reserved. Releasing it lets another currency take it, and the currency is no longer found by it. The current symbol cannot be released. Audited as currency.symbol_release.
// @Tags admin currencies
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The currency's UUID" format(uuid)
// @Param symbol path string true "The previous symbol"
// @Success 200 {object} models.APIResponse
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "Not a previous symbol of the currency"
// @Failure 409 {object} models.APIResponse "symbol_current"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies/{hk}/symbols/{symbol} [delete]
func (h *AdminHandler) ReleaseSymbol(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	hk, ok := currencyParam(c)
	if !ok {
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(c.Param("symbol")))
	if !symbolPattern.MatchString(symbol) {
		sendValidationErrors(c, fieldErrors{"symbol": "must be 2 to 15 letters or digits"})
		return
	}
	origin, _, ok := restrictionOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}

	// The row stays, marked, for the record (#248).
	w, err := h.CurrencyStore.ReleaseSymbol(ctx, hk, symbol, origin)
	switch {
	case errors.Is(err, currencies.ErrNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("CURRENCY_NOT_FOUND"), nil)
		return
	case errors.Is(err, currencies.ErrNotPreviousSymbol):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("NOT_A_PREVIOUS_SYMBOL"), nil)
		return
	case errors.Is(err, currencies.ErrSymbolCurrent):
		helpers.SendError(c, http.StatusConflict, "symbol_current", i18n.M("SYMBOL_IS_CURRENT"), nil)
		return
	case err != nil:
		sendUnrecordedOr(c, err, "release a symbol of", i18n.M("FAILED_TO_RELEASE_SYMBOL"))
		return
	}
	if w.Public {
		h.afterPublicChange(ctx, hk)
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("SYMBOL_RELEASED"), nil)
}
