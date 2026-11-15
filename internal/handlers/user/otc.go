package user

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/otc"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/verification"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

const codeOTCUnavailable = "otc_unavailable"

// quoteTimeout bounds a quote request: the pricing reads Redis and the
// database a few times, well under it.
const quoteTimeout = 2 * time.Second

// quoteRequest asks for an OTC quote. Exactly one of quantity, in the base
// currency, and quote_amount, in the quote currency, is set.
type quoteRequest struct {
	Market string `json:"market" example:"BTC/USDT"`
	Side   string `json:"side" enums:"buy,sell" example:"buy"`
	// Quantity is rounded down to the lot size of the market.
	Quantity *string `json:"quantity,omitempty" example:"0.5"`
	// QuoteAmount buys or sells the most lots whose total stays within it.
	QuoteAmount *string `json:"quote_amount,omitempty" example:"1000"`
	// SubAccountID binds the quote to a sub-account of the user; empty is
	// the main account.
	SubAccountID *string `json:"sub_account_id,omitempty" format:"uuid"`
}

// RequestQuote gives a firm OTC quote (#61).
//
// @Summary Request an OTC quote
// @Description A firm price for a buy or a sell of the market, valid until expires_at (otc.quote_ttl_seconds, 5 to 10 seconds), for the signed-in user (and sub-account) only. The price is the reference price of the price engine raised for a buy, and lowered for a sell, by half the spread, the hedge cost, the margin, the risk premium, and a slippage that grows with the square root of the quantity; it is rounded to the tick size against the user. The fee is the taker fee of the market, in the quote currency, apart from the price: a buy pays total + fee, a sell receives total − fee. The quote is refused while the price is stale, the market does not trade, or the user's level, restrictions, currencies, or balance do not allow the trade; the balance is a pre-check, and the execution checks again. Limited per user by otc.quotes_per_second.
// @Tags user-otc
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body quoteRequest true "The request"
// @Success 201 {object} models.APIResponse{info=otc.Quote}
// @Failure 400 {object} models.APIResponse "validation_failed, quantity_below_minimum, quantity_above_maximum, notional_below_minimum, or quote_too_large"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "verification_required, account_frozen, operation_restricted, asset_restricted, security_hold, or currency_operation_disabled"
// @Failure 404 {object} models.APIResponse "otc_market_unavailable, or price_unavailable: the reference price is stale"
// @Failure 409 {object} models.APIResponse "market_inactive or insufficient_balance"
// @Failure 429 {object} models.APIResponse "Too many quote requests"
// @Failure 503 {object} models.APIResponse "otc_unavailable or feature_locked"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/otc/quote [post]
func (h *UserHandler) RequestQuote(c *gin.Context) {
	claims, ok := sessionCaller(c)
	if !ok {
		return
	}
	if h.Quotes == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codeOTCUnavailable, i18n.M("OTC_UNAVAILABLE"), nil)
		return
	}
	var req quoteRequest
	if !decodeStrict(c, &req) {
		return
	}
	in := otc.Request{Account: otc.Account{UserHK: claims.UUID}, Market: req.Market, Side: otc.Side(req.Side)}
	invalid := map[string]string{}
	if !in.Side.Valid() {
		invalid["side"] = "buy or sell"
	}
	parse := func(field string, s *string) *decimal.Decimal {
		if s == nil {
			return nil
		}
		d, err := decimal.NewFromString(*s)
		if err != nil || !d.IsPositive() {
			invalid[field] = "a positive decimal amount"
			return nil
		}
		return &d
	}
	in.Quantity, in.QuoteAmount = parse("quantity", req.Quantity), parse("quote_amount", req.QuoteAmount)
	if (req.Quantity == nil) == (req.QuoteAmount == nil) {
		invalid["quantity"] = "exactly one of quantity and quote_amount"
	}
	if req.SubAccountID != nil {
		if _, err := uuid.Parse(*req.SubAccountID); err != nil {
			invalid["sub_account_id"] = "a UUID"
		}
		in.Account.SubAccountID = req.SubAccountID
	}
	if len(invalid) > 0 {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": invalid})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), quoteTimeout)
	defer cancel()
	q, err := h.Quotes.Quote(ctx, in)
	if err != nil {
		sendQuoteError(c, claims.UUID, err)
		return
	}
	helpers.SendResponse(c, http.StatusCreated, i18n.M("OTC_QUOTE_GIVEN"), q)
}

// quoteRefusals are the status and the message of each refusal of a
// quote.
var quoteRefusals = map[string]struct {
	status  int
	message i18n.Key
}{
	otc.CodeInvalidQuoteRequest:  {http.StatusBadRequest, i18n.Key("VALIDATION_FAILED")},
	otc.CodeMarketNotQuoted:      {http.StatusNotFound, i18n.Key("OTC_MARKET_UNAVAILABLE")},
	otc.CodeMarketInactive:       {http.StatusConflict, i18n.Key("OTC_MARKET_INACTIVE")},
	otc.CodePriceUnavailable:     {http.StatusNotFound, i18n.Key("PRICE_UNAVAILABLE")},
	otc.CodeQuantityBelowMinimum: {http.StatusBadRequest, i18n.Key("OTC_QUANTITY_BELOW_MINIMUM")},
	otc.CodeQuantityAboveMaximum: {http.StatusBadRequest, i18n.Key("OTC_QUANTITY_ABOVE_MAXIMUM")},
	otc.CodeNotionalBelowMinimum: {http.StatusBadRequest, i18n.Key("OTC_NOTIONAL_BELOW_MINIMUM")},
	otc.CodeQuoteTooLarge:        {http.StatusBadRequest, i18n.Key("OTC_QUOTE_TOO_LARGE")},
	otc.CodeInsufficientBalance:  {http.StatusConflict, i18n.Key("INSUFFICIENT_BALANCE")},
}

func sendQuoteError(c *gin.Context, userHK string, err error) {
	var r *otc.Refusal
	switch {
	case errors.As(err, &r):
		answer, ok := quoteRefusals[r.Code]
		if !ok {
			break
		}
		var info gin.H
		if r.Limit != "" {
			info = gin.H{"limit": r.Limit}
		}
		helpers.SendError(c, answer.status, r.Code, i18n.M(answer.message), info)
		return
	case restrictions.Send(c, err), verification.Send(c, err), currencies.SendRefusal(c, err):
		return
	}
	log.Printf("Failed to quote for user %s: %v", userHK, err)
	helpers.SendError(c, http.StatusInternalServerError, "otc_quote_failed", i18n.M("OTC_QUOTE_FAILED"), nil)
}
