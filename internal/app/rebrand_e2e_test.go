package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/rebrand"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCurrencyRebrand_EndToEnd walks through #215 as MATIC became POL: the
// rename keeps MATIC as a previous symbol that finds POL and that no other
// currency can take, and a forced migration moves the holders of the old
// token to the new one.
func TestCurrencyRebrand_EndToEnd(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	enrollment, err := env.accounts.Create(ctx, "rebrand@example.com")
	require.NoError(t, err)
	admin := env.signInAdmin(t, enrollment)
	created := 0
	currency := func(symbol string) map[string]interface{} {
		created++
		c := testCurrency(symbol)
		info := c["info"].(map[string]interface{})
		info["en_name"], info["fa_name"] = "Token "+symbol, "Token "+symbol
		info["contract"] = "contract-" + symbol + "-" + string(rune('a'+created))
		return c
	}
	create := func(symbol string) string {
		t.Helper()
		status, resp := env.post(t, "/api/v1/admin/currencies", currency(symbol), admin)
		require.Equal(t, http.StatusCreated, status, resp.Message)
		hk, ok := resp.Info["hk"].(string)
		require.True(t, ok, resp.Info)
		return hk
	}
	matic := create("MATIC")

	// A rename with a new logo: MATIC stays a previous symbol.
	status, raw := env.call(t, http.MethodPut, "/api/v1/admin/currencies/"+matic, map[string]interface{}{
		"info": map[string]string{"symbol": "POL", "en_name": "Polygon Ecosystem Token", "logo_url": "https://example.com/pol.svg"},
	}, admin)
	require.Equal(t, http.StatusOK, status, string(raw))
	status, raw = env.call(t, http.MethodPut, "/api/v1/admin/currencies/"+matic, map[string]interface{}{
		"info": map[string]string{"logo_url": "http://example.com/pol.svg"},
	}, admin)
	assert.Equal(t, http.StatusBadRequest, status, "a logo is served over https: %s", raw)

	search := func(query string) []models.Currency {
		t.Helper()
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/public/currencies?"+query, nil))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var resp struct {
			Info struct {
				Items []models.Currency `json:"items"`
			} `json:"info"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		return resp.Info.Items
	}
	for _, query := range []string{"q=matic", "symbol=MATIC"} {
		found := search(query)
		require.Len(t, found, 1, query)
		assert.Equal(t, "POL", found[0].Info.Symbol)
		assert.Equal(t, []string{"MATIC"}, found[0].PreviousSymbols)
		assert.Equal(t, "https://example.com/pol.svg", found[0].Info.LogoURL)
	}

	// Another currency cannot take MATIC until it is released.
	status, raw = env.call(t, http.MethodPost, "/api/v1/admin/currencies", currency("MATIC"), admin)
	assert.Equal(t, http.StatusConflict, status)
	assert.Contains(t, string(raw), "symbol_reserved")
	status, _ = env.call(t, http.MethodDelete, "/api/v1/admin/currencies/"+matic+"/symbols/POL", nil, admin)
	assert.Equal(t, http.StatusConflict, status, "the current symbol stays")
	status, _ = env.call(t, http.MethodDelete, "/api/v1/admin/currencies/"+matic+"/symbols/NOPE", nil, admin)
	assert.Equal(t, http.StatusNotFound, status)
	status, raw = env.call(t, http.MethodDelete, "/api/v1/admin/currencies/"+matic+"/symbols/matic", nil, admin)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Empty(t, search("symbol=MATIC"), "a released symbol no longer finds the currency")
	create("MATIC")

	// A forced migration of an old token, LEND, into AAVE at 100 LEND = 1 AAVE.
	lend, aave := create("LEND"), create("AAVE")
	start := time.Now().UTC().Truncate(time.Second)
	at := func(h int) string { return start.Add(time.Duration(h) * time.Hour).Format(time.RFC3339) }
	path := "/api/v1/admin/currencies/" + aave + "/migrations"
	body := map[string]interface{}{
		"sources": []map[string]string{{"currency_hk": lend, "ratio": "0.01"}},
		"forced":  true,
		"steps": []map[string]string{
			{"phase": "announced", "at": at(0)}, {"phase": "deposits_paused", "at": at(1)},
			{"phase": "trading_halted", "at": at(2)}, {"phase": "converting", "at": at(3)}, {"phase": "completed", "at": at(4)},
		},
	}
	status, raw = env.call(t, http.MethodPost, path, body, admin)
	require.Equal(t, http.StatusCreated, status, string(raw))
	var scheduled struct {
		Info rebrand.Migration `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &scheduled))
	status, raw = env.call(t, http.MethodPost, path, body, admin)
	assert.Equal(t, http.StatusConflict, status)
	assert.Contains(t, string(raw), "source_in_migration")

	advance := func(hours int) models.Currency {
		t.Helper()
		_, err := rebrand.Advance(ctx, env.db, start.Add(time.Duration(hours)*time.Hour), 100)
		require.NoError(t, err)
		require.NoError(t, env.handlers.Catalog.Invalidate(ctx))
		found := search("symbol=LEND")
		if len(found) == 0 {
			return models.Currency{}
		}
		return found[0]
	}
	c := advance(1)
	require.NotNil(t, c.Migration)
	assert.Equal(t, "deposits_paused", c.Migration.Phase)
	assert.Equal(t, aave, c.Migration.TargetHK)
	assert.Equal(t, "0.01", c.Migration.Ratio)
	assert.False(t, c.Lifecycle.Capabilities.Deposit)
	assert.True(t, c.Lifecycle.Capabilities.Sell)

	status, raw = env.call(t, http.MethodGet, path, nil, admin)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, string(raw), scheduled.Info.ID)

	assert.Empty(t, advance(4).HK, "the completed source leaves the list")
	status, raw = env.call(t, http.MethodPost, path+"/"+scheduled.Info.ID+"/cancel", nil, admin)
	assert.Equal(t, http.StatusConflict, status)
	assert.Contains(t, string(raw), "migration_not_open")
}
