package sites

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/mdastpak/crypto-exchange/internal/geoip"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
)

// ChoiceCookie keeps the site that a visitor picked in the site switcher.
// The locale they picked is in i18n.Cookie.
const ChoiceCookie = "site"

// Why a visitor belongs to the site they are sent to.
const (
	// ReasonEnforced: their country's site keeps its visitors.
	ReasonEnforced = "enforced"
	// ReasonChoice: they picked the site.
	ReasonChoice = "choice"
	// ReasonStay: they chose a locale on the site they are on, which offers
	// it, so they are not moved.
	ReasonStay = "stay"
	// ReasonCrawler: search engines index every site as it is.
	ReasonCrawler = "crawler"
	// ReasonCountry: the site of their country.
	ReasonCountry = "country"
	// ReasonDefault: no site serves their country, or it is not known.
	ReasonDefault = "default"
)

// Request is what the resolution of a visitor depends on.
type Request struct {
	// Host is the host of the page the visitor is on.
	Host string
	// Country is the visitor's country.
	Country geoip.Result
	// Locale is the locale that the request negotiated.
	Locale i18n.Negotiated
	// Choice is the site in the visitor's ChoiceCookie, if any.
	Choice string
	// UserAgent tells crawlers apart.
	UserAgent string
}

// Resolution is where a visitor belongs.
type Resolution struct {
	// Current is the site the visitor is on, or nil for a host that is
	// no site, such as the API's own.
	Current *Site
	// Target is the site the visitor belongs to.
	Target *Site
	// Reason says why, one of the Reason constants.
	Reason string
	// Locale is the locale on Target, and LocaleSource where it came from:
	// a source of i18n, or i18n.SourceSite for the target's default.
	Locale       i18n.Locale
	LocaleSource string
	// Explicit reports whether the visitor chose the locale.
	Explicit bool
}

// RedirectNeeded reports whether the visitor is on a site and belongs to
// another.
func (res Resolution) RedirectNeeded() bool {
	return res.Current != nil && res.Target != nil && res.Current != res.Target
}

// Resolve decides where a visitor belongs, in this order:
//
//  1. a site that enforces the visitor's country;
//  2. a crawler stays on the site it is on;
//  3. the site the visitor picked;
//  4. the site the visitor is on, when they chose a locale it offers;
//  5. the site of their country, or else the default site.
//
// The locale is the negotiated one when the visitor named it, or their
// browser asked for it, and the target offers it; else the target's
// default locale. So a visitor whose language the target does not offer
// gets its default: on the global site, English.
func (r *Registry) Resolve(req Request) Resolution {
	res := Resolution{Locale: req.Locale.Locale, LocaleSource: req.Locale.Source, Explicit: req.Locale.Explicit()}
	if !r.Enabled() {
		return res
	}
	res.Current = r.ByHost(req.Host)
	countrySite, served := r.ForCountry(req.Country.Country)
	chosen := r.ByID(req.Choice)
	switch {
	case served && countrySite.Enforce:
		res.Target, res.Reason = countrySite, ReasonEnforced
	case res.Current != nil && IsCrawler(req.UserAgent):
		res.Target, res.Reason = res.Current, ReasonCrawler
	case chosen != nil:
		res.Target, res.Reason = chosen, ReasonChoice
	case res.Current != nil && res.Explicit && offers(res.Current, req.Locale.Locale):
		res.Target, res.Reason = res.Current, ReasonStay
	case served:
		res.Target, res.Reason = countrySite, ReasonCountry
	default:
		res.Target, res.Reason = r.def, ReasonDefault
	}

	// A locale that only the default or the site the visitor is on chose
	// gives way to the target's own default.
	asked := req.Locale.Source != i18n.SourceDefault && req.Locale.Source != i18n.SourceSite
	if l, ok := res.Target.Offers(req.Locale.Locale); ok && asked {
		res.Locale = l
		return res
	}
	res.Locale, res.LocaleSource, res.Explicit = res.Target.Default(), i18n.SourceSite, false
	return res
}

func offers(s *Site, l i18n.Locale) bool {
	_, ok := s.Offers(l)
	return ok
}

// URL returns the address of path on a site in a locale. The locale is
// named in the lang query parameter unless it is the site's default, so
// that a choice made on one domain reaches the next one, where the cookie
// of the first is not sent.
func (r *Registry) URL(site *Site, l i18n.Locale, path string) (string, error) {
	u, err := ParsePath(path)
	if err != nil {
		return "", err
	}
	if l.Code != "" && l.Code != site.DefaultLocale {
		q := u.Query()
		q.Set(i18n.QueryParam, l.Code)
		u.RawQuery = q.Encode()
	}
	return site.URL + u.String(), nil
}

