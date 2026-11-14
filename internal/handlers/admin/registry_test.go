package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/assetregistry"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/domain/currency"
	"github.com/mdastpak/crypto-exchange/internal/domain/market"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRegistry answers the writes with err, and keeps what it was asked.
type fakeRegistry struct {
	err      error
	written  currencies.NetworkWritten
	market   currencies.MarketWritten
	network  assetregistry.Network
	decimals int
	asked    assetregistry.Market
	origins  []audit.Event
}

func (f *fakeRegistry) Networks(_ context.Context, hk string) ([]currency.Network, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []currency.Network{{Network: "tron"}}, nil
}

func (f *fakeRegistry) SetNetwork(_ context.Context, _ string, n assetregistry.Network, decimals int, origin audit.Event) (currencies.NetworkWritten, error) {
	f.network, f.decimals, f.origins = n, decimals, append(f.origins, origin)
	return f.written, f.err
}

func (f *fakeRegistry) Markets(context.Context) ([]market.Market, error) {
	return []market.Market{}, f.err
}

func (f *fakeRegistry) Market(_ context.Context, base, quote string) (market.Market, error) {
	return market.Market{BaseSymbol: base, QuoteSymbol: quote}, f.err
}

func (f *fakeRegistry) SetMarket(_ context.Context, m assetregistry.Market, origin audit.Event) (currencies.MarketWritten, error) {
	f.asked, f.origins = m, append(f.origins, origin)
	return f.market, f.err
}

// countingCatalog counts the drops of the cached currency list.
type countingCatalog struct{ drops int }

func (c *countingCatalog) Invalidate(context.Context) error {
	c.drops++
	return nil
}

func serveRegistry(t *testing.T, h *AdminHandler, method, path, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/currencies/:hk/networks", h.ListNetworks)
	r.PUT("/currencies/:hk/networks/:network", h.SetNetwork)
	r.GET("/markets", h.ListMarkets)
	r.GET("/markets/:base/:quote", h.GetMarket)
	r.PUT("/markets/:base/:quote", h.SetMarket)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return w.Code, resp
}

const tronNetwork = `{"contract_address": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "decimals": 6, "custody_mode": "ON_CHAIN",
	"deposit_enabled": true, "withdrawal_enabled": true, "min_withdrawal": "20", "max_withdrawal": "0",
	"withdrawal_fee": "1", "fee_currency": "TRX", "explorer_url": "https://tronscan.org"}`

func TestNetworkRoutes_RunWithoutInfrastructure(t *testing.T) {
	store := &fakeRegistry{written: currencies.NetworkWritten{Created: true, Changed: true, Public: true,
		Network: currency.Network{Network: "tron"}}}
	catalog := &countingCatalog{}
	h := &AdminHandler{Registry: store, Currencies: catalog}
	path := "/currencies/" + fakeHK + "/networks/tron"

	status, resp := serveRegistry(t, h, http.MethodPut, path, tronNetwork)
	require.Equal(t, http.StatusCreated, status, resp)
	assert.Equal(t, "tron", store.network.Network, "the network of the path")
	assert.Equal(t, "TRX", store.network.FeeAsset)
	assert.Equal(t, 6, store.decimals)
	assert.Equal(t, audit.ActorAdmin, store.origins[0].ActorType)
	assert.Equal(t, 1, catalog.drops, "the public list shows the network")

	store.written.Created, store.written.Changed = false, false
	status, _ = serveRegistry(t, h, http.MethodPut, path, tronNetwork)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, 1, catalog.drops, "nothing changed")

	for name, tc := range map[string]struct {
		body, field string
	}{
		"no decimals":         {`{"custody_mode": "ON_CHAIN"}`, "decimals"},
		"an explorer of http": {strings.Replace(tronNetwork, "https://tronscan.org", "http://tronscan.org", 1), "explorer_url"},
	} {
		t.Run(name, func(t *testing.T) {
			status, resp := serveRegistry(t, h, http.MethodPut, path, tc.body)
			assert.Equal(t, http.StatusBadRequest, status)
			assert.Contains(t, resp["info"].(map[string]any)["errors"], tc.field)
		})
	}
	status, _ = serveRegistry(t, h, http.MethodPut, path, `{"decimals": 6, "network": "tron"}`)
	assert.Equal(t, http.StatusBadRequest, status, "an unknown field")
	status, _ = serveRegistry(t, h, http.MethodPut, "/currencies/x/networks/tron", tronNetwork)
	assert.Equal(t, http.StatusBadRequest, status, "not a currency's hk")

	for err, want := range map[error]struct {
		status int
		code   string
	}{
		&currencies.Invalid{Problems: []assetregistry.Problem{{Field: "fee_asset", Text: "fee_asset is ..."}}}: {http.StatusBadRequest, "validation_failed"},
		currencies.ErrContractFixed: {http.StatusConflict, codeNetworkContractFixed},
		currencies.ErrContractTaken: {http.StatusConflict, codeContractTaken},
		rowseal.ErrUnverified:       {http.StatusConflict, codeRowUnverified},
		currencies.ErrUnrecorded:    {http.StatusInternalServerError, ""},
	} {
		store.err = err
		status, resp := serveRegistry(t, h, http.MethodPut, path, tronNetwork)
		assert.Equal(t, want.status, status, err.Error())
		if want.code != "" {
			assert.Equal(t, want.code, resp["code"], err.Error())
		}
		if want.code == "validation_failed" {
			assert.Contains(t, resp["info"].(map[string]any)["errors"], "fee_currency", "named as the request names it")
		}
	}
	store.err = currencies.ErrNotFound
	status, _ = serveRegistry(t, h, http.MethodPut, path, tronNetwork)
	assert.Equal(t, http.StatusNotFound, status)
	status, _ = serveRegistry(t, h, http.MethodGet, "/currencies/"+fakeHK+"/networks", "")
	assert.Equal(t, http.StatusNotFound, status)

	store.err = nil
	status, resp = serveRegistry(t, h, http.MethodGet, "/currencies/"+fakeHK+"/networks", "")
	require.Equal(t, http.StatusOK, status)
	assert.Len(t, resp["info"], 1)
}

