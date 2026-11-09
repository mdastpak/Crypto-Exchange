package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"

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

// BearerToken returns the token of an Authorization header of the Bearer
// scheme, for a route that takes a token without requiring one.
func BearerToken(header string) (string, bool) { return bearerToken(header) }

func unauthorized(c *gin.Context, code string, message i18n.Message) {
	c.Header("WWW-Authenticate", `Bearer`)
	helpers.SendError(c, http.StatusUnauthorized, code, message, nil)
}

// RequireRole authenticates a request with a Bearer access token of role and
// rejects blacklisted tokens. A token of another role fails verification,
// because each role has its own keys and audience. On success the claims and
// the token are available to handlers through GetClaims and accessToken.
func RequireRole(role string, tb *TokenBlacklist) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" {
			unauthorized(c, "token_missing", i18n.M("UNAUTHORIZED_TOKEN_NOT_PROVIDED"))
			return
		}
		token, ok := bearerToken(header)
		if !ok {
			unauthorized(c, helpers.CodeTokenInvalid, i18n.M("UNAUTHORIZED_BEARER_TOKEN_REQUIRED"))
			return
		}

		claims, err := ParseToken(token, role, TokenTypeAccess)
		if err != nil {
			unauthorized(c, helpers.CodeTokenInvalid, i18n.M("UNAUTHORIZED_INVALID_TOKEN"))
			return
		}

		if tb != nil {
			isBlacklisted, err := tb.IsTokenBlacklisted(c.Request.Context(), token)
			if err != nil {
				helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("ERROR_CHECKING_BLACKLIST"), nil)
				c.Abort()
				return
			}
			if isBlacklisted {
				unauthorized(c, "token_revoked", i18n.M("UNAUTHORIZED_TOKEN_IS_BLACKLISTED"))
				return
			}
		}

		SetClaims(c, claims)
		c.Set(accessTokenContextKey, token)
		c.Next()
	}
}

// accountUnavailableMessage answers a token whose user can no longer sign in.
// The profile routes give the same answer.
var accountUnavailableMessage = i18n.M("ACCOUNT_UNAVAILABLE")

// AccountError returns the code and message of an ErrAccountUnavailable,
// which is account_frozen for a frozen account.
func AccountError(err error) (code string, message i18n.Message) {
	if errors.Is(err, ErrAccountFrozen) {
		return restrictions.CodeAccountFrozen, restrictions.MessageAccountFrozen
	}
	return helpers.CodeAccountUnavailable, accountUnavailableMessage
}

// RequireActiveSession runs after JWTUserVerification and checks the token's
// session and user in the database on every request. A token stays valid only
// while its session is active and its user can still sign in, so logging out,
// refresh token reuse, and disabling, suspending, or deleting the user reject
// every access token of the session at once, instead of when the tokens
// expire. Like RequireActiveAdmin, it fails closed without a session store.
func RequireActiveSession(sessions SessionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := GetClaims(c)
		if !ok || claims.SessionID == "" || sessions == nil {
			unauthorized(c, helpers.CodeTokenInvalid, i18n.M("UNAUTHORIZED_INVALID_TOKEN"))
			return
		}
		var locale string
		var err error
		if v, ok := sessions.(LocaleVerifier); ok {
			locale, err = v.VerifyLocale(c.Request.Context(), claims.SessionID, claims.UUID)
		} else {
			err = sessions.Verify(c.Request.Context(), claims.SessionID, claims.UUID)
		}
		switch {
		case err == nil:
			// The user's saved locale applies from here on (#286).
			i18n.Prefer(c, locale)
			c.Next()
		case errors.Is(err, ErrSessionInvalid):
			unauthorized(c, reasonSessionInactive, i18n.M("UNAUTHORIZED_SESSION_IS_NOT_ACTIVE"))
		case errors.Is(err, ErrAccountUnavailable):
			code, message := AccountError(err)
			helpers.SendError(c, http.StatusForbidden, code, message, nil)
		default:
			log.Printf("Failed to verify session %s: %v", claims.SessionID, err)
			helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_VERIFY_THE_SESSION"), nil)
			c.Abort()
		}
	}
}

// AuthenticateUserToken checks a user access token as JWTUserVerification
// and RequireActiveSession do together, for callers outside an HTTP route,
// such as the WebSocket gateway. It returns ErrInvalidToken for a token that
// does not verify or is blacklisted, ErrSessionInvalid for an ended session,
// and ErrAccountUnavailable for a user who can no longer sign in.
func AuthenticateUserToken(ctx context.Context, token string, tb *TokenBlacklist, sessions SessionStore) (*Claims, error) {
	claims, err := ParseToken(token, RoleUser, TokenTypeAccess)
	if err != nil {
		return nil, err
	}
	if tb != nil {
		blacklisted, err := tb.IsTokenBlacklisted(ctx, token)
		if err != nil {
			return nil, fmt.Errorf("checking the blacklist: %w", err)
		}
		if blacklisted {
			return nil, ErrInvalidToken
		}
	}
	if claims.SessionID == "" || sessions == nil {
		return nil, ErrInvalidToken
	}
	if err := sessions.Verify(ctx, claims.SessionID, claims.UUID); err != nil {
		return nil, err
	}
	return claims, nil
}

// JWTAdminVerification accepts only admin access tokens.
func JWTAdminVerification(tb *TokenBlacklist) gin.HandlerFunc {
	return RequireRole(RoleAdmin, tb)
}

// JWTUserVerification accepts only user access tokens.
func JWTUserVerification(tb *TokenBlacklist) gin.HandlerFunc {
	return RequireRole(RoleUser, tb)
}

// SetClaims stores the claims of an accepted token, which GetClaims returns.
// RequireRole calls it, and so may tests of handlers that run without it.
func SetClaims(c *gin.Context, claims *Claims) {
	c.Set(claimsContextKey, claims)
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
