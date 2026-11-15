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

	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/otc"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
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
