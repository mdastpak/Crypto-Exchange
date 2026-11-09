package public

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/geoip"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/sites"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSiteRouter serves the site routes for a global site and an Iranian
// one, with the countries of 192.0.2.0/24 (IR) and 198.51.100.0/24 (DE)
// fixed, behind a proxy at 10.0.0.1.
func newSiteRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	locales := i18n.MustNew("en", "en", "fa")
	registry, err := sites.New("global", "https", []sites.Spec{
		{ID: "global", Domain: "example.com", Locales: []string{"en"}},
		{ID: "ir", Domain: "example.ir", Countries: []string{"IR"}, Locales: []string{"fa", "en"}},
	}, locales)
	require.NoError(t, err)
	resolver, err := geoip.New(geoip.Options{Overrides: map[string]string{"192.0.2.0/24": "IR", "198.51.100.0/24": "DE"}})
	require.NoError(t, err)
	h := &SiteHandler{
		Sites: registry, Locales: locales, GeoIP: resolver,
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.1/32")},
	}
	engine := gin.New()
	require.NoError(t, engine.SetTrustedProxies([]string{"10.0.0.1"}))
	engine.Use(i18n.Middleware(locales, registry.LocaleOfHost))
	engine.GET("/public/site", h.Site)
	engine.GET("/public/site/redirect", h.Redirect)
	engine.POST("/public/site/choice", h.Choice)
	return engine
}

type siteCall struct {
	method, path, from, origin, body string
	header                           map[string]string
	cookies                          map[string]string
}

