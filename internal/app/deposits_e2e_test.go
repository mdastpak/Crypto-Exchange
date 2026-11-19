package app

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/assetregistry"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/deposits"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/tron"
	"github.com/mdastpak/crypto-exchange/internal/verification"
)

// TestDepositAddresses walks through the TRON deposit addresses of #68 on
// the routes: a user gets one address, derived from the xpub, which TRX and
// USDT share.
func TestDepositAddresses(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	reg, err := assetregistry.Load(filepath.Join("..", "..", "config", "assets.yaml"))
	require.NoError(t, err)
	_, err = currencies.Store{DB: env.db}.Import(ctx, reg, audit.Event{ActorType: audit.ActorSystem}, false)
	require.NoError(t, err)
	_, err = env.db.Exec(`UPDATE currency_networks SET deposit_enabled = true WHERE network = 'tron'`)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, env.db, rowseal.CurrencyNetworks, "network = 'tron'"))
	var usdt, trx string
	require.NoError(t, env.db.QueryRow(`SELECT hk FROM currencies WHERE info->>'symbol' = 'USDT'`).Scan(&usdt))
	require.NoError(t, env.db.QueryRow(`SELECT hk FROM currencies WHERE info->>'symbol' = 'TRX'`).Scan(&trx))

	x, err := tron.ParseXPub("xpub68Gmy5EdvgibQVfPdqkBBCHxA5htiqg55crXYuXoQRKfDBFA1WEjWgP6LHhwBZeNK1VTsfTFUHCdrfp1bgwQ9xv5ski8PX9rL2dZXvgGDnw")
	require.NoError(t, err)
	levels, ok := env.handlers.User.Verification.(verification.Store)
	require.True(t, ok)
	env.handlers.User.Deposits = deposits.Service{DB: env.db, Networks: map[string]deposits.Deriver{"tron": x}, Levels: levels}

	token, userID := env.signedInUser(t, "deposit@example.com")
	_, err = env.db.Exec(`UPDATE users SET kyc_tier = 'STANDARD', kyc_status = 'APPROVED' WHERE hk = $1`, userID)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, env.db, rowseal.Users, "hk = $1", userID))

	status, raw := env.call(t, http.MethodPost, "/api/v1/user/deposit-addresses", map[string]string{"currency_hk": usdt, "network": "tron"}, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	var got struct {
		Info deposits.Address `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &got))
	_, err = tron.ParseAddress(got.Info.Address)
	require.NoError(t, err)

	status, raw = env.call(t, http.MethodPost, "/api/v1/user/deposit-addresses", map[string]string{"currency_hk": trx, "network": "tron"}, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Contains(t, string(raw), got.Info.Address, "TRX and USDT share the address")
	status, raw = env.call(t, http.MethodPost, "/api/v1/user/deposit-addresses", map[string]string{"currency_hk": usdt, "network": "bitcoin"}, token)
	assert.Equal(t, http.StatusBadRequest, status, string(raw))
	status, raw = env.call(t, http.MethodGet, "/api/v1/user/deposit-addresses", nil, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Contains(t, string(raw), got.Info.Address)
}
