package user

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/otc"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/verification"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeQuotes answers with a quote, or err, and keeps the requests.
type fakeQuotes struct {
	err      error
	requests []otc.Request
}

func (f *fakeQuotes) Quote(_ context.Context, r otc.Request) (otc.Quote, error) {
	f.requests = append(f.requests, r)
	if f.err != nil {
		return otc.Quote{}, f.err
	}
	now := time.Now().UTC()
	return otc.Quote{ID: "q1", UserHK: r.Account.UserHK, Market: "BTC/USDT", Side: r.Side,
		Quantity: shared.MustNewDecimalFromString("0.5"), Price: shared.MustNewDecimalFromString("65325"),
		Total: shared.MustNewDecimalFromString("32662.5"), Fee: shared.MustNewDecimalFromString("65.33"), FeeBPS: 20,
		FeeCurrency: "USDT", CreatedAt: now, ExpiresAt: now.Add(8 * time.Second),
		Components: otc.Components{MarginBPS: shared.MustNewDecimalFromString("20").Decimal()}}, nil
}

func serveQuote(t *testing.T, h *UserHandler, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		auth.SetClaims(c, &auth.Claims{UUID: "user-1", SessionID: uuid.NewString(), Role: auth.RoleUser})
	})
	r.POST("/otc/quote", h.RequestQuote)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/otc/quote", strings.NewReader(body)))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return w.Code, resp
}

func TestQuoteRoute(t *testing.T) {
	status, resp := serveQuote(t, &UserHandler{}, `{"market": "BTC/USDT", "side": "buy", "quantity": "0.5"}`)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, codeOTCUnavailable, resp["code"])

	service := &fakeQuotes{}
	h := &UserHandler{Quotes: service}
	status, resp = serveQuote(t, h, `{"market": "BTC/USDT", "side": "buy", "quantity": "0.5"}`)
	require.Equal(t, http.StatusCreated, status, resp)
	info := resp["info"].(map[string]any)
	assert.Equal(t, "q1", info["id"])
	assert.Equal(t, "65325", info["price"])
	assert.Equal(t, "65.33", info["fee"])
	assert.NotContains(t, info, "components", "the parts of the price are not shown")
	assert.NotContains(t, info, "user_hk")
	require.Len(t, service.requests, 1)
	assert.Equal(t, "user-1", service.requests[0].Account.UserHK, "the quote is of the signed-in user")
	assert.Equal(t, "0.5", service.requests[0].Quantity.String())
	assert.Nil(t, service.requests[0].QuoteAmount)

	sub := uuid.NewString()
	status, _ = serveQuote(t, h, `{"market": "BTC/USDT", "side": "sell", "quote_amount": "1000", "sub_account_id": "`+sub+`"}`)
	require.Equal(t, http.StatusCreated, status)
	assert.Equal(t, sub, *service.requests[1].Account.SubAccountID)

	for _, body := range []string{
		`{"market": "BTC/USDT", "side": "buy"}`,
		`{"market": "BTC/USDT", "side": "buy", "quantity": "1", "quote_amount": "1"}`,
		`{"market": "BTC/USDT", "side": "hold", "quantity": "1"}`,
		`{"market": "BTC/USDT", "side": "buy", "quantity": "-1"}`,
		`{"market": "BTC/USDT", "side": "buy", "quantity": "1", "sub_account_id": "main"}`,
	} {
		status, resp := serveQuote(t, h, body)
		assert.Equal(t, http.StatusBadRequest, status, body)
		assert.Equal(t, "validation_failed", resp["code"], body)
	}
	status, _ = serveQuote(t, h, `{"market": "BTC/USDT", "side": "buy", "quantity": "1", "price": "1"}`)
	assert.Equal(t, http.StatusBadRequest, status, "an unknown field is refused")

	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{&otc.Refusal{Code: otc.CodeMarketNotQuoted}, http.StatusNotFound, otc.CodeMarketNotQuoted},
		{&otc.Refusal{Code: otc.CodeMarketInactive}, http.StatusConflict, otc.CodeMarketInactive},
		{&otc.Refusal{Code: otc.CodePriceUnavailable}, http.StatusNotFound, otc.CodePriceUnavailable},
		{&otc.Refusal{Code: otc.CodeQuantityBelowMinimum, Limit: "0.001"}, http.StatusBadRequest, otc.CodeQuantityBelowMinimum},
		{&otc.Refusal{Code: otc.CodeQuantityAboveMaximum}, http.StatusBadRequest, otc.CodeQuantityAboveMaximum},
		{&otc.Refusal{Code: otc.CodeNotionalBelowMinimum}, http.StatusBadRequest, otc.CodeNotionalBelowMinimum},
		{&otc.Refusal{Code: otc.CodeQuoteTooLarge}, http.StatusBadRequest, otc.CodeQuoteTooLarge},
		{otc.ErrInsufficientBalance, http.StatusConflict, otc.CodeInsufficientBalance},
		{&restrictions.Refusal{Code: restrictions.CodeAccountFrozen}, http.StatusForbidden, restrictions.CodeAccountFrozen},
		{&verification.Refusal{Code: verification.CodeVerificationRequired, Action: verification.TradeOTC}, http.StatusForbidden,
			verification.CodeVerificationRequired},
		{errors.New("redis is down"), http.StatusInternalServerError, "otc_quote_failed"},
	} {
		service.err = tc.err
		status, resp := serveQuote(t, h, `{"market": "BTC/USDT", "side": "buy", "quantity": "1"}`)
		assert.Equal(t, tc.status, status, tc.err.Error())
		assert.Equal(t, tc.code, resp["code"], tc.err.Error())
	}
	service.err = &otc.Refusal{Code: otc.CodeQuantityBelowMinimum, Limit: "0.001"}
	_, resp = serveQuote(t, h, `{"market": "BTC/USDT", "side": "buy", "quantity": "1"}`)
	assert.Equal(t, "0.001", resp["info"].(map[string]any)["limit"])
}

