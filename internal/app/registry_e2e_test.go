package app

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/assetregistry"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAdminRegistry walks through #57: the registry file is imported, an
// administrator changes a network and a market, and users see them in the
// public currency list and the public markets. A contract does not change
// through the API.
func TestAdminRegistry(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	reg, err := assetregistry.Load(filepath.Join("..", "..", "config", "assets.yaml"))
	require.NoError(t, err)
	_, err = currencies.Store{DB: env.db}.Import(ctx, reg, audit.Event{ActorType: audit.ActorSystem}, false)
	require.NoError(t, err)
	enrollment, err := env.accounts.Create(ctx, "registry-ops@example.com")
	require.NoError(t, err)
	token := env.signInAdmin(t, enrollment)
	var usdt string
	require.NoError(t, env.db.QueryRow(`SELECT hk FROM currencies WHERE info->>'symbol' = 'USDT'`).Scan(&usdt))

	network := map[string]any{
		"contract_address": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "decimals": 6, "custody_mode": "ON_CHAIN",
		"deposit_enabled": true, "withdrawal_enabled": false, "min_withdrawal": "20", "max_withdrawal": "1000000",
		"withdrawal_fee": "2.5", "fee_currency": "TRX", "explorer_url": "https://tronscan.org/#/transaction/",
	}
	route := "/api/v1/admin/currencies/" + usdt + "/networks/tron"
	status, raw := env.call(t, http.MethodPut, route, network, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Contains(t, string(raw), `"withdrawal_fee":"2.5"`)

	network["contract_address"] = "TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8"
	status, resp := env.put(t, route, network, token)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "network_contract_fixed", resp.Code)
	status, raw = env.call(t, http.MethodGet, "/api/v1/admin/currencies/"+usdt+"/networks", nil, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Contains(t, string(raw), "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "the contract stays")

	status, raw = env.call(t, http.MethodGet, "/api/v1/public/currencies/"+usdt, nil, "")
	require.Equal(t, http.StatusOK, status, string(raw))
	var currency struct {
		Info models.Currency `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &currency))
	require.Len(t, currency.Info.Networks, 2)
	tron := currency.Info.Networks[1]
	assert.Equal(t, "tron", tron.Network)
	assert.Equal(t, "2.5", tron.WithdrawalFee, "users see the change at once")
	assert.False(t, tron.WithdrawalEnabled)
	assert.Equal(t, "TRX", tron.FeeCurrency)

	market := map[string]any{"status": "SUSPENDED", "min_order_size": "0.0001", "max_order_size": "10",
		"min_notional": "10", "tick_size": "0.01", "lot_size": "0.0001", "maker_fee_bps": 10, "taker_fee_bps": 25}
	status, raw = env.call(t, http.MethodPut, "/api/v1/admin/markets/BTC/USDT", market, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	market["taker_fee_bps"] = 2000
	status, resp = env.put(t, "/api/v1/admin/markets/BTC/USDT", market, token)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "validation_failed", resp.Code)

	status, raw = env.call(t, http.MethodGet, "/api/v1/public/markets", nil, "")
	require.Equal(t, http.StatusOK, status, string(raw))
	var markets struct {
		Info []currencies.PublicMarket `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &markets))
	bySymbol := map[string]currencies.PublicMarket{}
	for _, m := range markets.Info {
		bySymbol[m.Symbol] = m
	}
	require.Contains(t, bySymbol, "BTC/USDT")
	assert.Equal(t, "SUSPENDED", bySymbol["BTC/USDT"].Status)
	assert.Equal(t, 25, bySymbol["BTC/USDT"].TakerFeeBPS)
	assert.NotContains(t, bySymbol, "BTC/TRX", "INACTIVE")

	networkEvents := auditEvents(t, env.db, audit.ActionCurrencyNetworkSet)
	last := networkEvents[len(networkEvents)-1]
	assert.Equal(t, audit.ActorAdmin, last.ActorType)
	assert.Equal(t, enrollment.AdminID, last.ActorID.String)
	marketEvents := auditEvents(t, env.db, audit.ActionMarketSet)
	assert.Equal(t, audit.ActorAdmin, marketEvents[len(marketEvents)-1].ActorType)
}
