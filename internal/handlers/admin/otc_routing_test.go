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
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/otc/routing"
	"github.com/mdastpak/crypto-exchange/internal/region"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRouting keeps the rules saved and the trade explained.
type fakeRouting struct {
	err       error
	saved     []routing.Rule
	reason    string
	origin    audit.Event
	explained routing.Input
}

func (f *fakeRouting) Latest(context.Context) (routing.Version, error) {
	return routing.Version{RuleSet: routing.RuleSet{Version: 4, Rules: []routing.Rule{{Name: "own", UseInventory: true}}}}, f.err
}

func (f *fakeRouting) Save(_ context.Context, rules []routing.Rule, reason string, origin audit.Event) (routing.Version, error) {
	if err := routing.Validate(rules, nil); err != nil {
		return routing.Version{}, err
	}
	f.saved, f.reason, f.origin = rules, reason, origin
	return routing.Version{RuleSet: routing.RuleSet{Version: 5, Rules: rules}, Reason: reason}, f.err
}

func (f *fakeRouting) Explain(_ context.Context, in routing.Input) (routing.Explanation, error) {
	f.explained = in
	return routing.Explanation{RuleVersion: 4, Reason: routing.ReasonNoMatchingRule}, f.err
}

func serveRouting(t *testing.T, h *AdminHandler, adminID, method, path, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { auth.SetClaims(c, &auth.Claims{UUID: adminID, Role: auth.RoleAdmin}) })
	r.GET("/otc/routing/rules", h.GetRoutingRules)
	r.PUT("/otc/routing/rules", h.SaveRoutingRules)
	r.POST("/otc/routing/explain", h.ExplainRoute)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return w.Code, resp
}

func TestRoutingRoutes(t *testing.T) {
	admin := uuid.NewString()
	status, _ := serveRouting(t, &AdminHandler{}, admin, http.MethodGet, "/otc/routing/rules", "")
	assert.Equal(t, http.StatusServiceUnavailable, status, "no store")

	f := &fakeRouting{}
	c := &config.Configuration{}
	c.OTC.Routing = config.OTCRoutingConfig{Enabled: true, CacheSeconds: 5}
	h := &AdminHandler{Config: c, RoutingRules: f, RouteExplainer: f, Registry: &fakeRegistry{}}
	status, resp := serveRouting(t, h, admin, http.MethodGet, "/otc/routing/rules", "")
	assert.Equal(t, http.StatusOK, status)
	info := resp["info"].(map[string]any)
	assert.Equal(t, true, info["enabled"])
	assert.EqualValues(t, 4, info["version"].(map[string]any)["version"])

	// A save needs a reason, and rules that validate, each problem by its
	// field.
	status, _ = serveRouting(t, h, admin, http.MethodPut, "/otc/routing/rules", `{"rules":[],"reason":"  "}`)
	assert.Equal(t, http.StatusBadRequest, status)
	status, resp = serveRouting(t, h, admin, http.MethodPut, "/otc/routing/rules",
		`{"rules":[{"name":"a","venues":[{"venue":"kraken"}]}],"reason":"try"}`)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, resp["info"].(map[string]any)["errors"], "rules[0].venues[0].venue")
	status, _ = serveRouting(t, h, admin, http.MethodPut, "/otc/routing/rules",
		`{"rules":[{"name":"binance","priority":1,"venues":[{"venue":"binance","weight":2}]}],"reason":" cover on binance "}`)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "cover on binance", f.reason)
	assert.Equal(t, admin, f.origin.ActorID)
	assert.Equal(t, 2, f.saved[0].Venues[0].Weight)

	// The explanation of a trade.
	status, _ = serveRouting(t, h, admin, http.MethodPost, "/otc/routing/explain", `{"market":"BTC","side":"hold","quantity":"0","price":"x"}`)
	assert.Equal(t, http.StatusBadRequest, status)
	status, resp = serveRouting(t, h, admin, http.MethodPost, "/otc/routing/explain",
		`{"market":"btc/usdt","side":"buy","quantity":"0.5","price":"65000","level":2,"network":"bitcoin","region":"international"}`)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, routing.ReasonNoMatchingRule, resp["info"].(map[string]any)["reason"])
	assert.Equal(t, "BTC/USDT", f.explained.Market)
	assert.Equal(t, region.International, f.explained.Region)
	assert.Equal(t, "bitcoin", f.explained.Network)
	assert.Equal(t, 2, f.explained.Level)

	h.Registry = &fakeRegistry{err: currencies.ErrMarketNotFound}
	status, _ = serveRouting(t, h, admin, http.MethodPost, "/otc/routing/explain", `{"market":"BTC/USDT","side":"buy","quantity":"1","price":"1"}`)
	assert.Equal(t, http.StatusNotFound, status)

	f.err = rowseal.ErrUnverified
	status, resp = serveRouting(t, h, admin, http.MethodGet, "/otc/routing/rules", "")
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, codeRowUnverified, resp["code"])
}