// ErrPath reports a path that is not a local path of a site.
var ErrPath = errors.New("path must be a local path that starts with a single /")

// MaxPathLength bounds the path of a redirect.
const MaxPathLength = 2048

// ParsePath reads the path of a page, with its query, and refuses
// anything that could lead to another host: a scheme, a host, "//" or
// "/\" at the start, which browsers read as a host, backslashes, and
// control characters. An empty path is "/".
func ParsePath(path string) (*url.URL, error) {
	if path == "" {
		path = "/"
	}
	if len(path) > MaxPathLength || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsRune(path, '\\') {
		return nil, ErrPath
	}
	for _, r := range path {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return nil, ErrPath
		}
	}
	u, err := url.Parse(path)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return nil, ErrPath
	}
	u.Fragment, u.RawFragment = "", ""
	return u, nil
}

// Alternate is the address of a page on one site in one locale, for the
// hreflang links of the page.
type Alternate struct {
	Site     string `json:"site" example:"ir"`
	Locale   string `json:"locale" example:"fa"`
	HrefLang string `json:"hreflang" example:"fa-IR"`
	URL      string `json:"url" example:"https://example.ir/markets"`
}

// Alternates returns the address of path on every site in every locale,
// and the x-default address on the default site in its default locale.
// The hreflang of a locale on a site of countries names each country,
// such as fa-IR, so that search engines show each site where it belongs
// and no hreflang appears twice.
func (r *Registry) Alternates(path string) ([]Alternate, error) {
	if _, err := ParsePath(path); err != nil {
		return nil, err
	}
	var alternates []Alternate
	for _, s := range r.Sites() {
		for _, l := range s.locales {
			href, _ := r.URL(s, l, path)
			if len(s.Countries) == 0 {
				alternates = append(alternates, Alternate{Site: s.ID, Locale: l.Code, HrefLang: l.Code, URL: href})
				continue
			}
			for _, country := range s.Countries {
				alternates = append(alternates, Alternate{Site: s.ID, Locale: l.Code, HrefLang: l.Base() + "-" + country, URL: href})
			}
		}
	}
	if r.Enabled() {
		href, _ := r.URL(r.def, r.def.Default(), path)
		alternates = append(alternates, Alternate{Site: r.def.ID, Locale: r.def.DefaultLocale, HrefLang: "x-default", URL: href})
	}
	return alternates, nil
}

// crawlerMarks are parts of the User-Agent of search engines and link
// previews, in lower case.
var crawlerMarks = []string{
	"bot", "crawler", "spider", "slurp", "facebookexternalhit", "embedly",
	"mediapartners-google", "adsbot-google", "google-inspectiontool",
	"bingpreview", "yandex", "baiduspider", "duckduckgo", "applebot",
	"petalbot", "semrush", "ahrefs", "whatsapp", "telegram", "skypeuripreview",
}

// IsCrawler reports whether a User-Agent is a search engine or a link
// preview, which must see each site as it is instead of being redirected.
func IsCrawler(userAgent string) bool {
	ua := strings.ToLower(userAgent)
	if ua == "" {
		return false
	}
	for _, mark := range crawlerMarks {
		if strings.Contains(ua, mark) {
			return true
		}
	}
	return false
}

// RequestHost returns the host of the page that a request comes from: the
// host of its Origin, which browsers send with every cross-origin call and
// a page cannot change; else X-Forwarded-Host, when forwarded says that
// the request came through a trusted proxy; else its Host.
func RequestHost(req *http.Request, forwarded bool) string {
	if origin := req.Header.Get("Origin"); origin != "" && origin != "null" {
		if u, err := url.Parse(origin); err == nil && u.Host != "" {
			return u.Host
		}
	}
	if forwarded {
		if host := strings.TrimSpace(strings.Split(req.Header.Get("X-Forwarded-Host"), ",")[0]); host != "" {
			return host
		}
	}
	return req.Host
}

// LocaleOfHost returns the default locale of the site that a request is
// on, for i18n.Middleware, or "" when it is on none.
func (r *Registry) LocaleOfHost(req *http.Request) string {
	if site := r.ByHost(RequestHost(req, false)); site != nil {
		return site.DefaultLocale
	}
	return ""
}
