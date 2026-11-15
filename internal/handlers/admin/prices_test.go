package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/price"
	"github.com/mdastpak/crypto-exchange/internal/priceoverride"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

const priceAdmin = "5f0c9b4e-2d1a-4c3b-9e8f-7a6b5c4d3e2f"

type fakePrices struct {
	p   price.Price
	err error
}

func (f *fakePrices) Get(_ context.Context, market string) (price.Price, error) {
	f.p.Market = market
	return f.p, f.err
}

type fakeOverrides struct {
	err      error
	set      priceoverride.New
	ended    string
	approved string
	pending  bool
	origins  []audit.Event
	inForce  bool
	limit    int
}

func (f *fakeOverrides) Set(_ context.Context, n priceoverride.New, adminHK string, origin audit.Event) (priceoverride.Override, error) {
	f.set, f.origins = n, append(f.origins, origin)
	return priceoverride.Override{ID: "o1", Market: n.Market, Mid: n.Mid, InForce: !f.pending, Pending: f.pending, CreatedBy: adminHK}, f.err
}

func (f *fakeOverrides) Approve(_ context.Context, id, adminHK string, origin audit.Event) (priceoverride.Override, error) {
	f.approved, f.origins = id, append(f.origins, origin)
	return priceoverride.Override{ID: id, InForce: true, ApprovedBy: adminHK}, f.err
}

func (f *fakeOverrides) End(_ context.Context, id, _ string, origin audit.Event) (priceoverride.Override, error) {
	f.ended, f.origins = id, append(f.origins, origin)
	return priceoverride.Override{ID: id}, f.err
}

func (f *fakeOverrides) List(_ context.Context, inForce bool, limit int) ([]priceoverride.Override, error) {
	f.inForce, f.limit = inForce, limit
	return []priceoverride.Override{}, f.err
}

type countingRefresher struct{ refreshes int }

func (c *countingRefresher) Refresh(context.Context) error {
	c.refreshes++
	return nil
}

func servePrices(t *testing.T, h *AdminHandler, method, path, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { auth.SetClaims(c, &auth.Claims{UUID: priceAdmin, Role: auth.RoleAdmin}) })
	r.GET("/markets/:base/:quote/price", h.GetMarketPrice)
	r.POST("/markets/:base/:quote/price-override", h.SetPriceOverride)
	r.GET("/price-overrides", h.ListPriceOverrides)
	r.POST("/price-overrides/:id/end", h.EndPriceOverride)
	r.POST("/price-overrides/:id/approve", h.ApprovePriceOverride)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return w.Code, resp
}

func TestPriceRoutes_WithoutTheStoresAreUnavailable(t *testing.T) {
	h := &AdminHandler{}
	for _, r := range [][2]string{
		{http.MethodGet, "/markets/BTC/USDT/price"},
		{http.MethodPost, "/markets/BTC/USDT/price-override"},
		{http.MethodGet, "/price-overrides"},
		{http.MethodPost, "/price-overrides/o1/end"},
		{http.MethodPost, "/price-overrides/o1/approve"},
	} {
		status, resp := servePrices(t, h, r[0], r[1], `{}`)
		assert.Equal(t, http.StatusServiceUnavailable, status, r[1])
		assert.Equal(t, codePricesUnavailable, resp["code"], r[1])
	}
}

func TestGetMarketPrice(t *testing.T) {
	prices := &fakePrices{p: price.Price{Status: price.StatusLive, Mid: shared.MustNewDecimalFromString("64050")}}
	h := &AdminHandler{Prices: prices}
	status, resp := servePrices(t, h, http.MethodGet, "/markets/btc/usdt/price", "")
	require.Equal(t, http.StatusOK, status, resp)
	assert.Equal(t, "BTC/USDT", resp["info"].(map[string]any)["market"])

	for err, want := range map[error]int{
		price.ErrNoPrice:            http.StatusNotFound,
		price.ErrStale:              http.StatusNotFound,
		errors.New("redis is down"): http.StatusServiceUnavailable,
	} {
		prices.err = err
		status, _ := servePrices(t, h, http.MethodGet, "/markets/BTC/USDT/price", "")
		assert.Equal(t, want, status, err.Error())
	}
}

