package auth

import (
	"log"
	"net/http"
	"strings"
	"time"

	"encrypted-db/internal/helpers"

	"github.com/gin-gonic/gin"
)

// LogoutHandler ends the session of the access token in the Authorization
// header: it revokes the session, so its refresh token stops working, and
// blacklists the access token until it expires.
func LogoutHandler(c *gin.Context, tb *TokenBlacklist, sessions SessionStore) {
	tokenString := c.GetHeader("Authorization")
	if tokenString == "" {
		helpers.SendResponse(c, http.StatusUnauthorized, "Unauthorized - Token not provided", nil)
		return
	}
	if strings.HasPrefix(tokenString, "Bearer ") {
		tokenString = strings.TrimPrefix(tokenString, "Bearer ")
	}

	claims, err := VerifyAccessToken(tokenString)
	if err != nil {
		helpers.SendResponse(c, http.StatusUnauthorized, "Unauthorized - Invalid token", nil)
		return
	}

	ctx := c.Request.Context()
	if claims.SessionID != "" && sessions != nil {
		if err := sessions.Revoke(ctx, claims.SessionID, claims.UUID); err != nil {
			log.Printf("Error revoking session %s: %v", claims.SessionID, err)
			helpers.SendResponse(c, http.StatusInternalServerError, "Failed to logout", nil)
			return
		}
	}

	var expiresAt time.Time
	if claims.ExpiresAt != nil {
		expiresAt = claims.ExpiresAt.Time
	} else {
		expiresAt = time.Now().Add(24 * time.Hour)
	}

	if err := tb.AddTokenToBlacklist(ctx, tokenString, expiresAt); err != nil {
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to logout", nil)
		return
	}

	ClearRefreshTokenCookie(c)
	helpers.SendResponse(c, http.StatusOK, "Logged out successfully", nil)
}
