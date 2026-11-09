package i18n

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
)

// The places a request names its locale in.
const (
	// QueryParam is a choice for one request, such as a link to a page in
	// another language.
	QueryParam = "lang"
	// Header is the choice of an app, which has no cookies.
	Header = "X-Locale"
	// Cookie is the choice that a visitor made in the language switcher,
	// saved by the frontend.
	Cookie = "locale"
)

// VaryHeaders are the request headers that a localized answer depends on,
// for the Vary header of the response.
const VaryHeaders = "Accept-Language, Cookie, X-Locale"

// Where the locale of a request came from.
const (
	SourceQuery          = "query"
	SourceHeader         = "header"
	SourceCookie         = "cookie"
	SourceAcceptLanguage = "accept_language"
	// SourceProfile is the locale that a signed-in user saved in their
	// profile (#286). It comes after the explicit choices of the request
	// and before Accept-Language.
	SourceProfile = "profile"
	// SourceSite is the default locale of the site that the request is
	// on (#285), when the request names no supported locale.
	SourceSite    = "site"
	SourceDefault = "default"
)

// Negotiated is the locale of a request.
type Negotiated struct {
	Locale Locale
	// Source is where it came from, one of the Source constants.
	Source string

	registry *Registry
}

// Explicit reports whether the visitor chose the locale, as opposed to
// their browser's languages or the default.
func (n Negotiated) Explicit() bool {
	switch n.Source {
	case SourceQuery, SourceHeader, SourceCookie:
		return true
	}
	return false
}

// Negotiate returns the locale of a request. The explicit choices come
// first, in the order of their scope: the query parameter for this request,
// the header of an app, and the cookie of the language switcher. Then the
// browser's Accept-Language, then the default. A value that names no
// supported locale is skipped, never an error, so that a stale cookie or a
// shared link in an unsupported language still gets an answer.
func (r *Registry) Negotiate(req *http.Request) Negotiated {
	n := Negotiated{registry: r}
	if l, ok := r.Lookup(req.URL.Query().Get(QueryParam)); ok {
		n.Locale, n.Source = l, SourceQuery
		return n
	}
	if l, ok := r.Lookup(req.Header.Get(Header)); ok {
		n.Locale, n.Source = l, SourceHeader
		return n
	}
	if cookie, err := req.Cookie(Cookie); err == nil {
		if l, ok := r.Lookup(cookie.Value); ok {
			n.Locale, n.Source = l, SourceCookie
			return n
		}
	}
	if l, ok := r.Match(req.Header.Get("Accept-Language")); ok {
		n.Locale, n.Source = l, SourceAcceptLanguage
		return n
	}
	n.Locale, n.Source = r.Default(), SourceDefault
	return n
}

// NegotiateWith is Negotiate with a fallback before the default: the
// locale that fallback returns for the request, such as the default
// locale of the site it is on (#285).
func (r *Registry) NegotiateWith(req *http.Request, fallback func(*http.Request) string) Negotiated {
	n := r.Negotiate(req)
	if n.Source != SourceDefault || fallback == nil {
		return n
	}
	if l, ok := r.Lookup(fallback(req)); ok {
		n.Locale, n.Source = l, SourceSite
	}
	return n
}

// Render returns m in the negotiated locale, and the language its text is
// in; see Registry.Render.
func (n Negotiated) Render(m Message) (text, language string) {
	if n.registry == nil {
		return m.String(), English
	}
	return n.registry.Render(n.Locale, m)
}

type contextKey struct{}

// ginKey is the key of the Negotiated in a Gin context.
const ginKey = "i18n.negotiated"

// Middleware negotiates the locale of every request and keeps it on the
// request, for the handlers and the response envelope. fallback, when it
// is set, names the locale of a request that names none, before the
// default; see NegotiateWith.
func Middleware(r *Registry, fallback ...func(*http.Request) string) gin.HandlerFunc {
	var f func(*http.Request) string
	if len(fallback) > 0 {
		f = fallback[0]
	}
	return func(c *gin.Context) {
		n := r.NegotiateWith(c.Request, f)
		c.Set(ginKey, n)
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), contextKey{}, n))
		c.Next()
	}
}

// FromGin returns the locale of a request that Middleware negotiated.
func FromGin(c *gin.Context) (Negotiated, bool) {
	if c == nil {
		return Negotiated{}, false
	}
	v, ok := c.Get(ginKey)
	if !ok {
		return Negotiated{}, false
	}
	n, ok := v.(Negotiated)
	return n, ok
}

// FromContext returns the locale of a request from its context, for code
// that has no Gin context, such as a notification sent for the request.
func FromContext(ctx context.Context) (Negotiated, bool) {
	n, ok := ctx.Value(contextKey{}).(Negotiated)
	return n, ok
}

// Prefer applies the locale that a signed-in user saved (#286) to the
// request, once the user is known: it wins over Accept-Language, the site,
// and the default, but not over an explicit choice of the request. A code
// that is empty or no longer supported changes nothing. It reports whether
// the locale changed.
func Prefer(c *gin.Context, code string) bool {
	n, ok := FromGin(c)
	if !ok || code == "" || n.Explicit() || n.registry == nil {
		return false
	}
	l, ok := n.registry.Lookup(code)
	if !ok {
		return false
	}
	n.Locale, n.Source = l, SourceProfile
	c.Set(ginKey, n)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), contextKey{}, n))
	return true
}
