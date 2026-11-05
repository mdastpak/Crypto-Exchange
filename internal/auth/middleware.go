package auth

import (
	"net/http"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/helpers"

	"github.com/gin-gonic/gin"
)

// Context keys set by RequireRole.
const (
	claimsContextKey      = "claims"
	accessTokenContextKey = "access_token"
)

// bearerToken returns the token of an "Authorization: Bearer <token>" header
// (RFC 6750). The scheme is case-insensitive. Other schemes and bare tokens
// are rejected.
func bearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}

func unauthorized(c *gin.Context, message string) {
	c.Header("WWW-Authenticate", `Bearer`)
	helpers.SendResponse(c, http.StatusUnauthorized, message, nil)
	c.Abort()
}

// RequireRole authenticates a request with a Bearer access token of role and
// rejects blacklisted tokens. A token of another role fails verification,
// because each role has its own keys and audience. On success the claims and
// the token are available to handlers through GetClaims and accessToken.
func RequireRole(role string, tb *TokenBlacklist) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" {
			unauthorized(c, "Unauthorized - Token not provided")
			return
		}
		token, ok := bearerToken(header)
		if !ok {
			unauthorized(c, "Unauthorized - Bearer token required")
			return
		}

		claims, err := ParseToken(token, role, TokenTypeAccess)
		if err != nil {
			unauthorized(c, "Unauthorized - Invalid token")
			return
		}

		if tb != nil {
			isBlacklisted, err := tb.IsTokenBlacklisted(c.Request.Context(), token)
			if err != nil {
				helpers.SendResponse(c, http.StatusInternalServerError, "Error checking blacklist", nil)
				c.Abort()
				return
			}
			if isBlacklisted {
				unauthorized(c, "Unauthorized - Token is blacklisted")
				return
			}
		}

		c.Set(claimsContextKey, claims)
		c.Set(accessTokenContextKey, token)
		c.Next()
	}
}

// JWTAdminVerification accepts only admin access tokens.
func JWTAdminVerification(tb *TokenBlacklist) gin.HandlerFunc {
	return RequireRole(RoleAdmin, tb)
}

// JWTUserVerification accepts only user access tokens.
func JWTUserVerification(tb *TokenBlacklist) gin.HandlerFunc {
	return RequireRole(RoleUser, tb)
}

// GetClaims returns the claims of the token that RequireRole accepted.
func GetClaims(c *gin.Context) (*Claims, bool) {
	claims, exists := c.Get(claimsContextKey)
	if !exists {
		return nil, false
	}
	casted, ok := claims.(*Claims)
	return casted, ok
}

// accessToken returns the token that RequireRole accepted.
func accessToken(c *gin.Context) (string, bool) {
	token, ok := c.Get(accessTokenContextKey)
	if !ok {
		return "", false
	}
	s, ok := token.(string)
	return s, ok && s != ""
}
