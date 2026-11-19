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
	"github.com/mdastpak/crypto-exchange/internal/otc/routed"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRoutedOrders keeps the filter of a list and the resolution asked.
type fakeRoutedOrders struct {
	err    error
	filter routed.Filter
	id     string
	action string
	note   string
	origin audit.Event
}

func (f *fakeRoutedOrders) List(_ context.Context, filter routed.Filter) ([]routed.Item, error) {
	f.filter = filter
	return []routed.Item{{ID: "o1", Status: routed.StatusManualReview, Sealed: true, Events: []routed.EventItem{}}}, f.err
}

func (f *fakeRoutedOrders) Resolve(_ context.Context, id, action, note string, origin audit.Event) (routed.Resolution, error) {
	f.id, f.action, f.note, f.origin = id, action, note, origin
	return routed.Resolution{Order: routed.Item{ID: id, Status: routed.StatusResolved}, Action: action, Note: note}, f.err
}

func serveRouted(t *testing.T, h *AdminHandler, adminID, method, path, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { auth.SetClaims(c, &auth.Claims{UUID: adminID, Role: auth.RoleAdmin}) })
	r.GET("/otc/routed-orders", h.ListRoutedOrders)
	r.POST("/otc/routed-orders/:id/resolve", h.ResolveRoutedOrder)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return w.Code, resp
}

func TestRoutedOrderRoutes(t *testing.T) {
	admin := uuid.NewString()
	status, _ := serveRouted(t, &AdminHandler{}, admin, http.MethodGet, "/otc/routed-orders", "")
	assert.Equal(t, http.StatusServiceUnavailable, status)

	f := &fakeRoutedOrders{}
	h := &AdminHandler{RoutedOrders: f}
	trade := uuid.NewString()
	status, resp := serveRouted(t, h, admin, http.MethodGet, "/otc/routed-orders?status=manual_review&venue=binance&trade_id="+trade+"&limit=20", "")
	require.Equal(t, http.StatusOK, status, resp)
	assert.Equal(t, routed.Filter{Status: "manual_review", Venue: "binance", TradeID: trade, Limit: 20}, f.filter)
	for _, q := range []string{"status=open", "venue=Bad-Name", "trade_id=x", "limit=0", "limit=201"} {
		status, _ = serveRouted(t, h, admin, http.MethodGet, "/otc/routed-orders?"+q, "")
		assert.Equal(t, http.StatusBadRequest, status, q)
	}

	id := uuid.NewString()
	path := "/otc/routed-orders/" + id + "/resolve"
	status, _ = serveRouted(t, h, admin, http.MethodPost, "/otc/routed-orders/x/resolve", `{"action":"handled","note":"n"}`)
	assert.Equal(t, http.StatusBadRequest, status)
	status, _ = serveRouted(t, h, admin, http.MethodPost, path, `{"action":"cancel","note":"n"}`)
	assert.Equal(t, http.StatusBadRequest, status)
	status, _ = serveRouted(t, h, admin, http.MethodPost, path, `{"action":"handled","note":"   "}`)
	assert.Equal(t, http.StatusBadRequest, status)
	status, resp = serveRouted(t, h, admin, http.MethodPost, path, `{"action":"handled","note":" hedged by hand "}`)
	require.Equal(t, http.StatusOK, status, resp)
	assert.Equal(t, id, f.id)
	assert.Equal(t, "hedged by hand", f.note)
	assert.Equal(t, admin, f.origin.ActorID)

	for err, want := range map[error][2]any{
		routed.ErrNotFound:          {http.StatusNotFound, nil},
		routed.ErrNotInReview:       {http.StatusConflict, "not_in_review"},
		routed.ErrNotRetryable:      {http.StatusConflict, "not_retryable"},
		routed.ErrNoRetryRoute:      {http.StatusConflict, "no_route"},
		routed.ErrRetriesExhausted:  {http.StatusConflict, "attempts_exhausted"},
		routed.ErrRetryUnavailable:  {http.StatusServiceUnavailable, "routing_unavailable"},
		rowseal.ErrUnverified:       {http.StatusConflict, codeRowUnverified},
		&platform.Locked{Kind: "x"}: {http.StatusServiceUnavailable, nil},
		assert.AnError:              {http.StatusInternalServerError, nil},
	} {
		f.err = err
		status, resp = serveRouted(t, h, admin, http.MethodPost, path, `{"action":"retry","note":"n"}`)
		assert.Equal(t, want[0], status, err.Error())
		if want[1] != nil {
			assert.Equal(t, want[1], resp["code"], err.Error())
		}
	}
}