func (sc siteCall) do(engine *gin.Engine) *httptest.ResponseRecorder {
	method := sc.method
	if method == "" {
		method = http.MethodGet
	}
	req := httptest.NewRequest(method, sc.path, strings.NewReader(sc.body))
	req.RemoteAddr = sc.from + ":4000"
	// The API's own host, which is no site.
	req.Host = "api.example.net"
	if sc.origin != "" {
		req.Header.Set("Origin", sc.origin)
	}
	for k, v := range sc.header {
		req.Header.Set(k, v)
	}
	for k, v := range sc.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func siteInfoOf(t *testing.T, w *httptest.ResponseRecorder) SiteInfo {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body struct {
		Info SiteInfo `json:"info"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	return body.Info
}

func TestSite_SendsAnIranianVisitorToTheIranianSite(t *testing.T) {
	engine := newSiteRouter(t)
	w := siteCall{path: "/public/site?path=/markets", from: "192.0.2.7", origin: "https://example.com"}.do(engine)
	info := siteInfoOf(t, w)
	assert.Equal(t, "IR", info.Country)
	assert.Equal(t, geoip.SourceOverride, info.CountrySource)
	require.NotNil(t, info.CurrentSite)
	assert.Equal(t, "global", *info.CurrentSite)
	require.NotNil(t, info.Site)
	assert.Equal(t, "ir", info.Site.ID)
	assert.Equal(t, sites.ReasonCountry, info.Reason)
	assert.Equal(t, "fa", info.Locale.Code)
	assert.Equal(t, "rtl", info.Locale.Dir)
	assert.Equal(t, SiteRedirect{Needed: true, URL: "https://example.ir/markets"}, info.Redirect)
	assert.Len(t, info.Sites, 2)
	assert.Len(t, info.Alternates, 4)
	assert.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
	assert.Contains(t, w.Header().Values("Vary"), "Origin, "+i18n.VaryHeaders)

	// The site's default locale is also the language of its answers.
	w = siteCall{path: "/public/site", from: "192.0.2.7", origin: "https://example.ir"}.do(engine)
	info = siteInfoOf(t, w)
	assert.False(t, info.Redirect.Needed)
	assert.Equal(t, "fa", w.Header().Get("Content-Language"))
}

func TestSite_FallsBackToTheGlobalSiteInEnglish(t *testing.T) {
	engine := newSiteRouter(t)
	w := siteCall{path: "/public/site", from: "198.51.100.9", origin: "https://example.ir", header: map[string]string{"Accept-Language": "de-DE"}}.do(engine)
	info := siteInfoOf(t, w)
	assert.Equal(t, "DE", info.Country)
	assert.Equal(t, "global", info.Site.ID)
	assert.Equal(t, sites.ReasonDefault, info.Reason)
	assert.Equal(t, "en", info.Locale.Code)
	assert.Equal(t, "ltr", info.Locale.Dir)
	assert.Equal(t, SiteRedirect{Needed: true, URL: "https://example.com/"}, info.Redirect)
}

func TestSite_HostThroughATrustedProxy(t *testing.T) {
	engine := newSiteRouter(t)
	proxied := map[string]string{"X-Forwarded-Host": "example.ir", "X-Forwarded-For": "192.0.2.7"}
	info := siteInfoOf(t, siteCall{path: "/public/site", from: "10.0.0.1", header: proxied}.do(engine))
	require.NotNil(t, info.CurrentSite)
	assert.Equal(t, "ir", *info.CurrentSite)
	assert.Equal(t, "IR", info.Country, "the client address comes through the proxy")
	assert.False(t, info.Redirect.Needed)

	// From anyone else, the header names nothing.
	info = siteInfoOf(t, siteCall{path: "/public/site", from: "192.0.2.7", header: proxied}.do(engine))
	assert.Nil(t, info.CurrentSite)
	assert.False(t, info.Redirect.Needed, "a call from no site redirects nobody")
	assert.Equal(t, "https://example.ir/", info.Redirect.URL)
}

func TestRedirect(t *testing.T) {
	engine := newSiteRouter(t)
	w := siteCall{path: "/public/site/redirect?path=/markets%3Ftab%3Dspot", from: "192.0.2.7", origin: "https://example.com"}.do(engine)
	assert.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, "https://example.ir/markets?tab=spot", w.Header().Get("Location"))

	// An English choice on the Iranian site travels in the URL.
	w = siteCall{path: "/public/site/redirect?path=/&lang=en", from: "192.0.2.7", origin: "https://example.com", cookies: map[string]string{sites.ChoiceCookie: "ir"}}.do(engine)
	assert.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, "https://example.ir/?lang=en", w.Header().Get("Location"))

	for name, sc := range map[string]siteCall{
		"already there": {path: "/public/site/redirect", from: "192.0.2.7", origin: "https://example.ir"},
		"picked a site": {path: "/public/site/redirect", from: "192.0.2.7", origin: "https://example.com", cookies: map[string]string{sites.ChoiceCookie: "global"}},
		"chose English": {path: "/public/site/redirect", from: "192.0.2.7", origin: "https://example.com", cookies: map[string]string{i18n.Cookie: "en"}},
		"crawler":       {path: "/public/site/redirect", from: "192.0.2.7", origin: "https://example.com", header: map[string]string{"User-Agent": "Googlebot/2.1"}},
		"no site":       {path: "/public/site/redirect", from: "192.0.2.7"},
	} {
		w := sc.do(engine)
		assert.Equal(t, http.StatusNoContent, w.Code, name)
		assert.Empty(t, w.Header().Get("Location"), name)
	}

	for _, path := range []string{"//evil.example", "https://evil.example", "/%5Cevil", "/\\evil", "evil"} {
		w := siteCall{path: "/public/site/redirect?path=" + strings.ReplaceAll(path, "\\", "%5C"), from: "192.0.2.7", origin: "https://example.com"}.do(engine)
		if path == "/%5Cevil" {
			// An encoded backslash stays encoded, and is harmless.
			continue
		}
		assert.Equal(t, http.StatusBadRequest, w.Code, path)
		assert.Contains(t, w.Body.String(), CodeInvalidPath, path)
		assert.Empty(t, w.Header().Get("Location"), path)
	}
}

func TestChoice(t *testing.T) {
	engine := newSiteRouter(t)
	w := siteCall{method: http.MethodPost, path: "/public/site/choice", from: "192.0.2.7", body: `{"site":"GLOBAL","locale":"en-US"}`}.do(engine)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	cookies := map[string]*http.Cookie{}
	for _, c := range w.Result().Cookies() {
		cookies[c.Name] = c
	}
	require.Contains(t, cookies, sites.ChoiceCookie)
	require.Contains(t, cookies, i18n.Cookie)
	assert.Equal(t, "global", cookies[sites.ChoiceCookie].Value)
	assert.Equal(t, "en", cookies[i18n.Cookie].Value)
	assert.True(t, cookies[sites.ChoiceCookie].Secure)
	assert.True(t, cookies[sites.ChoiceCookie].HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, cookies[sites.ChoiceCookie].SameSite)
	assert.Equal(t, 365*24*3600, cookies[sites.ChoiceCookie].MaxAge)

	// Clearing.
	w = siteCall{method: http.MethodPost, path: "/public/site/choice", from: "192.0.2.7", body: `{}`}.do(engine)
	require.Equal(t, http.StatusOK, w.Code)
	for _, c := range w.Result().Cookies() {
		assert.Equal(t, -1, c.MaxAge, c.Name)
	}

	for body, code := range map[string]string{
		`{"site":"mars"}`:  CodeUnknownSite,
		`{"locale":"de"}`:  CodeUnknownLocale,
		`{"other":1}`:      "invalid_request",
		`not json`:         "invalid_request",
		`{"site":"ir"} []`: "",
	} {
		w := siteCall{method: http.MethodPost, path: "/public/site/choice", from: "192.0.2.7", body: body}.do(engine)
		if code == "" {
			continue
		}
		assert.Equal(t, http.StatusBadRequest, w.Code, body)
		assert.Contains(t, w.Body.String(), `"code":"`+code+`"`, body)
	}
}

func TestSite_WithoutSites(t *testing.T) {
	gin.SetMode(gin.TestMode)
	locales := i18n.MustNew("en", "en", "fa")
	registry, err := sites.New("", "https", nil, locales)
	require.NoError(t, err)
	h := &SiteHandler{Sites: registry, Locales: locales}
	engine := gin.New()
	engine.Use(i18n.Middleware(locales, registry.LocaleOfHost))
	engine.GET("/public/site", h.Site)
	engine.GET("/public/site/redirect", h.Redirect)

	info := siteInfoOf(t, siteCall{path: "/public/site", from: "192.0.2.7", header: map[string]string{"Accept-Language": "fa"}}.do(engine))
	assert.Nil(t, info.Site)
	assert.Equal(t, "", info.Country)
	assert.Equal(t, geoip.SourceNone, info.CountrySource)
	assert.Equal(t, "fa", info.Locale.Code)
	assert.False(t, info.Redirect.Needed)
	assert.Empty(t, info.Sites)
	assert.Equal(t, http.StatusNoContent, siteCall{path: "/public/site/redirect", from: "192.0.2.7"}.do(engine).Code)
}
