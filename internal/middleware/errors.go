package middleware

import (
	"net/http"

	"encrypted-db/config"
	"encrypted-db/internal/helpers"

	"github.com/gin-gonic/gin"
)

// RecoveryMiddleware turns a panic into a 500 response in the standard
// envelope. Gin logs the panic value and the stack trace on the server; the
// response never contains them, in any mode, because they reveal internal
// details to anyone who can trigger the panic.
func RecoveryMiddleware() gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered interface{}) {
		helpers.SendResponse(c, http.StatusInternalServerError, "Internal server error", nil)
	})
}

func ErrorSanitizer() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		if len(c.Errors) > 0 {
			for _, e := range c.Errors {
				if config.Config.Server.Mode == "release" {
					e.SetMeta(map[string]bool{"sanitized": true})
				}
			}
		}
	}
}

func SanitizeError(err error, isProduction bool) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if isProduction {
		sensitivePatterns := []string{
			"password", "secret", "token", "key", "credential",
			"connection string", "database", "postgres://", "redis://",
			"stack trace", "goroutine", "runtime.", "main.",
		}
		for _, pattern := range sensitivePatterns {
			if len(msg) > 0 && containsIgnoreCase(msg, pattern) {
				return "Internal server error"
			}
		}
	}
	return msg
}

func containsIgnoreCase(s, substr string) bool {
	sLower := ""
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			sLower += string(r + 32)
		} else {
			sLower += string(r)
		}
	}
	substrLower := ""
	for _, r := range substr {
		if r >= 'A' && r <= 'Z' {
			substrLower += string(r + 32)
		} else {
			substrLower += string(r)
		}
	}
	return len(sLower) >= len(substrLower) && (sLower == substrLower || len(sLower) > len(substrLower) && (sLower[:len(substrLower)] == substrLower || sLower[len(sLower)-len(substrLower):] == substrLower || containsSubstring(sLower, substrLower)))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
