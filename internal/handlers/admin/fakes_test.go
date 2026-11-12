package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCurrencies is a CurrencyStore in memory: one currency, which Update
// edits as the database store would, and errors to answer with.
type fakeCurrencies struct {
	status  string
	info    models.CurrencyInfo
	version int64
	deleted bool
	// origins are the audit origins of the changes.
	origins []audit.Event
}

func (f *fakeCurrencies) check(expected *int64) error {
	if expected != nil && *expected != f.version {
		return &currencies.VersionConflict{Current: f.version}
	}
	return nil
}

func (f *fakeCurrencies) Create(_ context.Context, status string, info models.CurrencyInfo, origin audit.Event) (currencies.Written, error) {
	f.status, f.info, f.version, f.origins = status, info, 1, append(f.origins, origin)
	return currencies.Written{HK: "11111111-1111-1111-1111-111111111111", Version: 1}, nil
}

func (f *fakeCurrencies) Update(_ context.Context, hk string, expected *int64, edit currencies.Edit, origin audit.Event) (currencies.Written, error) {
	if f.deleted {
		return currencies.Written{}, currencies.ErrNotFound
	}
	if err := f.check(expected); err != nil {
		return currencies.Written{}, err
	}
	status, info, err := edit(f.status, f.info)
	if err != nil {
		return currencies.Written{}, err
	}
	f.status, f.info, f.origins = status, info, append(f.origins, origin)
	f.version++
	return currencies.Written{HK: hk, Version: f.version}, nil
}

func (f *fakeCurrencies) Delete(_ context.Context, hk string, expected *int64, _ audit.Event) (currencies.Written, error) {
	if f.deleted {
		return currencies.Written{}, currencies.ErrAlreadyDeleted
	}
	if err := f.check(expected); err != nil {
		return currencies.Written{}, err
	}
	f.deleted = true
	f.version++
	return currencies.Written{HK: hk, Version: f.version}, nil
}

func (f *fakeCurrencies) Restore(_ context.Context, hk string, expected *int64, _ audit.Event) (currencies.Written, error) {
	if err := f.check(expected); err != nil {
		return currencies.Written{}, err
	}
	if !f.deleted {
		return currencies.Written{}, currencies.ErrNotDeleted
	}
	f.deleted = false
	f.version++
	return currencies.Written{HK: hk, Version: f.version}, nil
}

func (f *fakeCurrencies) ReleaseSymbol(context.Context, string, string, audit.Event) (currencies.Written, error) {
	return currencies.Written{}, currencies.ErrSymbolCurrent
}

func (f *fakeCurrencies) Get(context.Context, string) (models.Currency, error) {
	return models.Currency{}, currencies.ErrNotFound
}

func (f *fakeCurrencies) All(context.Context) ([]models.Currency, error) { return nil, nil }

const fakeHK = "22222222-2222-2222-2222-222222222222"

// serve runs one request against a router of the currency routes of h.
func serve(t *testing.T, h *AdminHandler, method, path, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.PUT("/currencies/:hk", h.UpdateCurrency)
	r.DELETE("/currencies/:hk", h.DeleteCurrency)
	r.POST("/currencies/:hk/restore", h.RestoreCurrency)
	r.GET("/currencies/:hk", h.GetCurrency)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return w.Code, resp
}

// TestCurrencyRoutes_RunWithoutInfrastructure runs the currency routes on a
// fake store (#48): the handler validates, maps the errors of the store to
// their answers, and never touches a database.
func TestCurrencyRoutes_RunWithoutInfrastructure(t *testing.T) {
	store := &fakeCurrencies{status: models.CurrencyStatusPending, version: 3,
		info: models.CurrencyInfo{EnName: "Bitcoin", FaName: "بیت‌کوین", Symbol: "BTC"}}
	h := &AdminHandler{CurrencyStore: store}

	status, resp := serve(t, h, http.MethodPut, "/currencies/"+fakeHK, `{"info": {"en_name": "Bitcoin Core"}, "version": 3}`)
	require.Equal(t, http.StatusOK, status, resp)
	assert.Equal(t, "Bitcoin Core", store.info.EnName)
	assert.Equal(t, "BTC", store.info.Symbol, "a partial update keeps the other fields")
	assert.EqualValues(t, 4, resp["info"].(map[string]any)["version"])
	require.Len(t, store.origins, 1)
	assert.Equal(t, audit.ActorAdmin, store.origins[0].ActorType, "the change is an administrator's")

	status, resp = serve(t, h, http.MethodPut, "/currencies/"+fakeHK, `{"info": {"symbol": "b"}}`)
	assert.Equal(t, http.StatusBadRequest, status, "the edit validates the result")
	assert.Equal(t, "validation_failed", resp["code"])
	assert.Equal(t, "BTC", store.info.Symbol, "nothing changes")

	status, resp = serve(t, h, http.MethodDelete, "/currencies/"+fakeHK+"?version=3", "")
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "version_conflict", resp["code"])

	status, _ = serve(t, h, http.MethodDelete, "/currencies/"+fakeHK, "")
	assert.Equal(t, http.StatusOK, status)
	status, _ = serve(t, h, http.MethodDelete, "/currencies/"+fakeHK, "")
	assert.Equal(t, http.StatusNotFound, status, "a deleted currency is deleted once")
	status, _ = serve(t, h, http.MethodPut, "/currencies/"+fakeHK, `{"info": {"en_name": "X"}}`)
	assert.Equal(t, http.StatusNotFound, status, "a deleted currency is not updated")

	status, _ = serve(t, h, http.MethodPost, "/currencies/"+fakeHK+"/restore", "")
	assert.Equal(t, http.StatusOK, status)
	status, resp = serve(t, h, http.MethodPost, "/currencies/"+fakeHK+"/restore", "")
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "not_deleted", resp["code"])

	status, _ = serve(t, h, http.MethodGet, "/currencies/"+fakeHK, "")
	assert.Equal(t, http.StatusNotFound, status)
}
