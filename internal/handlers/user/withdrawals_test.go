package user

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/confirm"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/withdrawals"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeWithdrawals answers with out, or err, and keeps the requests.
type fakeWithdrawals struct {
	out      withdrawals.Outcome
	err      error
	requests []withdrawals.Request
}

func (f *fakeWithdrawals) Request(_ context.Context, _ string, r withdrawals.Request, _ string, _ audit.Event) (withdrawals.Outcome, error) {
	f.requests = append(f.requests, r)
	return f.out, f.err
}

func (f *fakeWithdrawals) List(context.Context, string, *time.Time, int) ([]withdrawals.Withdrawal, error) {
	return []withdrawals.Withdrawal{{ID: "w1"}}, f.err
}

func (f *fakeWithdrawals) Get(context.Context, string, string) (withdrawals.Withdrawal, error) {
	return withdrawals.Withdrawal{ID: "w1"}, f.err
}

func (f *fakeWithdrawals) Cancel(context.Context, string, string, audit.Event) (withdrawals.Withdrawal, error) {
	return withdrawals.Withdrawal{ID: "w1", Status: withdrawals.StatusCancelled}, f.err
}

func serveWithdrawal(t *testing.T, h *UserHandler, method, path, key, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		auth.SetClaims(c, &auth.Claims{UUID: uuid.NewString(), SessionID: uuid.NewString(), Role: auth.RoleUser})
	})
	r.POST("/withdrawals", h.RequestWithdrawal)
	r.GET("/withdrawals", h.ListWithdrawals)
	r.GET("/withdrawals/:id", h.GetWithdrawal)
	r.POST("/withdrawals/:id/cancel", h.CancelWithdrawal)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return w.Code, resp
}

const withdrawalBody = `{"currency_hk": "11111111-1111-1111-1111-111111111111", "network": "tron",
	"destination_id": "22222222-2222-2222-2222-222222222222", "amount": "100"}`

func TestWithdrawalRoutes_RunWithoutInfrastructure(t *testing.T) {
	service := &fakeWithdrawals{out: withdrawals.Outcome{Challenge: &confirm.Challenge{ID: "c1", Method: confirm.MethodTOTP}}}
	h := &UserHandler{Withdrawals: service}

	status, resp := serveWithdrawal(t, h, http.MethodPost, "/withdrawals", "", withdrawalBody)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, codeIdempotencyKeyRequired, resp["code"])

	status, resp = serveWithdrawal(t, h, http.MethodPost, "/withdrawals", "k1", withdrawalBody)
	require.Equal(t, http.StatusAccepted, status, resp)
	assert.Equal(t, "c1", resp["info"].(map[string]any)["confirmation"].(map[string]any)["id"])
	require.Len(t, service.requests, 1)
	assert.Equal(t, "k1", service.requests[0].IdempotencyKey)
	assert.Equal(t, "100", service.requests[0].Amount.String())

	status, _ = serveWithdrawal(t, h, http.MethodPost, "/withdrawals", "k1", `{"amount": "100", "amont": "1"}`)
	assert.Equal(t, http.StatusBadRequest, status, "an unknown field is refused, never ignored")

	service.out = withdrawals.Outcome{Withdrawal: &withdrawals.Withdrawal{ID: "w1", Status: withdrawals.StatusQueued}}
	status, _ = serveWithdrawal(t, h, http.MethodPost, "/withdrawals", "k1", withdrawalBody)
	assert.Equal(t, http.StatusCreated, status)
	service.out.Replayed = true
	status, _ = serveWithdrawal(t, h, http.MethodPost, "/withdrawals", "k1", withdrawalBody)
	assert.Equal(t, http.StatusOK, status, "a retry")

	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{withdrawals.Invalid{"amount": "is below the minimum of 20"}, http.StatusBadRequest, "validation_failed"},
		{withdrawals.ErrInsufficientBalance, http.StatusConflict, codeInsufficientBalance},
		{withdrawals.ErrClosed, http.StatusConflict, codeWithdrawalsClosed},
		{withdrawals.ErrIdempotencyConflict, http.StatusConflict, codeIdempotencyKeyReused},
		{confirm.ErrNotConfirmed, http.StatusConflict, codeConfirmationNotConfirmed},
		{confirm.ErrMismatch, http.StatusConflict, codeConfirmationOfAnotherRequest},
		{confirm.ErrExpired, http.StatusConflict, CodeConfirmationExpired},
		{&destinations.NotUsableYet{Until: time.Now()}, http.StatusConflict, codeDestinationNotUsableYet},
		{&restrictions.Refusal{Code: restrictions.CodeSecurityHold}, http.StatusForbidden, restrictions.CodeSecurityHold},
		{errors.Join(errors.New("context"), withdrawals.ErrNotCancellable), http.StatusConflict, codeWithdrawalNotCancellable},
	} {
		service.err = tc.err
		status, resp := serveWithdrawal(t, h, http.MethodPost, "/withdrawals", "k1", withdrawalBody)
		assert.Equal(t, tc.status, status, tc.err.Error())
		assert.Equal(t, tc.code, resp["code"], tc.err.Error())
	}

	service.err = withdrawals.ErrNotFound
	status, resp = serveWithdrawal(t, h, http.MethodGet, "/withdrawals/w2", "", "")
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, codeWithdrawalNotFound, resp["code"])

	service.err = nil
	status, _ = serveWithdrawal(t, h, http.MethodGet, "/withdrawals?limit=1000", "", "")
	assert.Equal(t, http.StatusBadRequest, status)
	status, _ = serveWithdrawal(t, h, http.MethodGet, "/withdrawals?before=yesterday", "", "")
	assert.Equal(t, http.StatusBadRequest, status)
	status, resp = serveWithdrawal(t, h, http.MethodGet, "/withdrawals?limit=10&before=2026-11-14T10:00:00Z", "", "")
	require.Equal(t, http.StatusOK, status)
	assert.Len(t, resp["info"], 1)
	status, resp = serveWithdrawal(t, h, http.MethodPost, "/withdrawals/w1/cancel", "", "")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "cancelled", resp["info"].(map[string]any)["status"])

	status, resp = serveWithdrawal(t, &UserHandler{}, http.MethodGet, "/withdrawals", "", "")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, codeWithdrawalsUnavailable, resp["code"])
}
