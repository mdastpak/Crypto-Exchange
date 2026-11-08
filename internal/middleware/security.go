package middleware

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// SecurityOptions configures SecurityHeaders.
type SecurityOptions struct {
	// HSTS sends Strict-Transport-Security, with HSTSMaxAge in seconds,
	// and includeSubDomains with HSTSIncludeSubdomains.
	HSTS                  bool
	HSTSMaxAge            int
	HSTSIncludeSubdomains bool
	// DocsPrefix is the path of the Swagger UI, which gets a policy that
	// lets it run its own scripts and styles. Empty means none.
	DocsPrefix string
}

// The content security policies (#191). The API returns JSON only, which
// needs no content at all. The Swagger UI loads its scripts, styles, and
// images from the server, and its page starts it with an inline script.
const (
	apiContentSecurityPolicy  = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
	docsContentSecurityPolicy = "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
)

// SecurityHeaders sets the headers that every response carries. Responses
// hold tokens and personal data, so they are not stored by browsers or
// caches (Cache-Control: no-store); a handler whose answer is public may
// set Cache-Control again.
func SecurityHeaders(opts SecurityOptions) gin.HandlerFunc {
	hsts := ""
	if opts.HSTS {
		hsts = fmt.Sprintf("max-age=%d", opts.HSTSMaxAge)
		if opts.HSTSIncludeSubdomains {
			hsts += "; includeSubDomains"
		}
	}
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		// The XSS auditor of old browsers is a source of bugs of its own;
		// the content security policy replaces it.
		h.Set("X-XSS-Protection", "0")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		if opts.DocsPrefix != "" && strings.HasPrefix(c.Request.URL.Path, opts.DocsPrefix) {
			h.Set("Content-Security-Policy", docsContentSecurityPolicy)
		} else {
			h.Set("Content-Security-Policy", apiContentSecurityPolicy)
		}
		if hsts != "" {
			h.Set("Strict-Transport-Security", hsts)
		}
		c.Next()
	}
}

// exposedHeaders are the response headers that the API documents for
// clients, which a cross-origin script can read only when they are listed
// in Access-Control-Expose-Headers.
const exposedHeaders = "X-Request-ID, Retry-After, X-RateLimit-Limit, X-RateLimit-Remaining, Deprecation, Link"

// NewCORSMiddleware grants cross-origin access to the listed origins only.
// Allowed origins may send credentials (the refresh token cookie), so a
// wildcard is rejected: combined with credentials it would let any site read
// authenticated responses. With no origins, no cross-origin access is granted.
func NewCORSMiddleware(allowedOrigins []string) (gin.HandlerFunc, error) {
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		normalized, err := normalizeOrigin(origin)
		if err != nil {
			return nil, err
		}
		allowed[normalized] = true
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			// The CORS headers depend on the Origin header, so shared caches
			// must not reuse a response for a different origin.
			c.Writer.Header().Add("Vary", "Origin")
		}
		if origin != "" && allowed[strings.ToLower(origin)] {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, X-Requested-With, X-Locale")
			c.Header("Access-Control-Max-Age", "86400")
			c.Header("Access-Control-Expose-Headers", exposedHeaders)
		}
		c.Next()
	}, nil
}

// Preflight answers the CORS preflight requests (OPTIONS), with the
// headers that the CORS middleware set. It runs after the rate limit, so
// that preflights are counted like every other request (#191).
func Preflight() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// normalizeOrigin validates a configured origin (scheme and host, with an
// optional port, and nothing else) and returns it in lower case, the form in
// which browsers send it.
func normalizeOrigin(origin string) (string, error) {
	if origin == "*" {
		return "", fmt.Errorf("CORS origin %q is not allowed: a wildcard cannot be combined with credentials; list explicit origins", origin)
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid CORS origin %q: expected scheme://host[:port], for example https://app.example.com", origin)
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), nil
}
