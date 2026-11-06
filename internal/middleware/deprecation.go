package middleware

import (
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
)

// Deprecated marks the responses of a route that has a successor under
// prefix, so that clients can find it and log the use of the old one:
//
//   - Deprecation (RFC 9745) says since when the route is deprecated;
//   - Link (RFC 8288) points to the successor, the same path under prefix.
//
// The route still answers as before.
func Deprecated(prefix string, since time.Time) gin.HandlerFunc {
	deprecation := fmt.Sprintf("@%d", since.Unix())
	return func(c *gin.Context) {
		c.Header("Deprecation", deprecation)
		c.Header("Link", fmt.Sprintf(`<%s%s>; rel="successor-version"`, prefix, c.Request.URL.EscapedPath()))
		c.Next()
	}
}
