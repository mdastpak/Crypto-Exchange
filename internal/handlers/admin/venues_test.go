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
	"github.com/mdastpak/crypto-exchange/internal/venues"
	"github.com/mdastpak/crypto-exchange/internal/venues/state"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeVenues answers with err, and keeps what it was asked.
type fakeVenues struct {
	err    error
	origin audit.Event
	reason string
}

func (f *fakeVenues) Overview(context.Context, map[string]venues.Mapping) ([]state.VenueState, error) {
	return []state.VenueState{{Venue: "binance"}}, f.err
}

func (f *fakeVenues) ClearHold(_ context.Context, id string, origin audit.Event, reason string) (state.Hold, error) {
	f.origin, f.reason = origin, reason
	return state.Hold{ID: id}, f.err
}

func serveVenues(t *testing.T, h *AdminHandler, adminID, method, path, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { auth.SetClaims(c, &auth.Claims{UUID: adminID, Role: auth.RoleAdmin}) })
	r.GET("/venues", h.ListVenues)
	r.POST("/venues/holds/:id/clear", h.ClearVenueHold)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return w.Code, resp
}

func TestVenueRoutes(t *testing.T) {
	admin, hold := uuid.NewString(), uuid.NewString()
	status, _ := serveVenues(t, &AdminHandler{}, admin, http.MethodGet, "/venues", "")
	assert.Equal(t, http.StatusServiceUnavailable, status, "no store")

	f := &fakeVenues{}
	h := &AdminHandler{Venues: f}
	status, resp := serveVenues(t, h, admin, http.MethodGet, "/venues", "")
	require.Equal(t, http.StatusOK, status, resp)

	status, _ = serveVenues(t, h, admin, http.MethodPost, "/venues/holds/x/clear", `{"reason":"ok"}`)
	assert.Equal(t, http.StatusBadRequest, status, "not a UUID")
	status, _ = serveVenues(t, h, admin, http.MethodPost, "/venues/holds/"+hold+"/clear", `{"reason":"  "}`)
	assert.Equal(t, http.StatusBadRequest, status, "a reason is required")
	status, _ = serveVenues(t, h, admin, http.MethodPost, "/venues/holds/"+hold+"/clear",
		`{"reason":"`+strings.Repeat("x", 1001)+`"}`)
	assert.Equal(t, http.StatusBadRequest, status)

	status, resp = serveVenues(t, h, admin, http.MethodPost, "/venues/holds/"+hold+"/clear", `{"reason":" late credit "}`)
	require.Equal(t, http.StatusOK, status, resp)
	assert.Equal(t, admin, f.origin.ActorID, "the clear is the administrator's")
	assert.Equal(t, "late credit", f.reason)

	for err, want := range map[error]int{
		state.ErrNotFound:       http.StatusNotFound,
		state.ErrAlreadyCleared: http.StatusConflict,
		rowseal.ErrUnverified:   http.StatusConflict,
	} {
		f.err = err
		status, _ = serveVenues(t, h, admin, http.MethodPost, "/venues/holds/"+hold+"/clear", `{"reason":"r"}`)
		assert.Equal(t, want, status, "%v", err)
	}
	f.err = rowseal.ErrUnverified
	status, _ = serveVenues(t, h, admin, http.MethodGet, "/venues", "")
	assert.Equal(t, http.StatusConflict, status)
}
