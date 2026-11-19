package user

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/deposits"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

type fakeDeposits struct{ err error }

func (f fakeDeposits) Get(context.Context, string, string, string, audit.Event) (deposits.Address, error) {
	return deposits.Address{Network: "tron", Address: "TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7"}, f.err
}

func (f fakeDeposits) List(context.Context, string) ([]deposits.Address, error) {
	return []deposits.Address{}, f.err
}

func (f fakeDeposits) History(context.Context, string) ([]deposits.UserDeposit, error) {
	return []deposits.UserDeposit{{Network: "tron", Status: deposits.StatusDetected}}, f.err
}

func TestDepositHistory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	serve := func(h *UserHandler) (int, map[string]any) {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			auth.SetClaims(c, &auth.Claims{UUID: uuid.NewString(), SessionID: uuid.NewString(), Role: auth.RoleUser})
		})
		r.GET("/deposits", h.ListDeposits)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/deposits", nil))
		var resp map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
		return w.Code, resp
	}
	status, resp := serve(&UserHandler{Deposits: fakeDeposits{}})
	assert.Equal(t, http.StatusOK, status)
	assert.Len(t, resp["info"], 1)
	status, resp = serve(&UserHandler{Deposits: fakeDeposits{err: rowseal.ErrUnverified}})
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "row_unverified", resp["code"])
	status, _ = serve(&UserHandler{})
	assert.Equal(t, http.StatusServiceUnavailable, status)
}

func TestDepositRoutes_RunWithoutInfrastructure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	serve := func(h *UserHandler, method, body string) (int, map[string]any) {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			auth.SetClaims(c, &auth.Claims{UUID: uuid.NewString(), SessionID: uuid.NewString(), Role: auth.RoleUser})
		})
		r.GET("/deposit-addresses", h.ListDepositAddresses)
		r.POST("/deposit-addresses", h.GetDepositAddress)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, "/deposit-addresses", bytes.NewBufferString(body)))
		var resp map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
		return w.Code, resp
	}
	body := `{"currency_hk": "` + uuid.NewString() + `", "network": "tron"}`
	status, _ := serve(&UserHandler{Deposits: fakeDeposits{}}, http.MethodPost, body)
	assert.Equal(t, http.StatusOK, status)
	status, _ = serve(&UserHandler{Deposits: fakeDeposits{}}, http.MethodPost, `{"network": "tron", "memo": 1}`)
	assert.Equal(t, http.StatusBadRequest, status)
	status, _ = serve(&UserHandler{Deposits: fakeDeposits{}}, http.MethodGet, "")
	assert.Equal(t, http.StatusOK, status)
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{deposits.ErrUnsupported, http.StatusBadRequest, codeDepositNetwork},
		{deposits.ErrNotFound, http.StatusNotFound, codeDepositCurrency},
		{deposits.ErrClosed, http.StatusConflict, codeDepositsClosed},
		{rowseal.ErrUnverified, http.StatusConflict, "row_unverified"},
	} {
		status, resp := serve(&UserHandler{Deposits: fakeDeposits{err: c.err}}, http.MethodPost, body)
		assert.Equal(t, c.status, status, c.err.Error())
		assert.Equal(t, c.code, resp["code"])
	}
	status, resp := serve(&UserHandler{}, http.MethodGet, "")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, codeDepositsUnavailable, resp["code"])
}
