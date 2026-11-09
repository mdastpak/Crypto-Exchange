package admin

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/models"

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

	tx, err := h.PostgresDB.DB.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("Error starting the release of a symbol: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_RELEASE_SYMBOL"), nil)
		return
	}
	defer tx.Rollback()
	var status string
	var deleted sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT status, deleted_at FROM currencies WHERE hk = $1 FOR UPDATE`, hk).Scan(&status, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("CURRENCY_NOT_FOUND"), nil)
		return
	}
	var current bool
	if err == nil {
		err = tx.QueryRowContext(ctx, `SELECT current FROM currency_symbols WHERE symbol = $1 AND currency_hk = $2 AND released_at IS NULL`, symbol, hk).Scan(&current)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("NOT_A_PREVIOUS_SYMBOL"), nil)
		return
	case err != nil:
		log.Printf("Error reading the symbols of %s: %v", hk, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_RELEASE_SYMBOL"), nil)
		return
	case current:
		helpers.SendError(c, http.StatusConflict, "symbol_current", i18n.M("SYMBOL_IS_CURRENT"), nil)
		return
	}
	// The row stays, marked, for the record (#248).
	if _, err := tx.ExecContext(ctx, `UPDATE currency_symbols SET released_at = NOW(), released_by = $3
		WHERE symbol = $1 AND currency_hk = $2 AND NOT current AND released_at IS NULL`, symbol, hk, origin.ActorID); err != nil {
		log.Printf("Error releasing the symbol %s of %s: %v", symbol, hk, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_RELEASE_SYMBOL"), nil)
		return
	}
	e := audit.Of(origin, audit.ActionCurrencySymbolRelease, audit.TargetCurrency, hk)
	e.Changes = &audit.Changes{Before: map[string]any{"previous_symbol": symbol}}
	if !h.recordChange(ctx, c, tx, e) {
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("Error releasing the symbol %s of %s: %v", symbol, hk, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_RELEASE_SYMBOL"), nil)
		return
	}
	if status == models.CurrencyStatusApproved && !deleted.Valid {
		h.afterPublicChange(ctx, hk)
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("SYMBOL_RELEASED"), nil)
}
