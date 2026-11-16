package user

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/balances"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/pagination"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeBalances struct {
	resolveErr error
	list       []balances.Balance
	listErr    error
	history    []balances.Change
	gotFilter  balances.Filter
	gotAfter   *balances.Position
	gotLimit   int
}

func (f *fakeBalances) Resolve(_ context.Context, _, raw string) (balances.Account, error) {
	if f.resolveErr != nil {
		return balances.Account{}, f.resolveErr
	}
	switch raw {
	case "all":
		return balances.Account{All: true}, nil
	case "", "main":
		return balances.Account{}, nil
	}
	id := uuid.MustParse(raw)
	return balances.Account{Sub: &id}, nil
}

func (f *fakeBalances) Balances(context.Context, string, balances.Account, string) ([]balances.Balance, error) {
	return f.list, f.listErr
}

func (f *fakeBalances) History(_ context.Context, _ string, filter balances.Filter, after *balances.Position, limit int) ([]balances.Change, error) {
	f.gotFilter, f.gotAfter, f.gotLimit = filter, after, limit
	if len(f.history) > limit {
		return f.history[:limit], nil
	}
	return f.history, nil
}

func serveBalances(t *testing.T, h *UserHandler, path string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		auth.SetClaims(c, &auth.Claims{UUID: uuid.NewString(), SessionID: uuid.NewString(), Role: auth.RoleUser})
	})
	r.GET("/balances", h.GetBalances)
	r.GET("/transactions", h.ListTransactions)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return w.Code, resp
}

func TestBalanceRoutes(t *testing.T) {
	status, resp := serveBalances(t, &UserHandler{}, "/balances")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, codeBalancesUnavailable, resp["code"])

	sub := uuid.NewString()
	fake := &fakeBalances{list: []balances.Balance{
		{CurrencyHK: "c1", Symbol: "BTC", Available: shared.MustNewDecimalFromString("0.5")},
		{CurrencyHK: "c2", Symbol: "USDT", Available: shared.MustNewDecimalFromString("95.5")},
		{CurrencyHK: "c2", Symbol: "USDT", SubAccountID: &sub, Available: shared.MustNewDecimalFromString("30")},
	}}
	h := &UserHandler{Balances: fake}
	status, resp = serveBalances(t, h, "/balances?account=all")
	require.Equal(t, http.StatusOK, status)
	accounts := resp["info"].(map[string]any)["accounts"].([]any)
	require.Len(t, accounts, 2, "grouped by account")
	first := accounts[0].(map[string]any)
	assert.Nil(t, first["sub_account_id"], "the main account first")
	assert.Len(t, first["balances"], 2)
	assert.Equal(t, "95.5", first["balances"].([]any)[1].(map[string]any)["available"], "amounts are strings")
	assert.Equal(t, sub, accounts[1].(map[string]any)["sub_account_id"])

	fake.list = nil
	status, resp = serveBalances(t, h, "/balances?account="+sub)
	require.Equal(t, http.StatusOK, status)
	accounts = resp["info"].(map[string]any)["accounts"].([]any)
	require.Len(t, accounts, 1, "an account without balances is one empty group")
	assert.Equal(t, sub, accounts[0].(map[string]any)["sub_account_id"])
	assert.Empty(t, accounts[0].(map[string]any)["balances"])

	status, resp = serveBalances(t, h, "/balances?currency=usdt")
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, resp["info"].(map[string]any)["errors"], "currency")

	fake.resolveErr = balances.ErrAccountNotFound
	status, resp = serveBalances(t, h, "/balances?account="+uuid.NewString())
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, codeAccountNotFound, resp["code"])
	fake.resolveErr = nil

	fake.listErr = &balances.Unverified{CurrencyHK: "c2", Err: rowseal.ErrUnverified}
	status, resp = serveBalances(t, h, "/balances")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, codeBalanceUnverified, resp["code"])
	assert.Equal(t, map[string]any{"currency_hk": "c2"}, resp["info"])
}

func TestTransactionRoutes(t *testing.T) {
	at := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	var history []balances.Change
	for i := range 3 {
		history = append(history, balances.Change{ID: uuid.NewString(), Kind: "lock", Time: at.Add(-time.Duration(i) * time.Minute),
			Amount: shared.MustNewDecimalFromString("1")})
	}
	fake := &fakeBalances{history: history}
	h := &UserHandler{Balances: fake}

	status, resp := serveBalances(t, h, "/transactions?limit=2&kind=lock&since=2026-11-01T00:00:00Z&until=2026-11-17T00:00:00Z")
	require.Equal(t, http.StatusOK, status)
	info := resp["info"].(map[string]any)
	assert.Len(t, info["items"], 2)
	require.NotEmpty(t, info["next_cursor"])
	assert.Equal(t, 3, fake.gotLimit, "one more than the page, to know of a next one")
	assert.Equal(t, "lock", fake.gotFilter.Kind)
	assert.Nil(t, fake.gotAfter)

	status, _ = serveBalances(t, h, "/transactions?limit=2&cursor="+info["next_cursor"].(string))
	require.Equal(t, http.StatusOK, status)
	require.NotNil(t, fake.gotAfter)
	assert.Equal(t, history[1].ID, fake.gotAfter.ID, "the next page starts after the last item")
	assert.True(t, history[1].Time.Equal(fake.gotAfter.Time))

	other, err := pagination.Cursor(map[string]any{"id": 7})
	require.NoError(t, err)
	for name, query := range map[string]string{
		"kind":       "kind=bonus",
		"since":      "since=yesterday",
		"until":      "since=2026-11-02T00:00:00Z&until=2026-11-01T00:00:00Z",
		"cursor":     "cursor=" + other,
		"currency":   "currency=1",
		"bad cursor": "cursor=not-a-cursor",
	} {
		status, resp := serveBalances(t, h, "/transactions?"+query)
		assert.Equal(t, http.StatusBadRequest, status, name)
		assert.Equal(t, "validation_failed", resp["code"], name)
	}

	fake.resolveErr = balances.ErrAccountNotFound
	status, resp = serveBalances(t, h, "/transactions?account="+uuid.NewString())
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, codeAccountNotFound, resp["code"])
}
