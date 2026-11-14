package public

import (
	"context"
	"log"
	"net/http"
	"slices"
	"strconv"

	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/dberr"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/gin-gonic/gin"
)

// @Summary List the markets
// @Description The markets of the exchange (#57): pairs of two listed currencies that trade (ACTIVE) or are paused (MAINTENANCE, SUSPENDED), with their order sizes, tick and lot sizes, minimum notional, and fees in basis points. Amounts are decimal strings. A market leaves the list with either of its currencies, as when a currency is delisted. Every instance reads the markets again at most every ten seconds.
// @Tags public
// @Produce json
// @Success 200 {object} models.APIResponse{info=[]currencies.PublicMarket}
// @Failure 503 {object} models.APIResponse "service_unavailable, with Retry-After"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/markets [get]
func (h *PublicHandler) GetMarkets(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), currencyReadTimeout)
	defer cancel()
	unavailable := func(what string, err error) {
		log.Printf("Error listing the public markets: %s: %v", what, err)
		c.Header("Retry-After", strconv.Itoa(dberr.StoreUnavailable.RetryAfterSeconds))
		helpers.SendError(c, http.StatusServiceUnavailable, dberr.CodeServiceUnavailable, i18n.M("MARKETS_UNAVAILABLE"), nil)
	}
	if h.Markets == nil || h.Currencies == nil {
		unavailable("no store", nil)
		return
	}
	markets, err := h.Markets.List(ctx)
	if err != nil {
		unavailable("the markets", err)
		return
	}
	// The currencies that are listed: a currency that a delisting took out
	// of the list takes its markets with it.
	list, err := h.Currencies.List(ctx)
	if err != nil {
		unavailable("the currencies", err)
		return
	}
	listed := make(map[string]bool, len(list))
	for _, cur := range list {
		listed[cur.HK] = currencies.Listed(cur)
	}
	markets = slices.DeleteFunc(slices.Clone(markets), func(m currencies.PublicMarket) bool {
		return !listed[m.BaseHK] || !listed[m.QuoteHK]
	})
	helpers.SendResponse(c, http.StatusOK, i18n.M("MARKETS_RETRIEVED"), markets)
}