// TestQuoteRefusals_HaveMessages checks that every refusal has a message.
func TestQuoteRefusals_HaveMessages(t *testing.T) {
	for code, answer := range quoteRefusals {
		assert.NotZero(t, answer.status, code)
		assert.NotEmpty(t, answer.message, code)
	}
}

// fakeTrades answers with a trade, or err, and keeps the requests.
type fakeTrades struct {
	err      error
	replayed bool
	requests []otc.ExecuteRequest
}

func (f *fakeTrades) Execute(_ context.Context, r otc.ExecuteRequest, _ audit.Event) (otc.Execution, error) {
	f.requests = append(f.requests, r)
	if f.err != nil {
		return otc.Execution{}, f.err
	}
	return otc.Execution{Replayed: f.replayed, Trade: otc.Trade{ID: "t1", QuoteID: r.QuoteID, Market: "BTC/USDT", Side: otc.SideBuy,
		Quantity: shared.MustNewDecimalFromString("0.5"), Give: otc.Leg{Currency: "USDT", Amount: shared.MustNewDecimalFromString("32727.83")},
		Receive: otc.Leg{Currency: "BTC", Amount: shared.MustNewDecimalFromString("0.5")}}}, nil
}

func serveExecute(t *testing.T, h *UserHandler, key, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		auth.SetClaims(c, &auth.Claims{UUID: "user-1", SessionID: uuid.NewString(), Role: auth.RoleUser})
	})
	r.POST("/otc/execute", h.ExecuteQuote)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/otc/execute", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	r.ServeHTTP(w, req)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return w.Code, resp
}

