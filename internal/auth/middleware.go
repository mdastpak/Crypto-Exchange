package auth

import (
	"net/http"

	"encrypted-db/internal/helpers"

	"github.com/gin-gonic/gin"
)

// JWTMiddleware validates tokens and checks if they are blacklisted.
func JWTMiddleware(tb *TokenBlacklist) gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenString := c.GetHeader("Authorization")
		if tokenString == "" {
			helpers.SendResponse(c, http.StatusUnauthorized, "Unauthorized - Token not provided", nil)
			c.Abort()
			return
		}

		claims, err := VerifyRSATokenWithKeys(tokenString)
		if err == nil && claims.Type != TokenTypeAccess {
			err = ErrWrongTokenType
		}
		if err != nil {
			helpers.SendResponse(c, http.StatusUnauthorized, "Unauthorized - Invalid token", nil)
			c.Abort()
			return
		}

		if tb != nil {
			isBlacklisted, err := tb.IsTokenBlacklisted(c.Request.Context(), tokenString)
			if err != nil {
				helpers.SendResponse(c, http.StatusInternalServerError, "Error checking blacklist", nil)
				c.Abort()
				return
			}
			if isBlacklisted {
				helpers.SendResponse(c, http.StatusUnauthorized, "Unauthorized - Token is blacklisted", nil)
				c.Abort()
				return
			}
		}

		c.Set("claims", claims)
		c.Next()
	}
}

// GetClaims extracts claims from the context.
func GetClaims(c *gin.Context) (*Claims, bool) {
	claims, exists := c.Get("claims")
	if !exists {
		return nil, false
	}
	casted, ok := claims.(*Claims)
	return casted, ok
}

// JWTAdminVerification verifies if the JWT token belongs to an admin with blacklist check.
func JWTAdminVerification(tb *TokenBlacklist) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.GetHeader("Authorization")
		if token == "" {
			helpers.SendResponse(c, http.StatusUnauthorized, "Unauthorized - Token not provided", nil)
			c.Abort()
			return
		}

		claims, err := VerifyAccessToken(token)
		if err != nil {
			helpers.SendResponse(c, http.StatusUnauthorized, "Unauthorized - Invalid token", nil)
			c.Abort()
			return
		}

		if claims.Role != "admin" {
			helpers.SendResponse(c, http.StatusForbidden, "Forbidden - Admin access required", nil)
			c.Abort()
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
				helpers.SendResponse(c, http.StatusUnauthorized, "Unauthorized - Token is blacklisted", nil)
				c.Abort()
				return
			}
		}

		c.Set("claims", claims)
		c.Next()
	}
}

// JWTUserVerification verifies if the JWT token belongs to a user with blacklist check.
func JWTUserVerification(tb *TokenBlacklist) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.GetHeader("Authorization")
		if token == "" {
			helpers.SendResponse(c, http.StatusUnauthorized, "Unauthorized - Token not provided", nil)
			c.Abort()
			return
		}

		claims, err := VerifyAccessToken(token)
		if err != nil {
			helpers.SendResponse(c, http.StatusUnauthorized, "Unauthorized - Invalid token", nil)
			c.Abort()
			return
		}

		if claims.Role != "user" && claims.Role != "admin" {
			helpers.SendResponse(c, http.StatusForbidden, "Forbidden - User access required", nil)
			c.Abort()
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
				helpers.SendResponse(c, http.StatusUnauthorized, "Unauthorized - Token is blacklisted", nil)
				c.Abort()
				return
			}
		}

		c.Set("claims", claims)
		c.Next()
	}
}
