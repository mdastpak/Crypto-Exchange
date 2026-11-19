package admin

import (
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

type fakeDepositReviews struct {
	err    error
	filter deposits.Filter
	origin audit.Event
}

func (f *fakeDepositReviews) List(_ context.Context, filter deposits.Filter) ([]deposits.Deposit, error) {
	f.filter = filter
	return []deposits.Deposit{}, f.err
}

func (f *fakeDepositReviews) Credit(_ context.Context, id string, origin audit.Event) (deposits.Deposit, error) {
	f.origin = origin
	return deposits.Deposit{ID: id, Status: deposits.StatusCredited}, f.err
}

func TestDepositReviewRoutes_RunWithoutInfrastructure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	admin := uuid.NewString()
	serve := func(h *AdminHandler, method, path string) (int, map[string]any) {
		r := gin.New()
		r.Use(func(c *gin.Context) { auth.SetClaims(c, &auth.Claims{UUID: admin, Role: auth.RoleAdmin}) })
		r.GET("/deposits", h.ListDeposits)
		r.POST("/deposits/:id/credit", h.CreditDeposit)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		var resp map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
		return w.Code, resp
	}
	reviews := &fakeDepositReviews{}
	h := &AdminHandler{Deposits: reviews}
	user := uuid.NewString()
	status, _ := serve(h, http.MethodGet, "/deposits?held=true&status=CONFIRMED&user_id="+user)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, deposits.Filter{UserHK: user, Status: deposits.StatusConfirmed, Held: true}, reviews.filter)
	for _, bad := range []string{"?status=PAID", "?held=yes", "?user_id=bob"} {
		status, _ = serve(h, http.MethodGet, "/deposits"+bad)
		assert.Equal(t, http.StatusBadRequest, status, bad)
	}
	id := uuid.NewString()
	status, _ = serve(h, http.MethodPost, "/deposits/"+id+"/credit")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, admin, reviews.origin.ActorID, "the administrator of the token credits")
	status, _ = serve(h, http.MethodPost, "/deposits/x/credit")
	assert.Equal(t, http.StatusNotFound, status)
	for err, want := range map[error]int{deposits.ErrNotHeld: http.StatusConflict, deposits.ErrDepositNotFound: http.StatusNotFound,
		rowseal.ErrUnverified: http.StatusConflict} {
		status, _ = serve(&AdminHandler{Deposits: &fakeDepositReviews{err: err}}, http.MethodPost, "/deposits/"+id+"/credit")
		assert.Equal(t, want, status, err.Error())
	}
	status, resp := serve(&AdminHandler{}, http.MethodGet, "/deposits")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, codeDepositsUnavailable, resp["code"])
}
