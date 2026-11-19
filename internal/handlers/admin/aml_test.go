package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/aml"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/sanctions"
)

// fakeAML answers with err, and keeps what it was asked.
type fakeAML struct {
	err        error
	status     aml.Status
	user       string
	limit      int
	to         aml.Status
	admin      string
	resolution string
}

func (f *fakeAML) Rules(context.Context) ([]aml.Stored, error) {
	return []aml.Stored{{ID: "r1", Version: 2, Rule: aml.Rule{Code: "structuring_withdrawal"}}}, f.err
}

func (f *fakeAML) Alerts(_ context.Context, status aml.Status, user string, limit int) ([]aml.Alert, error) {
	f.status, f.user, f.limit = status, user, limit
	return []aml.Alert{{ID: "a1", Status: aml.StatusOpen}}, f.err
}

func (f *fakeAML) Alert(context.Context, string) (aml.Alert, error) {
	return aml.Alert{ID: "a1", Status: aml.StatusOpen}, f.err
}

func (f *fakeAML) Transition(_ context.Context, _ string, to aml.Status, adminHK, resolution string, _ audit.Event) (aml.Alert, error) {
	f.to, f.admin, f.resolution = to, adminHK, resolution
	return aml.Alert{ID: "a1", Status: to}, f.err
}

// fakeSanctions answers with the state of a list.
type fakeSanctions struct{ err error }

func (f fakeSanctions) Status(context.Context) (sanctions.Status, error) {
	return sanctions.Status{Version: sanctions.Version{ID: 3, Provider: "ofac", EntryCount: 18000}}, f.err
}

func serveAML(t *testing.T, h *AdminHandler, adminID, method, path, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { auth.SetClaims(c, &auth.Claims{UUID: adminID, Role: auth.RoleAdmin}) })
	r.GET("/aml/rules", h.ListAMLRules)
	r.GET("/aml/alerts", h.ListAMLAlerts)
	r.GET("/aml/alerts/:id", h.GetAMLAlert)
	r.POST("/aml/alerts/:id/transition", h.TransitionAMLAlert)
	r.GET("/aml/sanctions", h.GetSanctionsStatus)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return w.Code, resp
}

func TestAMLRoutes_RunWithoutInfrastructure(t *testing.T) {
	desk := &fakeAML{}
	h := &AdminHandler{AML: desk}
	admin := uuid.NewString()

	status, resp := serveAML(t, h, admin, http.MethodGet, "/aml/rules", "")
	require.Equal(t, http.StatusOK, status, resp)

	user := uuid.NewString()
	status, _ = serveAML(t, h, admin, http.MethodGet, "/aml/alerts?status=ESCALATED&user_hk="+user+"&limit=5", "")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, aml.StatusEscalated, desk.status)
	assert.Equal(t, user, desk.user)
	assert.Equal(t, 5, desk.limit)
	status, resp = serveAML(t, h, admin, http.MethodGet, "/aml/alerts?status=CLOSED&user_hk=x&limit=500", "")
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, resp["info"].(map[string]any)["errors"], "status")

	status, _ = serveAML(t, h, admin, http.MethodGet, "/aml/alerts/a1", "")
	assert.Equal(t, http.StatusOK, status)

	status, _ = serveAML(t, h, admin, http.MethodPost, "/aml/alerts/a1/transition", `{"status": "OPEN"}`)
	assert.Equal(t, http.StatusBadRequest, status, "an alert is not reopened")
	status, _ = serveAML(t, h, admin, http.MethodPost, "/aml/alerts/a1/transition", `{"status": "RESOLVED", "note": "x"}`)
	assert.Equal(t, http.StatusBadRequest, status, "an unknown field")
	status, resp = serveAML(t, h, admin, http.MethodPost, "/aml/alerts/a1/transition", `{"status": "RESOLVED", "resolution": "Salary."}`)
	require.Equal(t, http.StatusOK, status, resp)
	assert.Equal(t, aml.StatusResolved, desk.to)
	assert.Equal(t, admin, desk.admin, "the administrator of the token decides")
	assert.Equal(t, "Salary.", desk.resolution)

	for err, code := range map[error]string{
		aml.ErrNotFound:       codeAMLAlertNotFound,
		&aml.ErrTransition{}:  codeAMLAlertTransitionInvalid,
		rowseal.ErrUnverified: codeRowUnverified,
		aml.ErrResolution:     "validation_failed",
	} {
		desk.err = err
		_, resp := serveAML(t, h, admin, http.MethodPost, "/aml/alerts/a1/transition", `{"status": "ESCALATED"}`)
		assert.Equal(t, code, resp["code"], err.Error())
	}

	status, resp = serveAML(t, &AdminHandler{}, admin, http.MethodGet, "/aml/alerts", "")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, codeAMLUnavailable, resp["code"])

	status, resp = serveAML(t, &AdminHandler{Sanctions: fakeSanctions{}}, admin, http.MethodGet, "/aml/sanctions", "")
	require.Equal(t, http.StatusOK, status, resp)
	assert.Equal(t, float64(3), resp["info"].(map[string]any)["version"])
	_, resp = serveAML(t, &AdminHandler{Sanctions: fakeSanctions{err: rowseal.ErrUnverified}}, admin, http.MethodGet, "/aml/sanctions", "")
	assert.Equal(t, codeRowUnverified, resp["code"])
	status, _ = serveAML(t, &AdminHandler{}, admin, http.MethodGet, "/aml/sanctions", "")
	assert.Equal(t, http.StatusServiceUnavailable, status, "the provider is off")
}
