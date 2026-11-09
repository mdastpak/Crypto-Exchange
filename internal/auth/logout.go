package auth

import (
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/gin-gonic/gin"
)

// LogoutHandler ends the session of the access token that RequireRole
// accepted, so it must run behind that middleware: it revokes the session, so
// its refresh token stops working, and blacklists the access token until it
// expires. The revocation records the user's logout (#51); administrators,
// who have no sessions, record theirs in the admin handler.
func LogoutHandler(c *gin.Context, tb *TokenBlacklist, sessions SessionStore) {
	claims, ok := GetClaims(c)
	tokenString, hasToken := accessToken(c)
	if !ok || !hasToken {
		// The route is not protected by RequireRole.
		unauthorized(c, "token_missing", i18n.M("UNAUTHORIZED_TOKEN_NOT_PROVIDED"))
		return
	}

	ctx := c.Request.Context()
	if claims.SessionID != "" && sessions != nil {
		event := SessionEvent(c, audit.ActionUserLogout, claims.UUID, claims.SessionID)
		if err := sessions.Revoke(ctx, claims.SessionID, claims.UUID, event); err != nil {
			log.Printf("Error revoking session %s: %v", claims.SessionID, err)
			helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LOGOUT"), nil)
			return
		}
	}

	// ParseToken requires an expiry, so ExpiresAt is always set.
	expiresAt := time.Now().Add(time.Hour)
	if claims.ExpiresAt != nil {
		expiresAt = claims.ExpiresAt.Time
	}

	if err := tb.AddTokenToBlacklist(ctx, tokenString, expiresAt); err != nil {
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LOGOUT"), nil)
		return
	}

	ClearRefreshTokenCookie(c)
	helpers.SendResponse(c, http.StatusOK, i18n.M("LOGGED_OUT_SUCCESSFULLY"), nil)
}
