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

// fakeDesk answers with err, and keeps what it was asked.
type fakeDesk struct {
	err      error
	status   withdrawals.PayoutStatus
	tracking string
	reason   string
	viewer   string
}

func (f *fakeDesk) List(_ context.Context, status withdrawals.PayoutStatus, _ int) ([]withdrawals.Payout, error) {
	f.status = status
	return []withdrawals.Payout{{ID: "p1"}}, f.err
}

func (f *fakeDesk) Detail(_ context.Context, _ string, origin audit.Event) (withdrawals.PayoutDetail, error) {
	f.viewer = origin.ActorID
	return withdrawals.PayoutDetail{Payout: withdrawals.Payout{ID: "p1"}, DestinationValue: "IR820540102680020817909002"}, f.err
}

func (f *fakeDesk) Complete(_ context.Context, _, trackingID string, _ audit.Event) (withdrawals.Payout, error) {
	f.tracking = trackingID
	return withdrawals.Payout{ID: "p1", Status: withdrawals.PayoutCompleted}, f.err
}

func (f *fakeDesk) Fail(_ context.Context, _, reason string, _ audit.Event) (withdrawals.Payout, error) {
	f.reason = reason
	return withdrawals.Payout{ID: "p1", Status: withdrawals.PayoutReleased}, f.err
}

func servePayouts(t *testing.T, h *AdminHandler, adminID, method, path, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { auth.SetClaims(c, &auth.Claims{UUID: adminID, Role: auth.RoleAdmin}) })
	r.GET("/payouts", h.ListPayouts)
	r.GET("/payouts/:id", h.GetPayout)
	r.POST("/payouts/:id/complete", h.CompletePayout)
	r.POST("/payouts/:id/fail", h.FailPayout)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return w.Code, resp
}

func TestPayoutRoutes_RunWithoutInfrastructure(t *testing.T) {
	desk := &fakeDesk{}
	h := &AdminHandler{Payouts: desk}
	admin := uuid.NewString()

	status, _ := servePayouts(t, h, admin, http.MethodGet, "/payouts", "")
	assert.Equal(t, http.StatusOK, status)
	assert.Empty(t, desk.status, "submitted by default")
	status, _ = servePayouts(t, h, admin, http.MethodGet, "/payouts?status=lost", "")
	assert.Equal(t, http.StatusBadRequest, status)

	status, resp := servePayouts(t, h, admin, http.MethodGet, "/payouts/p1", "")
	require.Equal(t, http.StatusOK, status, resp)
	assert.Equal(t, admin, desk.viewer, "the reading is the administrator's")
	assert.Equal(t, "IR820540102680020817909002", resp["info"].(map[string]any)["destination_value"])

	status, _ = servePayouts(t, h, admin, http.MethodPost, "/payouts/p1/complete", `{"tracking_id": "140311150012345678"}`)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "140311150012345678", desk.tracking)
	status, _ = servePayouts(t, h, admin, http.MethodPost, "/payouts/p1/complete", `{"tracking": "x"}`)
	assert.Equal(t, http.StatusBadRequest, status, "an unknown field")
	status, _ = servePayouts(t, h, admin, http.MethodPost, "/payouts/p1/fail", `{"reason": "Refused."}`)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "Refused.", desk.reason)

	for err, code := range map[error]string{
		withdrawals.ErrPayoutNotFound:  codePayoutNotFound,
		withdrawals.ErrPayoutNotOpen:   codePayoutNotOpen,
		withdrawals.ErrPayoutAutomatic: codePayoutAutomatic,
		rowseal.ErrUnverified:          codeRowUnverified,
	} {
		desk.err = err
		_, resp := servePayouts(t, h, admin, http.MethodPost, "/payouts/p1/complete", `{"tracking_id": "x"}`)
		assert.Equal(t, code, resp["code"], err.Error())
	}

	status, resp = servePayouts(t, &AdminHandler{}, admin, http.MethodGet, "/payouts", "")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, codeWithdrawalsUnavailable, resp["code"])
}