func TestExecuteRoute(t *testing.T) {
	quote := uuid.NewString()
	body := `{"quote_id": "` + quote + `"}`
	status, resp := serveExecute(t, &UserHandler{}, "k1", body)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, codeOTCUnavailable, resp["code"])

	service := &fakeTrades{}
	h := &UserHandler{Trades: service}
	status, resp = serveExecute(t, h, "k1", body)
	require.Equal(t, http.StatusCreated, status, resp)
	info := resp["info"].(map[string]any)
	assert.Equal(t, "t1", info["id"])
	assert.Equal(t, "32727.83", info["give"].(map[string]any)["amount"])
	require.Len(t, service.requests, 1)
	assert.Equal(t, otc.ExecuteRequest{Account: otc.Account{UserHK: "user-1"}, QuoteID: quote, IdempotencyKey: "k1", IP: "192.0.2.1"},
		service.requests[0], "the trade is of the signed-in user")

	status, _ = serveExecute(t, h, "", `{"quote_id": "`+quote+`", "idempotency_key": "k2"}`)
	require.Equal(t, http.StatusCreated, status, "the key may come in the body")
	assert.Equal(t, "k2", service.requests[1].IdempotencyKey)
	service.replayed = true
	status, _ = serveExecute(t, h, "k1", body)
	assert.Equal(t, http.StatusOK, status, "a retry answers the first trade")
	service.replayed = false

	for _, tc := range []struct{ key, body string }{
		{"", body},
		{"k1", `{"quote_id": "q1"}`},
		{"k1", `{"quote_id": "` + quote + `", "idempotency_key": "k2"}`},
		{"k1", `{"quote_id": "` + quote + `", "sub_account_id": "main"}`},
		{"k1", `{"quote_id": "` + quote + `", "price": "1"}`},
	} {
		status, resp := serveExecute(t, h, tc.key, tc.body)
		assert.Equal(t, http.StatusBadRequest, status, tc.body)
		assert.NotEmpty(t, resp["code"], tc.body)
	}

	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{&otc.Refusal{Code: otc.CodeQuoteNotFound}, http.StatusNotFound, otc.CodeQuoteNotFound},
		{&otc.Refusal{Code: otc.CodeQuoteExpired}, http.StatusGone, otc.CodeQuoteExpired},
		{&otc.Refusal{Code: otc.CodeQuoteInvalid}, http.StatusConflict, otc.CodeQuoteInvalid},
		{&otc.Refusal{Code: otc.CodeQuoteNotSettleable}, http.StatusConflict, otc.CodeQuoteNotSettleable},
		{&otc.Refusal{Code: otc.CodeInventoryLimit}, http.StatusConflict, otc.CodeInventoryLimit},
		{&otc.Refusal{Code: otc.CodeTradeRefusedCompliance}, http.StatusForbidden, otc.CodeTradeRefusedCompliance},
		{&otc.Refusal{Code: otc.CodeTradeUnderReview}, http.StatusForbidden, otc.CodeTradeUnderReview},
		{&otc.Refusal{Code: otc.CodeTradeScreeningUnavailable}, http.StatusServiceUnavailable, otc.CodeTradeScreeningUnavailable},
		{&otc.Refusal{Code: otc.CodeIdempotencyKeyReused}, http.StatusConflict, otc.CodeIdempotencyKeyReused},
		{otc.ErrAgreementsPending, http.StatusForbidden, otc.CodeAgreementsPending},
		{otc.ErrInsufficientBalance, http.StatusConflict, otc.CodeInsufficientBalance},
		{&otc.Refusal{Code: otc.CodeMarketInactive}, http.StatusConflict, otc.CodeMarketInactive},
		{rowseal.ErrUnverified, http.StatusConflict, "row_unverified"},
		{&platform.Locked{Feature: platform.FeatureTrading}, http.StatusServiceUnavailable, platform.CodeFeatureLocked},
		{&restrictions.Refusal{Code: restrictions.CodeAccountFrozen}, http.StatusForbidden, restrictions.CodeAccountFrozen},
		{&verification.Refusal{Code: verification.CodeVerificationRequired, Action: verification.TradeOTC}, http.StatusForbidden,
			verification.CodeVerificationRequired},
		{errors.New("database is down"), http.StatusInternalServerError, "otc_trade_failed"},
	} {
		service.err = tc.err
		status, resp := serveExecute(t, h, "k1", body)
		assert.Equal(t, tc.status, status, tc.err.Error())
		assert.Equal(t, tc.code, resp["code"], tc.err.Error())
	}
}

// TestExecuteRefusals_HaveMessages checks that every refusal has a message.
func TestExecuteRefusals_HaveMessages(t *testing.T) {
	for code, answer := range executeRefusals {
		assert.NotZero(t, answer.status, code)
		assert.NotEmpty(t, answer.message, code)
	}
}
