package public

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/geoip"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/sites"

	"github.com/gin-gonic/gin"
)

// Codes of the site routes (#285).
const (
	CodeInvalidPath    = "invalid_path"
	CodeUnknownSite    = "unknown_site"
	CodeUnknownLocale  = "unknown_locale"
	choiceCookieMaxAge = 365 * 24 * time.Hour
)

// SiteHandler serves the routes that tell a visitor which site and locale
// they belong to (#285).
type SiteHandler struct {
	Sites   *sites.Registry
	Locales *i18n.Registry
	// GeoIP finds the visitor's country; nil knows none.
	GeoIP CountryResolver
	// TrustedProxies are the proxies whose X-Forwarded-Host is believed.
	TrustedProxies []netip.Prefix
}

// SiteInfo is the answer of GET /public/site.
type SiteInfo struct {
	// Country of the visitor, "" when it is not known, and its source.
	Country       string `json:"country" example:"IR"`
	CountrySource string `json:"country_source" example:"database" enums:"header,database,override,none"`
	// CurrentSite is the ID of the site the visitor is on, or null.
	CurrentSite *string `json:"current_site" example:"global"`
	// Site is the site the visitor belongs to, or null without sites.
	Site *sites.Site `json:"site"`
	// Reason says why: enforced, choice, stay, crawler, country, or
	// default.
	Reason string `json:"reason,omitempty" example:"country" enums:"enforced,choice,stay,crawler,country,default"`
	// Locale is the locale to show on it, with its direction.
	Locale       i18n.Locale `json:"locale"`
	LocaleSource string      `json:"locale_source" example:"site" enums:"query,header,cookie,accept_language,site,default"`
	// Redirect says whether to send the visitor to URL.
	Redirect SiteRedirect `json:"redirect"`
	// Sites are every site, for a site switcher.
	Sites []*sites.Site `json:"sites"`
	// Alternates are the URLs of the page on every site and locale, for
	// hreflang links.
	Alternates []sites.Alternate `json:"alternates"`
}

// SiteRedirect is where to send the visitor.
type SiteRedirect struct {
	Needed bool   `json:"needed"`
	URL    string `json:"url,omitempty" example:"https://example.ir/markets"`
}

// SiteChoice is the body of POST /public/site/choice.
type SiteChoice struct {
	// Site is the ID of the site the visitor picked, or "" for none.
	Site string `json:"site" example:"global"`
	// Locale is the code of the locale they picked, or "" for none.
	Locale string `json:"locale" example:"en"`
}

func (h *SiteHandler) forwarded(c *gin.Context) bool {
	peer, err := netip.ParseAddr(c.RemoteIP())
	if err != nil {
		return false
	}
	peer = peer.Unmap()
	for _, prefix := range h.TrustedProxies {
		if prefix.Contains(peer) {
			return true
		}
	}
	return false
}

func (h *SiteHandler) resolve(c *gin.Context) sites.Resolution {
	negotiated, ok := i18n.FromGin(c)
	if !ok {
		negotiated = h.Locales.Negotiate(c.Request)
	}
	choice := ""
	if cookie, err := c.Request.Cookie(sites.ChoiceCookie); err == nil {
		choice = cookie.Value
	}
	return h.Sites.Resolve(sites.Request{
		Host:      sites.RequestHost(c.Request, h.forwarded(c)),
		Country:   h.country(c),
		Locale:    negotiated,
		Choice:    choice,
		UserAgent: c.Request.UserAgent(),
	})
}

func (h *SiteHandler) path(c *gin.Context) (string, bool) {
	path := c.Query("path")
	if _, err := sites.ParsePath(path); err != nil {
		helpers.SendError(c, http.StatusBadRequest, CodeInvalidPath, i18n.M("INVALID_PATH"), nil)
		return "", false
	}
	if path == "" {
		path = "/"
	}
	return path, true
}

// Site answers GET /public/site.
//
// @Summary Show the site and locale the visitor belongs to
// @Description The visitor's country (GeoIP), the site they are on (by the Origin of the call, X-Forwarded-Host through a trusted proxy, or Host), the site they belong to and why, the locale to show with its direction, whether to redirect and where, every site for a switcher, and the alternates of the page for hreflang links (#285). A site that enforces the visitor's country comes first; then a crawler stays where it is; then the site the visitor picked (site cookie); then the site they are on when they chose a locale it offers; then the site of their country; then the default site. The locale is the one the visitor or their browser asked for when the site offers it, else the site's default.
// @Tags system
// @Produce json
// @Param path query string false "The page, a local path such as /markets?tab=spot" default(/)
// @Success 200 {object} models.APIResponse{info=SiteInfo}
// @Failure 400 {object} models.APIResponse "invalid_path"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/site [get]
func (h *SiteHandler) Site(c *gin.Context) {
	path, ok := h.path(c)
	if !ok {
		return
	}
	res := h.resolve(c)
	country := h.country(c)
	info := SiteInfo{
		Country:       country.Country,
		CountrySource: country.Source,
		Site:          res.Target,
		Reason:        res.Reason,
		Locale:        res.Locale,
		LocaleSource:  res.LocaleSource,
		Sites:         h.Sites.Sites(),
		Alternates:    []sites.Alternate{},
	}
	if info.Sites == nil {
		info.Sites = []*sites.Site{}
	}
	if res.Current != nil {
		info.CurrentSite = &res.Current.ID
	}
	if res.Target != nil {
		url, _ := h.Sites.URL(res.Target, res.Locale, path)
		info.Redirect = SiteRedirect{Needed: res.RedirectNeeded(), URL: url}
		info.Alternates, _ = h.Sites.Alternates(path)
	}
	privateAnswer(c)
	helpers.SendResponse(c, http.StatusOK, i18n.M("SITE_RETRIEVED_SUCCESSFULLY"), info)
}

