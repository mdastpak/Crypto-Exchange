package sites

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/geoip"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testLocales = i18n.MustNew("en", "en", "fa", "ar", "tr")

func testSpecs() []Spec {
	return []Spec{
		{ID: "global", Domain: "Example.com", Aliases: []string{"www.example.com"}, Locales: []string{"en", "ar"}},
		{ID: "ir", Domain: "example.ir", Aliases: []string{"www.example.ir"}, Countries: []string{"ir"}, Locales: []string{"fa", "en"}},
		{ID: "tr", Domain: "example.com.tr", Countries: []string{"TR", "CY"}, Locales: []string{"tr", "en"}, Enforce: true},
	}
}

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := New("global", "https", testSpecs(), testLocales)
	require.NoError(t, err)
	return r
}

func TestNew_RefusesInvalidTables(t *testing.T) {
	change := func(f func(s []Spec) []Spec) []Spec { return f(testSpecs()) }
	for name, tc := range map[string]struct {
		def    string
		scheme string
		specs  []Spec
		want   string
	}{
		"default missing":     {"world", "https", testSpecs(), `the default site "world" is not one of the sites (global, ir, tr)`},
		"bad scheme":          {"global", "ftp", testSpecs(), `scheme "ftp"`},
		"bad id":              {"global", "https", change(func(s []Spec) []Spec { s[0].ID = "Global Site"; return s }), "site id"},
		"duplicate id":        {"global", "https", change(func(s []Spec) []Spec { s[1].ID = "global"; return s }), `site "global" is listed twice`},
		"domain with scheme":  {"global", "https", change(func(s []Spec) []Spec { s[0].Domain = "https://example.com"; return s }), "without a scheme"},
		"domain with path":    {"global", "https", change(func(s []Spec) []Spec { s[0].Domain = "example.com/fa"; return s }), "without a scheme or a path"},
		"empty domain":        {"global", "https", change(func(s []Spec) []Spec { s[0].Domain = ""; return s }), "empty"},
		"shared domain":       {"global", "https", change(func(s []Spec) []Spec { s[1].Aliases = []string{"WWW.example.com"}; return s }), `domain "www.example.com" is also a domain of site "global"`},
		"bad country":         {"global", "https", change(func(s []Spec) []Spec { s[1].Countries = []string{"Iran"}; return s }), `"Iran" is not an ISO 3166-1`},
		"shared country":      {"global", "https", change(func(s []Spec) []Spec { s[2].Countries = []string{"IR"}; return s }), `country IR is also a country of site "ir"`},
		"no locales":          {"global", "https", change(func(s []Spec) []Spec { s[0].Locales = nil; return s }), "lists no locales"},
		"unsupported locale":  {"global", "https", change(func(s []Spec) []Spec { s[0].Locales = []string{"de"}; return s }), `locale "de" is not one of i18n.locales (en, fa, ar, tr)`},
		"locale twice":        {"global", "https", change(func(s []Spec) []Spec { s[0].Locales = []string{"en", "EN"}; return s }), `locale "en" is listed twice`},
		"enforce no country":  {"global", "https", change(func(s []Spec) []Spec { s[0].Enforce = true; return s }), "enforces its countries, but lists none"},
		"domain bad port":     {"global", "https", change(func(s []Spec) []Spec { s[0].Domain = "example.com:http"; return s }), "optional port"},
		"domain with user":    {"global", "https", change(func(s []Spec) []Spec { s[0].Domain = "user@example.com"; return s }), "without a scheme"},
		"domain not a host":   {"global", "https", change(func(s []Spec) []Spec { s[0].Domain = "exa_mple.com"; return s }), "is not a host name"},
		"region locale exact": {"global", "https", change(func(s []Spec) []Spec { s[0].Locales = []string{"en-GB"}; return s }), `locale "en-GB" is not one of`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(tc.def, tc.scheme, tc.specs, testLocales)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestRegistry_Lookups(t *testing.T) {
	r := testRegistry(t)
	assert.True(t, r.Enabled())
	assert.Equal(t, []string{"global", "ir", "tr"}, r.IDs())
	assert.Equal(t, "example.com", r.ByID("global").Domain)
	assert.Equal(t, "https://example.com", r.ByID("GLOBAL").URL)
	assert.Equal(t, "fa", r.ByID("ir").DefaultLocale)
	assert.Equal(t, []string{"CY", "TR"}, sorted(r.ByID("tr").Countries))

	for host, want := range map[string]string{
		"example.com": "global", "WWW.Example.com": "global", "example.com:443": "global",
		"example.ir": "ir", "www.example.ir.": "ir", "example.com.tr": "tr",
	} {
		site := r.ByHost(host)
		require.NotNil(t, site, host)
		assert.Equal(t, want, site.ID, host)
	}
	assert.Nil(t, r.ByHost("api.example.com"))
	assert.Nil(t, r.ByHost("example.com:8443"))
	assert.Nil(t, r.ByHost("evil.example"))

	site, served := r.ForCountry("ir")
	assert.True(t, served)
	assert.Equal(t, "ir", site.ID)
	site, served = r.ForCountry("DE")
	assert.False(t, served)
	assert.Equal(t, "global", site.ID)
	site, served = r.ForCountry("")
	assert.False(t, served)
	assert.Equal(t, "global", site.ID)

	assert.Equal(t, []string{
		"https://example.com", "https://www.example.com",
		"https://example.ir", "https://www.example.ir",
		"https://example.com.tr",
	}, r.Origins())
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestRegistry_Empty(t *testing.T) {
	r, err := New("", "", nil, testLocales)
	require.NoError(t, err)
	assert.False(t, r.Enabled())
	assert.Nil(t, r.Default())
	fa, _ := testLocales.Lookup("fa")
	res := r.Resolve(Request{Host: "example.com", Country: geoip.Result{Country: "IR"}, Locale: negotiated("fa", i18n.SourceCookie)})
	assert.Nil(t, res.Target)
	assert.False(t, res.RedirectNeeded())
	assert.Equal(t, fa, res.Locale)
	alternates, err := r.Alternates("/")
	require.NoError(t, err)
	assert.Empty(t, alternates)

	var nilRegistry *Registry
	assert.False(t, nilRegistry.Enabled())
	assert.Nil(t, nilRegistry.ByHost("example.com"))
	assert.Equal(t, "", nilRegistry.LocaleOfHost(httptest.NewRequest(http.MethodGet, "/", nil)))
}

// negotiated returns the negotiation of a request that named code in
// source.
func negotiated(code, source string) i18n.Negotiated {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	switch source {
	case i18n.SourceQuery:
		req.URL.RawQuery = "lang=" + code
	case i18n.SourceHeader:
		req.Header.Set(i18n.Header, code)
	case i18n.SourceCookie:
		req.AddCookie(&http.Cookie{Name: i18n.Cookie, Value: code})
	case i18n.SourceAcceptLanguage:
		req.Header.Set("Accept-Language", code)
	case i18n.SourceSite:
		return testLocales.NegotiateWith(req, func(*http.Request) string { return code })
	}
	n := testLocales.Negotiate(req)
	if n.Source != source {
		panic("the request negotiated " + n.Source + ", not " + source)
	}
	return n
}

func TestResolve(t *testing.T) {
	r := testRegistry(t)
	browser := func(code string) i18n.Negotiated { return negotiated(code, i18n.SourceAcceptLanguage) }
	defaults := negotiated("en", i18n.SourceDefault)
	for name, tc := range map[string]struct {
		req                          Request
		target, reason, locale, from string
		redirect                     bool
	}{
		"Iranian visitor on the global site": {
			Request{Host: "example.com", Country: geoip.Result{Country: "IR"}, Locale: browser("fa-IR")},
			"ir", ReasonCountry, "fa", i18n.SourceAcceptLanguage, true},
		"Iranian visitor with an English browser gets English there": {
			Request{Host: "example.com", Country: geoip.Result{Country: "IR"}, Locale: browser("en-US")},
			"ir", ReasonCountry, "en", i18n.SourceAcceptLanguage, true},
		"Iranian visitor without languages gets the site's default": {
			Request{Host: "www.example.com", Country: geoip.Result{Country: "IR"}, Locale: defaults},
			"ir", ReasonCountry, "fa", i18n.SourceSite, true},
		"German visitor: no site, no language: global in English": {
			Request{Host: "example.ir", Country: geoip.Result{Country: "DE"}, Locale: defaults},
			"global", ReasonDefault, "en", i18n.SourceSite, true},
		"Arabic speaker abroad gets Arabic on the global site": {
			Request{Host: "example.com", Country: geoip.Result{Country: "AE"}, Locale: browser("ar-AE")},
			"global", ReasonDefault, "ar", i18n.SourceAcceptLanguage, false},
		"Persian speaker abroad: the global site has no Persian": {
			Request{Host: "example.com", Country: geoip.Result{Country: "DE"}, Locale: browser("fa")},
			"global", ReasonDefault, "en", i18n.SourceSite, false},
		"unknown country stays global": {
			Request{Host: "example.com", Locale: defaults},
			"global", ReasonDefault, "en", i18n.SourceSite, false},
		"the site's own default locale gives way to the target's": {
			Request{Host: "example.ir", Country: geoip.Result{Country: "DE"}, Locale: negotiated("fa", i18n.SourceSite)},
			"global", ReasonDefault, "en", i18n.SourceSite, true},
		"a picked site wins over the country": {
			Request{Host: "example.ir", Country: geoip.Result{Country: "IR"}, Locale: browser("fa"), Choice: "global"},
			"global", ReasonChoice, "en", i18n.SourceSite, true},
		"a picked site the visitor is on": {
			Request{Host: "example.com", Country: geoip.Result{Country: "IR"}, Locale: defaults, Choice: "global"},
			"global", ReasonChoice, "en", i18n.SourceSite, false},
		"an unknown picked site is ignored": {
			Request{Host: "example.com", Country: geoip.Result{Country: "IR"}, Locale: defaults, Choice: "mars"},
			"ir", ReasonCountry, "fa", i18n.SourceSite, true},
		"a chosen locale the site offers keeps the visitor": {
			Request{Host: "example.com", Country: geoip.Result{Country: "IR"}, Locale: negotiated("en", i18n.SourceCookie)},
			"global", ReasonStay, "en", i18n.SourceCookie, false},
		"a chosen locale in the query keeps the visitor too": {
			Request{Host: "example.com", Country: geoip.Result{Country: "IR"}, Locale: negotiated("ar", i18n.SourceQuery)},
			"global", ReasonStay, "ar", i18n.SourceQuery, false},
		"a chosen locale the site does not offer moves the visitor": {
			Request{Host: "example.com", Country: geoip.Result{Country: "IR"}, Locale: negotiated("fa", i18n.SourceHeader)},
			"ir", ReasonCountry, "fa", i18n.SourceHeader, true},
		"an enforced site wins over every choice": {
			Request{Host: "example.com", Country: geoip.Result{Country: "CY"}, Locale: negotiated("en", i18n.SourceCookie), Choice: "global"},
			"tr", ReasonEnforced, "en", i18n.SourceCookie, true},
		"an enforced site wins over a crawler": {
			Request{Host: "example.com", Country: geoip.Result{Country: "TR"}, Locale: defaults, UserAgent: "Googlebot/2.1"},
			"tr", ReasonEnforced, "tr", i18n.SourceSite, true},
		"a crawler stays": {
			Request{Host: "example.com", Country: geoip.Result{Country: "IR"}, Locale: defaults, UserAgent: "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"},
			"global", ReasonCrawler, "en", i18n.SourceSite, false},
		"a call from no site gets the visitor's site, without a redirect": {
			Request{Host: "api.example.com", Country: geoip.Result{Country: "IR"}, Locale: defaults},
			"ir", ReasonCountry, "fa", i18n.SourceSite, false},
	} {
		t.Run(name, func(t *testing.T) {
			res := r.Resolve(tc.req)
			require.NotNil(t, res.Target)
			assert.Equal(t, tc.target, res.Target.ID)
			assert.Equal(t, tc.reason, res.Reason)
			assert.Equal(t, tc.locale, res.Locale.Code)
			assert.Equal(t, tc.from, res.LocaleSource)
			assert.Equal(t, tc.redirect, res.RedirectNeeded())
		})
	}
}

func TestURLAndPaths(t *testing.T) {
	r := testRegistry(t)
	ir := r.ByID("ir")
	fa, _ := testLocales.Lookup("fa")
	en, _ := testLocales.Lookup("en")

	u, err := r.URL(ir, fa, "/markets?tab=spot#top")
	require.NoError(t, err)
	assert.Equal(t, "https://example.ir/markets?tab=spot", u, "the default locale needs no lang, and the fragment stays in the browser")
	u, err = r.URL(ir, en, "/markets?tab=spot")
	require.NoError(t, err)
	assert.Equal(t, "https://example.ir/markets?lang=en&tab=spot", u)
	u, err = r.URL(ir, fa, "")
	require.NoError(t, err)
	assert.Equal(t, "https://example.ir/", u)
	u, err = r.URL(ir, fa, "/%D8%A8%D8%A7%D8%B2%D8%A7%D8%B1")
	require.NoError(t, err)
	assert.Equal(t, "https://example.ir/%D8%A8%D8%A7%D8%B2%D8%A7%D8%B1", u)

	for _, path := range []string{
		"//evil.example/", "/\\evil.example", "https://evil.example/", "evil.example",
		"/a\nb", "/a b", "\\\\evil", "javascript:alert(1)",
		"/" + string(make([]byte, MaxPathLength)),
	} {
		_, err := r.URL(ir, fa, path)
		assert.ErrorIs(t, err, ErrPath, "%q", path)
	}
	// An encoded line break stays encoded, so it cannot split a header.
	u, err = r.URL(ir, fa, "/a%0d%0ab")
	require.NoError(t, err)
	assert.Equal(t, "https://example.ir/a%0d%0ab", u)
}

func TestAlternates(t *testing.T) {
	r := testRegistry(t)
	alternates, err := r.Alternates("/markets")
	require.NoError(t, err)
	got := map[string]string{}
	for _, a := range alternates {
		got[a.HrefLang] = a.URL
	}
	assert.Equal(t, "https://example.com/markets", got["x-default"])
	assert.Equal(t, "https://example.com/markets", got["en"])
	assert.Equal(t, "https://example.ir/markets", got["fa-IR"])
	assert.Equal(t, "https://example.ir/markets?lang=en", got["en-IR"])
	assert.Equal(t, "https://example.com/markets?lang=ar", got["ar"])
	// A site of two countries has one entry per country, so that no
	// hreflang appears twice.
	assert.Equal(t, "https://example.com.tr/markets", got["tr-TR"])
	assert.Equal(t, "https://example.com.tr/markets", got["tr-CY"])
	assert.Equal(t, "https://example.com.tr/markets?lang=en", got["en-CY"])
	assert.Len(t, alternates, 9)
	assert.Len(t, got, 9)

	_, err = r.Alternates("//evil")
	assert.ErrorIs(t, err, ErrPath)
}

func TestRequestHostAndLocaleOfHost(t *testing.T) {
	r := testRegistry(t)
	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/x", nil)
	assert.Equal(t, "api.example.com", RequestHost(req, false))
	req.Header.Set("X-Forwarded-Host", "example.ir, proxy")
	assert.Equal(t, "api.example.com", RequestHost(req, false), "not from a trusted proxy")
	assert.Equal(t, "example.ir", RequestHost(req, true))
	req.Header.Set("Origin", "https://www.example.ir")
	assert.Equal(t, "www.example.ir", RequestHost(req, true), "the Origin of the page comes first")
	assert.Equal(t, "fa", r.LocaleOfHost(req))
	req.Header.Set("Origin", "null")
	assert.Equal(t, "", r.LocaleOfHost(req))
}

func TestIsCrawler(t *testing.T) {
	for _, ua := range []string{
		"Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)",
		"Mozilla/5.0 (compatible; YandexBot/3.0)",
		"facebookexternalhit/1.1",
		"Mozilla/5.0 (Linux; Android 6.0.1; Nexus 5X Build/MMB29P) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/W.X.Y.Z Mobile Safari/537.36 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
		"TelegramBot (like TwitterBot)",
	} {
		assert.True(t, IsCrawler(ua), ua)
	}
	for _, ua := range []string{"", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"} {
		assert.False(t, IsCrawler(ua), ua)
	}
}
