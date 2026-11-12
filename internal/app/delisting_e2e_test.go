package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/delisting"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCurrencyDelisting_EndToEnd walks through #216: an administrator
// schedules a delisting, overrides a capability, and the public currency
// shows the lifecycle as the steps apply, until the currency leaves the
// list.
func TestCurrencyDelisting_EndToEnd(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	enrollment, err := env.accounts.Create(ctx, "delist@example.com")
	require.NoError(t, err)
	admin := env.signInAdmin(t, enrollment)
	hks := map[string]string{}
	for _, symbol := range []string{"OLD", "USDT"} {
		status, resp := env.post(t, "/api/v1/admin/currencies", testCurrency(symbol), admin)
		require.Equal(t, http.StatusCreated, status, resp.Message)
		hk, ok := resp.Info["hk"].(string)
		require.True(t, ok, resp.Info)
		hks[symbol] = hk
	}

	publicCurrency := func() models.Currency {
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/public/currencies/"+hks["OLD"], nil))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var resp struct {
			Info models.Currency `json:"info"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		require.NotNil(t, resp.Info.Lifecycle)
		return resp.Info
	}
	assert.Equal(t, "active", publicCurrency().Lifecycle.State)

	path := "/api/v1/admin/currencies/" + hks["OLD"] + "/delistings"
	start := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	at := func(h int) string { return start.Add(time.Duration(h) * time.Hour).Format(time.RFC3339) }
	status, raw := env.call(t, http.MethodPost, path, map[string]interface{}{
		"kind": "standard", "settlement": "convert", "settlement_currency_hk": hks["USDT"], "price_source": "twap",
		"steps": []map[string]string{{"state": "deposits_disabled", "at": at(0)}},
	}, admin)
	assert.Equal(t, http.StatusBadRequest, status, "a standard delisting starts with the notice: %s", raw)

	status, raw = env.call(t, http.MethodPost, path, map[string]interface{}{
		"kind": "standard", "settlement": "convert", "settlement_currency_hk": hks["USDT"], "price_source": "twap",
		"announcement_url": "https://example.com/news/old-delisting",
		"steps": []map[string]string{
			{"state": "delist_announced", "at": at(0)}, {"state": "deposits_disabled", "at": at(1)},
			{"state": "trading_halted", "at": at(2)}, {"state": "withdrawal_only", "at": at(3)},
			{"state": "frozen", "at": at(4)}, {"state": "delisted", "at": at(5)},
		},
	}, admin)
	require.Equal(t, http.StatusCreated, status, string(raw))
	var scheduled struct {
		Info delisting.Delisting `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &scheduled))
	status, raw = env.call(t, http.MethodPost, path, map[string]interface{}{
		"kind": "emergency", "settlement": "claim", "claim_days": 30,
		"steps": []map[string]string{{"state": "trading_halted", "at": at(0)}},
	}, admin)
	assert.Equal(t, http.StatusConflict, status)
	assert.Contains(t, string(raw), "delisting_in_progress")

	l := publicCurrency().Lifecycle
	require.NotNil(t, l.Next, "clients can show a banner")
	assert.Equal(t, "delist_announced", l.Next.State)
	assert.True(t, l.Next.At.Equal(start))

	// An incident pauses the deposits of the currency at once.
	capability := "/api/v1/admin/currencies/" + hks["OLD"] + "/capabilities/deposit"
	status, raw = env.call(t, http.MethodPut, capability, map[string]interface{}{"allowed": false, "note": "node upgrade"}, admin)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.False(t, publicCurrency().Lifecycle.Capabilities.Deposit)
	status, _ = env.call(t, http.MethodPut, "/api/v1/admin/currencies/"+hks["OLD"]+"/capabilities/fly", map[string]interface{}{"allowed": false}, admin)
	assert.Equal(t, http.StatusNotFound, status)
	status, raw = env.call(t, http.MethodDelete, capability, nil, admin)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.True(t, publicCurrency().Lifecycle.Capabilities.Deposit)

	// The scheduler of an instance applies the steps as they come due.
	advance := func(hours int) {
		t.Helper()
		_, err := delisting.Advance(ctx, env.db, start.Add(time.Duration(hours)*time.Hour), 100)
		require.NoError(t, err)
		require.NoError(t, env.handlers.Public.Currencies.Invalidate(ctx))
	}
	advance(2)
	l = publicCurrency().Lifecycle
	assert.Equal(t, "trading_halted", l.State)
	assert.False(t, l.Capabilities.Sell)
	assert.True(t, l.Capabilities.Withdraw)
	assert.Equal(t, "withdrawal_only", l.Next.State)
	assert.Equal(t, []string{"OLD", "USDT"}, listCurrencies(t, env.router))

	status, raw = env.call(t, http.MethodGet, path, nil, admin)
	require.Equal(t, http.StatusOK, status)
	var list struct {
		Info DelistingListBody `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Len(t, list.Info.Items, 1)
	assert.NotNil(t, list.Info.Items[0].Steps[2].AppliedAt)
	assert.Nil(t, list.Info.Items[0].Steps[3].AppliedAt)

	advance(5)
	assert.Equal(t, "delisted", publicCurrency().Lifecycle.State, "a delisted currency stays readable by its hk")
	assert.Equal(t, []string{"USDT"}, listCurrencies(t, env.router), "and leaves the list")
	status, raw = env.call(t, http.MethodPost, path+"/"+scheduled.Info.ID+"/cancel", nil, admin)
	assert.Equal(t, http.StatusConflict, status)
	assert.Contains(t, string(raw), "delisting_not_scheduled")
}

// DelistingListBody is the info of a list of delistings.
type DelistingListBody struct {
	Items []delisting.Delisting `json:"items"`
}

// TestCurrencyDelisting_CancelRestoresTheCurrency cancels a delisting that
// has halted trading.
func TestCurrencyDelisting_CancelRestoresTheCurrency(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	enrollment, err := env.accounts.Create(ctx, "relist@example.com")
	require.NoError(t, err)
	admin := env.signInAdmin(t, enrollment)
	status, resp := env.post(t, "/api/v1/admin/currencies", testCurrency("BACK"), admin)
	require.Equal(t, http.StatusCreated, status, resp.Message)
	hk, ok := resp.Info["hk"].(string)
	require.True(t, ok, resp.Info)
	start := time.Now().UTC()
	status, raw := env.call(t, http.MethodPost, "/api/v1/admin/currencies/"+hk+"/delistings", map[string]interface{}{
		"kind": "emergency", "settlement": "claim", "claim_days": 90,
		"steps": []map[string]string{{"state": "trading_halted", "at": start.Format(time.RFC3339)}},
	}, admin)
	require.Equal(t, http.StatusCreated, status, string(raw))
	var d struct {
		Info delisting.Delisting `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &d))
	_, err = delisting.Advance(ctx, env.db, start.Add(time.Minute), 10)
	require.NoError(t, err)

	cancel := "/api/v1/admin/currencies/" + hk + "/delistings/" + d.Info.ID + "/cancel"
	status, raw = env.call(t, http.MethodPost, cancel, map[string]int64{"version": d.Info.Version + 1}, admin)
	assert.Equal(t, http.StatusConflict, status)
	assert.Contains(t, string(raw), "version_conflict")
	status, raw = env.call(t, http.MethodPost, cancel, map[string]int64{"version": d.Info.Version}, admin)
	require.Equal(t, http.StatusOK, status, string(raw))
	require.NoError(t, env.handlers.Public.Currencies.Invalidate(ctx))
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/public/currencies?lifecycle=active", nil))
	assert.Equal(t, []string{"BACK"}, listedSymbols(t, w))
}
