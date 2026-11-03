package middleware

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-XSS-Protection", "1; mode=block")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		c.Header("Cross-Origin-Opener-Policy", "same-origin")
		c.Header("Cross-Origin-Resource-Policy", "same-origin")
		c.Next()
	}
}

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
			c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, X-Requested-With")
			c.Header("Access-Control-Max-Age", "86400")
		}

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}, nil
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