// privateAnswer marks an answer that depends on the visitor.
func privateAnswer(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	c.Writer.Header().Add("Vary", "Origin, "+i18n.VaryHeaders)
}

// Redirect answers GET /public/site/redirect.
//
// @Summary Redirect the visitor to their site
// @Description 302 to the same page on the site the visitor belongs to, chosen as GET /public/site chooses it, or 204 when they are already on it, or when there are no sites (#285). Frontends, or an edge function, call it on the first page view. Only the configured domains are ever targets, and crawlers are never redirected.
// @Tags system
// @Produce json
// @Param path query string false "The page, a local path such as /markets" default(/)
// @Success 204 "The visitor is on their site"
// @Success 302 "To the visitor's site; Location names the page there"
// @Failure 400 {object} models.APIResponse "invalid_path"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/site/redirect [get]
func (h *SiteHandler) Redirect(c *gin.Context) {
	path, ok := h.path(c)
	if !ok {
		return
	}
	privateAnswer(c)
	res := h.resolve(c)
	if !res.RedirectNeeded() {
		c.Status(http.StatusNoContent)
		return
	}
	url, err := h.Sites.URL(res.Target, res.Locale, path)
	if err != nil {
		helpers.SendError(c, http.StatusBadRequest, CodeInvalidPath, i18n.M("INVALID_PATH"), nil)
		return
	}
	c.Redirect(http.StatusFound, url)
}

const maxChoiceBody = 1 << 10

// Choice answers POST /public/site/choice.
//
// @Summary Save the site and locale the visitor picked
// @Description Saves the visitor's explicit choice as the site and locale cookies of the API's host, for a year, so that GeoIP no longer moves them, except to a site that enforces their country (#285). An empty value clears that cookie. Answers what GET /public/site answers after the choice, for the root page.
// @Tags system
// @Accept json
// @Produce json
// @Param body body SiteChoice true "The site and locale"
// @Success 200 {object} models.APIResponse{info=SiteChoice}
// @Failure 400 {object} models.APIResponse "invalid_request, unknown_site, or unknown_locale"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/site/choice [post]
func (h *SiteHandler) Choice(c *gin.Context) {
	var choice SiteChoice
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, maxChoiceBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&choice); err != nil && !errors.Is(err, io.EOF) {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_REQUEST_BODY"), nil)
		return
	}
	saved := SiteChoice{}
	if choice.Site != "" {
		site := h.Sites.ByID(choice.Site)
		if site == nil {
			helpers.SendError(c, http.StatusBadRequest, CodeUnknownSite, i18n.M("UNKNOWN_SITE"), nil)
			return
		}
		saved.Site = site.ID
	}
	if choice.Locale != "" {
		l, ok := h.Locales.Lookup(choice.Locale)
		if !ok {
			helpers.SendError(c, http.StatusBadRequest, CodeUnknownLocale, i18n.M("UNKNOWN_LOCALE"), nil)
			return
		}
		saved.Locale = l.Code
	}
	h.setCookie(c, sites.ChoiceCookie, saved.Site)
	h.setCookie(c, i18n.Cookie, saved.Locale)
	privateAnswer(c)
	helpers.SendResponse(c, http.StatusOK, i18n.M("SITE_CHOICE_SAVED"), saved)
}

// setCookie sets a choice cookie, or clears it for "". Like the refresh
// token cookie, it is Secure, which browsers also accept on localhost, and
// HttpOnly: the frontend reads the choice from GET /public/site. It is Lax,
// so that it is sent when the visitor follows a link to the site.
func (h *SiteHandler) setCookie(c *gin.Context, name, value string) {
	cookie := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int(choiceCookieMaxAge / time.Second),
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if value == "" {
		cookie.MaxAge = -1
	}
	http.SetCookie(c.Writer, cookie)
}

// CountryResolver finds the country of a request (#284): *geoip.Resolver.
type CountryResolver interface {
	Resolve(c *gin.Context) geoip.Result
}

// country is the country of the request, or none without a resolver.
func (h *SiteHandler) country(c *gin.Context) geoip.Result {
	if h.GeoIP == nil {
		return geoip.Result{Source: geoip.SourceNone}
	}
	return h.GeoIP.Resolve(c)
}
