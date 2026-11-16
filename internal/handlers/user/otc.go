package user

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/otc"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
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

// executeTimeout bounds an execution: one transaction of the database and
// one take of Redis.
const executeTimeout = 5 * time.Second

// executeRequest takes a quote.
type executeRequest struct {
	QuoteID string `json:"quote_id" format:"uuid"`
	// IdempotencyKey may come in the Idempotency-Key header instead; when
	// both are sent, they must be the same.
	IdempotencyKey string `json:"idempotency_key,omitempty" example:"7d1e6c2a-trade-1"`
	// SubAccountID is the sub-account that asked for the quote; empty is
	// the main account.
	SubAccountID *string `json:"sub_account_id,omitempty" format:"uuid"`
}

// ExecuteQuote executes an OTC quote (#62).
//
// @Summary Execute an OTC quote
// @Description Takes the quote quote_id of the signed-in user (and sub-account) and settles it at its price, in one transaction: the quote is taken once, the controls of the quote are checked again under the locks of the trade (the trading feature of the platform, the current agreements, the user's level, the restrictions of both currencies, their capabilities, and the market, which must trade), the user's funds are locked and settled against the exchange's inventory, the trade is recorded and sealed, the inventory limits of both currencies are checked (otc.inventory_limits), and the events TradeExecuted and BalanceChanged are written to the outbox, which publishes them once the trade commits. A buy gives total + fee of the quote currency and receives the quantity; a sell gives the quantity and receives total − fee. A quote is traded at most once: once taken, it is gone, also when the trade is then refused; ask for a new one. The same idempotency key again answers 200 with the first trade; with another quote it answers 409 idempotency_key_reused.
// @Tags user-otc
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param Idempotency-Key header string false "1 to 100 letters, digits, dots, colons, underscores, or hyphens; or idempotency_key in the body"
// @Param body body executeRequest true "The request"
// @Success 201 {object} models.APIResponse{info=otc.Trade} "Executed"
// @Success 200 {object} models.APIResponse{info=otc.Trade} "The trade of an earlier request with the key"
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "terms_update_required, verification_required, account_frozen, operation_restricted, asset_restricted, or currency_operation_disabled"
// @Failure 404 {object} models.APIResponse "quote_not_found or otc_market_unavailable"
// @Failure 409 {object} models.APIResponse "insufficient_balance, market_inactive, quote_invalid, quote_not_settleable, otc_inventory_limit, idempotency_key_reused, or row_unverified"
// @Failure 410 {object} models.APIResponse "quote_expired"
// @Failure 503 {object} models.APIResponse "otc_unavailable, maintenance, or feature_locked"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/otc/execute [post]
func (h *UserHandler) ExecuteQuote(c *gin.Context) {
	claims, ok := sessionCaller(c)
	if !ok {
		return
	}
	if h.Trades == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codeOTCUnavailable, i18n.M("OTC_UNAVAILABLE"), nil)
		return
	}
	var req executeRequest
	if !decodeStrict(c, &req) {
		return
	}
	key := c.GetHeader("Idempotency-Key")
	invalid := map[string]string{}
	switch {
	case key == "":
		key = req.IdempotencyKey
	case req.IdempotencyKey != "" && req.IdempotencyKey != key:
		invalid["idempotency_key"] = "differs from the Idempotency-Key header"
	}
	if key == "" {
		invalid["idempotency_key"] = "is required, in the body or the Idempotency-Key header"
	}
	if _, err := uuid.Parse(req.QuoteID); err != nil {
		invalid["quote_id"] = "a UUID"
	}
	if req.SubAccountID != nil {
		if _, err := uuid.Parse(*req.SubAccountID); err != nil {
			invalid["sub_account_id"] = "a UUID"
		}
	}
	if len(invalid) > 0 {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": invalid})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), executeTimeout)
	defer cancel()
	out, err := h.Trades.Execute(ctx, otc.ExecuteRequest{
		Account: otc.Account{UserHK: claims.UUID, SubAccountID: req.SubAccountID}, QuoteID: req.QuoteID,
		IdempotencyKey: key, IP: c.ClientIP(),
	}, audit.FromRequest(c))
	if err != nil {
		sendExecuteError(c, claims.UUID, err)
		return
	}
	if out.Replayed {
		helpers.SendResponse(c, http.StatusOK, i18n.M("OTC_TRADE_REPLAYED"), out.Trade)
		return
	}
	helpers.SendResponse(c, http.StatusCreated, i18n.M("OTC_TRADE_EXECUTED"), out.Trade)
}

// executeRefusals are the status and the message of each refusal of an
// execution.
var executeRefusals = map[string]struct {
	status  int
	message i18n.Key
}{
	otc.CodeInvalidQuoteRequest:  {http.StatusBadRequest, i18n.Key("VALIDATION_FAILED")},
	otc.CodeQuoteNotFound:        {http.StatusNotFound, i18n.Key("OTC_QUOTE_NOT_FOUND")},
	otc.CodeQuoteExpired:         {http.StatusGone, i18n.Key("OTC_QUOTE_EXPIRED")},
	otc.CodeQuoteInvalid:         {http.StatusConflict, i18n.Key("OTC_QUOTE_INVALID")},
	otc.CodeQuoteNotSettleable:   {http.StatusConflict, i18n.Key("OTC_QUOTE_NOT_SETTLEABLE")},
	otc.CodeInventoryLimit:       {http.StatusConflict, i18n.Key("OTC_INVENTORY_LIMIT")},
	otc.CodeIdempotencyKeyReused: {http.StatusConflict, i18n.Key("OTC_IDEMPOTENCY_KEY_REUSED")},
	otc.CodeAgreementsPending:    {http.StatusForbidden, i18n.Key("TERMS_UPDATE_REQUIRED")},
	otc.CodeInsufficientBalance:  {http.StatusConflict, i18n.Key("INSUFFICIENT_BALANCE")},
	otc.CodeMarketInactive:       {http.StatusConflict, i18n.Key("OTC_MARKET_INACTIVE")},
	otc.CodeMarketNotQuoted:      {http.StatusNotFound, i18n.Key("OTC_MARKET_UNAVAILABLE")},
}

func sendExecuteError(c *gin.Context, userHK string, err error) {
	var r *otc.Refusal
	switch {
	case errors.As(err, &r):
		answer, ok := executeRefusals[r.Code]
		if !ok {
			break
		}
		var info gin.H
		if r.Limit != "" {
			info = gin.H{"limit": r.Limit}
		}
		if r.Code == otc.CodeQuoteNotSettleable {
			log.Printf("An OTC quote of user %s is not settleable: %v", userHK, err)
		}
		helpers.SendError(c, answer.status, r.Code, i18n.M(answer.message), info)
		return
	case errors.Is(err, rowseal.ErrUnverified):
		log.Printf("SECURITY: an OTC trade of user %s depends on a row that fails its seal: %v", userHK, err)
		helpers.SendError(c, http.StatusConflict, "row_unverified", i18n.M("OTC_TRADE_UNVERIFIED"), nil)
		return
	case platform.Send(c, err), restrictions.Send(c, err), verification.Send(c, err), currencies.SendRefusal(c, err):
		return
	}
	log.Printf("Failed to execute an OTC quote for user %s: %v", userHK, err)
	helpers.SendError(c, http.StatusInternalServerError, "otc_trade_failed", i18n.M("OTC_TRADE_FAILED"), nil)
}