const btcOverride = `{"mid": "64050", "bid": "64000", "ask": "64100", "expires_in_seconds": 900, "reason": "Binance and Kraken are down"}`

func TestSetPriceOverride(t *testing.T) {
	store, watcher := &fakeOverrides{}, &countingRefresher{}
	h := &AdminHandler{PriceOverrides: store, OverrideWatcher: watcher, Registry: &fakeRegistry{}}

	status, resp := servePrices(t, h, http.MethodPost, "/markets/btc/usdt/price-override", btcOverride)
	require.Equal(t, http.StatusCreated, status, resp)
	assert.Equal(t, "BTC/USDT", store.set.Market)
	assert.Equal(t, "64000", store.set.Bid.String())
	assert.Equal(t, 15*time.Minute, store.set.For)
	assert.Equal(t, audit.ActorAdmin, store.origins[0].ActorType)
	assert.Equal(t, priceAdmin, store.origins[0].ActorID)
	assert.Equal(t, 1, watcher.refreshes, "applied on this instance at once")

	status, _ = servePrices(t, h, http.MethodPost, "/markets/BTC/USDT/price-override",
		`{"mid": "64050", "expires_in_seconds": 60, "reason": "r"}`)
	require.Equal(t, http.StatusCreated, status)
	assert.Equal(t, "64050", store.set.Bid.String(), "bid and ask default to mid")
	assert.Equal(t, "64050", store.set.Ask.String())

	for name, tc := range map[string]struct{ body, field string }{
		"no mid":         {`{"expires_in_seconds": 60, "reason": "r"}`, "mid"},
		"a mid of 0":     {`{"mid": "0", "expires_in_seconds": 60, "reason": "r"}`, "mid"},
		"bid above mid":  {`{"mid": "1", "bid": "2", "ask": "3", "expires_in_seconds": 60, "reason": "r"}`, "mid"},
		"no expiry":      {`{"mid": "1", "reason": "r"}`, "expires_in_seconds"},
		"over a day":     {`{"mid": "1", "expires_in_seconds": 86401, "reason": "r"}`, "expires_in_seconds"},
		"no reason":      {`{"mid": "1", "expires_in_seconds": 60}`, "reason"},
		"a word for ask": {`{"mid": "1", "ask": "high", "expires_in_seconds": 60, "reason": "r"}`, "ask"},
	} {
		t.Run(name, func(t *testing.T) {
			status, resp := servePrices(t, h, http.MethodPost, "/markets/BTC/USDT/price-override", tc.body)
			assert.Equal(t, http.StatusBadRequest, status)
			assert.Contains(t, resp["info"].(map[string]any)["errors"], tc.field)
		})
	}
	status, _ = servePrices(t, h, http.MethodPost, "/markets/BTC/USDT/price-override", `{"mid": "1", "status": "live"}`)
	assert.Equal(t, http.StatusBadRequest, status, "an unknown field")

	h.Registry = &fakeRegistry{err: currencies.ErrMarketNotFound}
	status, resp = servePrices(t, h, http.MethodPost, "/markets/BTC/DOGE/price-override", btcOverride)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, codeMarketNotFound, resp["code"])
	h.Registry = &fakeRegistry{}

	for err, want := range map[error]struct {
		status int
		code   string
	}{
		priceoverride.ErrExists:  {http.StatusConflict, codePriceOverrideExists},
		priceoverride.ErrInvalid: {http.StatusBadRequest, "validation_failed"},
		rowseal.ErrUnverified:    {http.StatusConflict, codeRowUnverified},
		errors.New("db down"):    {http.StatusInternalServerError, ""},
	} {
		store.err = err
		status, resp := servePrices(t, h, http.MethodPost, "/markets/BTC/USDT/price-override", btcOverride)
		assert.Equal(t, want.status, status, err.Error())
		if want.code != "" {
			assert.Equal(t, want.code, resp["code"], err.Error())
		}
	}
	assert.Equal(t, 2, watcher.refreshes, "not after a failure")
}

