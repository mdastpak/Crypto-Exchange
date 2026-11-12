package logging

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
)

// Requests logs every request as one record after it is answered: its
// method, route, status, and duration, with its request ID and, for a
// signed-in request, the ID of its user or administrator, which userOf
// returns. A server error is logged at ERROR, every other answer at INFO.
// The route is the pattern, such as /api/v1/user/contacts/:id, so that a
// record holds no value of the path; the query string is never logged.
func Requests(logger *slog.Logger, userOf func(*gin.Context) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		status := c.Writer.Status()
		level := slog.LevelInfo
		if status >= 500 {
			level = slog.LevelError
		}
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		attrs := []slog.Attr{
			slog.String("method", c.Request.Method),
			slog.String("route", route),
			slog.Int("status", status),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		}
		if id := c.GetString("request_id"); id != "" {
			attrs = append(attrs, slog.String("request_id", id))
		}
		if userOf != nil {
			if user := userOf(c); user != "" {
				attrs = append(attrs, slog.String("user_id", user))
			}
		}
		logger.LogAttrs(c.Request.Context(), level, "request", attrs...)
	}
}
