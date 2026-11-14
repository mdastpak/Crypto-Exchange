package public

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeMarkets struct {
	list []currencies.PublicMarket
	err  error
}

func (f fakeMarkets) List(context.Context) ([]currencies.PublicMarket, error) { return f.list, f.err }

type fakeCurrencyList struct {
	list []models.Currency
	err  error
}

func (f fakeCurrencyList) List(context.Context) ([]models.Currency, error) { return f.list, f.err }

func (f fakeCurrencyList) Get(context.Context, string) (models.Currency, bool, error) {
	return models.Currency{}, false, f.err
}

func serveMarkets(t *testing.T, h *PublicHandler) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/markets", h.GetMarkets)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/markets", nil))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return w, resp
}

func TestGetMarkets_ListsTheMarketsOfListedCurrencies(t *testing.T) {
	markets := fakeMarkets{list: []currencies.PublicMarket{
		{Symbol: "BTC/USDT", BaseHK: "btc", QuoteHK: "usdt"},
		{Symbol: "OLD/USDT", BaseHK: "old", QuoteHK: "usdt"},
		{Symbol: "ETH/USDT", BaseHK: "eth", QuoteHK: "usdt"},
	}}
	delisted := &models.CurrencyLifecycle{}
	delisted.Capabilities.Listed = false
	h := &PublicHandler{Markets: markets, Currencies: fakeCurrencyList{list: []models.Currency{
		{HK: "btc"}, {HK: "usdt"}, {HK: "old", Lifecycle: delisted},
	}}}

	w, resp := serveMarkets(t, h)
	require.Equal(t, http.StatusOK, w.Code, resp)
	var symbols []string
	for _, m := range resp["info"].([]any) {
		symbols = append(symbols, m.(map[string]any)["symbol"].(string))
	}
	assert.Equal(t, []string{"BTC/USDT"}, symbols, "a delisted currency, or one that is not public, takes its markets")

	for name, h := range map[string]*PublicHandler{
		"no markets":    {Markets: fakeMarkets{err: errors.New("down")}, Currencies: h.Currencies},
		"no currencies": {Markets: markets, Currencies: fakeCurrencyList{err: errors.New("down")}},
		"no store":      {},
	} {
		t.Run(name, func(t *testing.T) {
			w, resp := serveMarkets(t, h)
			assert.Equal(t, http.StatusServiceUnavailable, w.Code)
			assert.Equal(t, "service_unavailable", resp["code"])
			assert.NotEmpty(t, w.Header().Get("Retry-After"))
		})
	}
}