func TestListAndEndPriceOverrides(t *testing.T) {
	store, watcher := &fakeOverrides{}, &countingRefresher{}
	h := &AdminHandler{PriceOverrides: store, OverrideWatcher: watcher}

	status, _ := servePrices(t, h, http.MethodGet, "/price-overrides", "")
	require.Equal(t, http.StatusOK, status)
	assert.False(t, store.inForce)
	assert.Equal(t, priceoverride.MaxList, store.limit)
	status, _ = servePrices(t, h, http.MethodGet, "/price-overrides?in_force=true&limit=5", "")
	require.Equal(t, http.StatusOK, status)
	assert.True(t, store.inForce)
	assert.Equal(t, 5, store.limit)
	for _, q := range []string{"in_force=maybe", "limit=0", "limit=101", "limit=x"} {
		status, _ := servePrices(t, h, http.MethodGet, "/price-overrides?"+q, "")
		assert.Equal(t, http.StatusBadRequest, status, q)
	}

	status, _ = servePrices(t, h, http.MethodPost, "/price-overrides/o1/end", "")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "o1", store.ended)
	assert.Equal(t, 1, watcher.refreshes)
	for err, want := range map[error]string{
		priceoverride.ErrNotFound: codePriceOverrideMissing,
		priceoverride.ErrEnded:    codePriceOverrideEnded,
	} {
		store.err = err
		_, resp := servePrices(t, h, http.MethodPost, "/price-overrides/o1/end", "")
		assert.Equal(t, want, resp["code"])
	}
}

// TestSetPriceOverride_PassesTheLiveReference checks that the live price,
// and only a live one, bounds a new override (#453), and that a pending
// override says so.
func TestSetPriceOverride_PassesTheLiveReference(t *testing.T) {
	store := &fakeOverrides{}
	prices := &fakePrices{p: price.Price{Status: price.StatusLive, Mid: shared.MustNewDecimalFromString("64000")}}
	h := &AdminHandler{PriceOverrides: store, Prices: prices, Registry: &fakeRegistry{}}
	status, _ := servePrices(t, h, http.MethodPost, "/markets/btc/usdt/price-override", btcOverride)
	require.Equal(t, http.StatusCreated, status)
	require.NotNil(t, store.set.Reference)
	assert.Equal(t, "64000", store.set.Reference.String())

	for _, p := range []fakePrices{{p: price.Price{Status: price.StatusOverride, Mid: shared.MustNewDecimalFromString("1")}}, {err: price.ErrStale}} {
		*prices = p
		store.pending = true
		status, resp := servePrices(t, h, http.MethodPost, "/markets/btc/usdt/price-override", btcOverride)
		require.Equal(t, http.StatusCreated, status)
		assert.Nil(t, store.set.Reference, "an override or no price is no reference")
		assert.Equal(t, true, resp["info"].(map[string]any)["pending"])
	}
}

func TestApprovePriceOverride(t *testing.T) {
	store, watcher := &fakeOverrides{}, &countingRefresher{}
	h := &AdminHandler{PriceOverrides: store, OverrideWatcher: watcher}
	status, resp := servePrices(t, h, http.MethodPost, "/price-overrides/o1/approve", "")
	require.Equal(t, http.StatusOK, status, resp)
	assert.Equal(t, "o1", store.approved)
	assert.Equal(t, priceAdmin, store.origins[0].ActorID)
	assert.Equal(t, 1, watcher.refreshes, "applied on this instance at once")
	for err, want := range map[error]struct {
		status int
		code   string
	}{
		priceoverride.ErrSameAdmin:  {http.StatusForbidden, codeSecondApproval},
		priceoverride.ErrNotPending: {http.StatusConflict, codeNotPending},
		priceoverride.ErrLapsed:     {http.StatusConflict, codeOverrideLapsed},
		priceoverride.ErrEnded:      {http.StatusConflict, codePriceOverrideEnded},
		priceoverride.ErrNotFound:   {http.StatusNotFound, codePriceOverrideMissing},
	} {
		store.err = err
		status, resp := servePrices(t, h, http.MethodPost, "/price-overrides/o1/approve", "")
		assert.Equal(t, want.status, status, err.Error())
		assert.Equal(t, want.code, resp["code"], err.Error())
	}
}
