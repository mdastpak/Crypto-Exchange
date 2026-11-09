package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRouter_SendsVisitorsToTheSiteOfTheirCountry runs the site routes
// through the whole pipeline (#285): GeoIP from the configuration, the
// locale of the site the call comes from, CORS for the site domains, and
// the redirect.
func TestRouter_SendsVisitorsToTheSiteOfTheirCountry(t *testing.T) {
	env := newE2EEnv(t)
	c := &config.Config
	c.RateLimits.REST.RequestsPerSecond, c.RateLimits.REST.Burst = 1000, 1000
	c.GeoIP.Enabled = true
	c.GeoIP.Overrides = map[string]string{"192.0.2.0/24": "IR"}
	c.GeoIP.ReloadIntervalSeconds, c.GeoIP.MaxAgeDays = 300, 45
	c.Sites.Default, c.Sites.Scheme = "global", "https"
	c.Sites.List = []config.SiteConfig{
		{ID: "global", Domain: "example.com", Locales: []string{"en"}},
		{ID: "ir", Domain: "example.ir", Countries: []string{"IR"}, Locales: []string{"fa", "en"}},
	}
	geo, _, err := startGeoIP()
	require.NoError(t, err)
	env.handlers.GeoIP = geo
	env.handlers.Sites = nil
	router := newTestRouter(t, env)

	call := func(method, path, origin, from string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Host = "api.example.net"
		req.RemoteAddr = from + ":5000"
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	w := call(http.MethodGet, "/api/v1/public/site?path=/markets", "https://example.com", "192.0.2.7")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "https://example.com", w.Header().Get("Access-Control-Allow-Origin"), "every site may call the API")
	var body struct {
		Info struct {
			Country  string                     `json:"country"`
			Site     struct{ ID string }        `json:"site"`
			Locale   struct{ Code, Dir string } `json:"locale"`
			Redirect struct {
				Needed bool   `json:"needed"`
				URL    string `json:"url"`
			} `json:"redirect"`
		} `json:"info"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "IR", body.Info.Country)
	assert.Equal(t, "ir", body.Info.Site.ID)
	assert.Equal(t, "fa", body.Info.Locale.Code)
	assert.Equal(t, "rtl", body.Info.Locale.Dir)
	assert.True(t, body.Info.Redirect.Needed)
	assert.Equal(t, "https://example.ir/markets", body.Info.Redirect.URL)

	w = call(http.MethodGet, "/api/v1/public/site/redirect?path=/markets", "https://example.com", "192.0.2.7")
	assert.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, "https://example.ir/markets", w.Header().Get("Location"))

	w = call(http.MethodGet, "/api/v1/public/site/redirect?path=//evil.example", "https://example.com", "192.0.2.7")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertErrorContract(t, w, "invalid path")

	// A call from the Iranian site answers in its language, unasked.
	w = call(http.MethodGet, "/api/v1/no-such-route", "https://example.ir", "203.0.113.1")
	assert.Equal(t, "fa", w.Header().Get("Content-Language"))
	assert.Equal(t, "https://example.ir", w.Header().Get("Access-Control-Allow-Origin"))

	w = call(http.MethodGet, "/api/v1/public/site/redirect", "https://example.ir", "192.0.2.7")
	assert.Equal(t, http.StatusNoContent, w.Code)
}
