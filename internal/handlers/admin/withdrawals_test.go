package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/withdrawals"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeReviews answers with err, and keeps what it was asked.
type fakeReviews struct {
	err      error
	status   withdrawals.Status
	reviewer string
	reason   string
}

func (f *fakeReviews) Review(_ context.Context, status withdrawals.Status, _ int) ([]withdrawals.Withdrawal, error) {
	f.status = status
	return []withdrawals.Withdrawal{{ID: "w1", Status: withdrawals.StatusReview}}, f.err
}

func (f *fakeReviews) Approve(_ context.Context, _, adminHK string, _ audit.Event) (withdrawals.Withdrawal, error) {
	f.reviewer = adminHK
	return withdrawals.Withdrawal{ID: "w1", Status: withdrawals.StatusQueued}, f.err
}

func (f *fakeReviews) Reject(_ context.Context, _, adminHK, reason string, _ audit.Event) (withdrawals.Withdrawal, error) {
	f.reviewer, f.reason = adminHK, reason
	return withdrawals.Withdrawal{ID: "w1", Status: withdrawals.StatusRejected}, f.err
}

func serveReview(t *testing.T, h *AdminHandler, adminID, method, path, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { auth.SetClaims(c, &auth.Claims{UUID: adminID, Role: auth.RoleAdmin}) })
	r.GET("/withdrawals", h.ListWithdrawals)
	r.POST("/withdrawals/:id/approve", h.ApproveWithdrawal)
	r.POST("/withdrawals/:id/reject", h.RejectWithdrawal)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return w.Code, resp
}

func TestWithdrawalReviewRoutes_RunWithoutInfrastructure(t *testing.T) {
	reviews := &fakeReviews{}
	h := &AdminHandler{Withdrawals: reviews}
	admin := uuid.NewString()

	status, resp := serveReview(t, h, admin, http.MethodGet, "/withdrawals", "")
	require.Equal(t, http.StatusOK, status, resp)
	assert.Empty(t, reviews.status, "review by default")
	status, _ = serveReview(t, h, admin, http.MethodGet, "/withdrawals?status=paid", "")
	assert.Equal(t, http.StatusBadRequest, status)
	status, _ = serveReview(t, h, admin, http.MethodGet, "/withdrawals?status=queued&limit=5", "")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, withdrawals.StatusQueued, reviews.status)

	status, resp = serveReview(t, h, admin, http.MethodPost, "/withdrawals/w1/approve", "")
	require.Equal(t, http.StatusOK, status, resp)
	assert.Equal(t, admin, reviews.reviewer, "the administrator of the token decides")
	status, _ = serveReview(t, h, admin, http.MethodPost, "/withdrawals/w1/reject", `{"reason": "Fraud.", "note": "x"}`)
	assert.Equal(t, http.StatusBadRequest, status, "an unknown field")
	status, _ = serveReview(t, h, admin, http.MethodPost, "/withdrawals/w1/reject", `{"reason": "Fraud."}`)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "Fraud.", reviews.reason)

	for err, code := range map[error]string{
		withdrawals.ErrNotInReview: codeWithdrawalNotInReview,
		withdrawals.ErrNotFound:    codeWithdrawalNotFound,
		rowseal.ErrUnverified:      codeRowUnverified,
	} {
		reviews.err = err
		_, resp := serveReview(t, h, admin, http.MethodPost, "/withdrawals/w1/approve", "")
		assert.Equal(t, code, resp["code"], err.Error())
	}
	reviews.err = withdrawals.Invalid{"reason": "is required"}
	status, _ = serveReview(t, h, admin, http.MethodPost, "/withdrawals/w1/reject", `{}`)
	assert.Equal(t, http.StatusBadRequest, status)

	status, resp = serveReview(t, &AdminHandler{}, admin, http.MethodGet, "/withdrawals", "")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, codeWithdrawalsUnavailable, resp["code"])
}
