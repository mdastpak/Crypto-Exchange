package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUserFavorites_EndToEnd walks through #217: a user adds, reorders, and
// removes favorite currencies, and filters the public list by them.
func TestUserFavorites_EndToEnd(t *testing.T) {
	env := newE2EEnv(t)
	hks := map[string]string{}
	for _, symbol := range []string{"BTC", "ETH", "USDT"} {
		info, err := json.Marshal(models.CurrencyInfo{Symbol: symbol, EnName: symbol, FaName: symbol})
		require.NoError(t, err)
		var hk string
		require.NoError(t, env.db.QueryRow(`INSERT INTO currencies (status, info) VALUES ('approved', $1::jsonb) RETURNING hk`, info).Scan(&hk))
		hks[symbol] = hk
	}
	require.NoError(t, env.handlers.Public.Currencies.Invalidate(context.Background()))
	token, _ := env.signedInUser(t, "favorites@example.com")
	other, _ := env.signedInUser(t, "other-fav@example.com")

	put := func(token, path string) (int, map[string]interface{}) {
		status, raw := env.call(t, http.MethodPut, path, nil, token)
		var body map[string]interface{}
		require.NoError(t, json.Unmarshal(raw, &body), string(raw))
		return status, body
	}
	for _, symbol := range []string{"ETH", "BTC"} {
		status, body := put(token, "/api/v1/user/favorites/currencies/"+hks[symbol])
		require.Equal(t, http.StatusOK, status, body)
		assert.Equal(t, true, body["info"].(map[string]interface{})["changed"])
	}
	status, body := put(token, "/api/v1/user/favorites/currencies/"+hks["BTC"])
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, false, body["info"].(map[string]interface{})["changed"], "a repeated add")
	status, _ = put(token, "/api/v1/user/favorites/currencies/00000000-0000-4000-8000-000000000000")
	assert.Equal(t, http.StatusNotFound, status)
	status, _ = put("", "/api/v1/user/favorites/currencies/"+hks["BTC"])
	assert.Equal(t, http.StatusUnauthorized, status)

	favoriteSymbols := func(token string) []string {
		status, body := env.sessionCall(t, http.MethodGet, "/api/v1/user/favorites", token)
		require.Equal(t, http.StatusOK, status, body)
		out := []string{}
		for _, c := range body["info"].(map[string]interface{})["currencies"].([]interface{}) {
			out = append(out, c.(map[string]interface{})["symbol"].(string))
		}
		return out
	}
	assert.Equal(t, []string{"ETH", "BTC"}, favoriteSymbols(token))
	assert.Empty(t, favoriteSymbols(other), "favorites are per user")

	// Reorder: every favorite once, or nothing changes.
	status, raw := env.call(t, http.MethodPut, "/api/v1/user/favorites/order", map[string][]string{"currencies": {hks["BTC"]}}, token)
	assert.Equal(t, http.StatusConflict, status, string(raw))
	assert.Contains(t, string(raw), "order_mismatch")
	status, raw = env.call(t, http.MethodPut, "/api/v1/user/favorites/order", map[string][]string{"currencies": {hks["BTC"], hks["ETH"]}}, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Equal(t, []string{"BTC", "ETH"}, favoriteSymbols(token))

	// The public list, filtered by the favorites of the token's user.
	listed := func(token string) (int, []string) {
		status, raw := env.call(t, http.MethodGet, "/api/v1/public/currencies?favorites=true&sort=symbol", nil, token)
		var resp struct {
			Info struct {
				Items []models.Currency `json:"items"`
			} `json:"info"`
		}
		require.NoError(t, json.Unmarshal(raw, &resp), string(raw))
		out := []string{}
		for _, c := range resp.Info.Items {
			out = append(out, c.Info.Symbol)
		}
		return status, out
	}
	status, got := listed(token)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, []string{"BTC", "ETH"}, got)
	status, got = listed(other)
	require.Equal(t, http.StatusOK, status)
	assert.Empty(t, got)
	status, _ = listed("")
	assert.Equal(t, http.StatusUnauthorized, status, "favorites=true needs a token")
	status, _ = listed("not-a-token")
	assert.Equal(t, http.StatusUnauthorized, status)

	status, _ = env.call(t, http.MethodDelete, "/api/v1/user/favorites/currencies/"+hks["BTC"], nil, token)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, []string{"ETH"}, favoriteSymbols(token))
}