const btcMarket = `{"status": "ACTIVE", "min_order_size": "0.0001", "max_order_size": "10", "min_notional": "10",
	"tick_size": "0.01", "lot_size": "0.0001", "maker_fee_bps": 10, "taker_fee_bps": 20}`

func TestMarketRoutes_RunWithoutInfrastructure(t *testing.T) {
	store := &fakeRegistry{market: currencies.MarketWritten{Created: true, Changed: true}}
	h := &AdminHandler{Registry: store}

	status, resp := serveRegistry(t, h, http.MethodPut, "/markets/BTC/USDT", btcMarket)
	require.Equal(t, http.StatusCreated, status, resp)
	assert.Equal(t, "BTC", store.asked.Base)
	assert.Equal(t, "USDT", store.asked.Quote)
	assert.Equal(t, 20, store.asked.TakerFeeBPS)

	status, resp = serveRegistry(t, h, http.MethodPut, "/markets/BTC/USDT", `{"status": "ACTIVE"}`)
	assert.Equal(t, http.StatusBadRequest, status)
	errs := resp["info"].(map[string]any)["errors"]
	assert.Contains(t, errs, "maker_fee_bps")
	assert.Contains(t, errs, "taker_fee_bps")

	store.err = &currencies.Invalid{Problems: []assetregistry.Problem{{Field: "base", Text: "no currency DOGE"}}}
	status, _ = serveRegistry(t, h, http.MethodPut, "/markets/DOGE/USDT", btcMarket)
	assert.Equal(t, http.StatusBadRequest, status)

	store.err = currencies.ErrMarketNotFound
	status, resp = serveRegistry(t, h, http.MethodGet, "/markets/BTC/DOGE", "")
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, codeMarketNotFound, resp["code"])

	store.err = rowseal.ErrUnverified
	for _, path := range []string{"/markets", "/markets/BTC/USDT"} {
		status, resp = serveRegistry(t, h, http.MethodGet, path, "")
		assert.Equal(t, http.StatusConflict, status, path)
		assert.Equal(t, codeRowUnverified, resp["code"], path)
	}
	_, resp = serveRegistry(t, h, http.MethodPut, "/markets/BTC/USDT", btcMarket)
	assert.Equal(t, codeRowUnverified, resp["code"], "a market changed in SQL is not written over")

	store.err = errors.New("database down")
	status, _ = serveRegistry(t, h, http.MethodGet, "/markets", "")
	assert.Equal(t, http.StatusInternalServerError, status)

	store.err = nil
	status, _ = serveRegistry(t, h, http.MethodGet, "/markets", "")
	assert.Equal(t, http.StatusOK, status)
	status, resp = serveRegistry(t, h, http.MethodGet, "/markets/btc/usdt", "")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "BTC", resp["info"].(map[string]any)["base_symbol"])
}
