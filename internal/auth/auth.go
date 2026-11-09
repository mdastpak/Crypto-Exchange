package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var (
	ErrInvalidToken   = errors.New("invalid token")
	ErrKeysNotLoaded  = errors.New("RSA keys not loaded")
	ErrUnknownRole    = errors.New("unknown role")
	ErrUnknownKeyID   = errors.New("unknown or missing key ID")
	ErrWrongTokenType = errors.New("wrong token type")
	ErrWrongRole      = errors.New("wrong role")
)

// signingMethod is the only accepted algorithm. Accepting any RS* or other
// algorithm named by the token itself invites algorithm confusion.
var signingMethod = jwt.SigningMethodRS256

// Token types carried in the "typ" claim. Access tokens authorize API
// requests; refresh tokens can only be exchanged for new tokens.
const (
	TokenTypeAccess  = "access"
	TokenTypeRefresh = "refresh"
)

// RefreshTokenCookie is the HttpOnly cookie that carries the refresh token.
// The token is never returned in a response body.
const RefreshTokenCookie = "refresh_token"

// legacyRefreshTokenCookiePath limits the cookie to the routes that use it,
// /public/auth/refresh and /public/auth/logout, under helpers.APIPrefix or,
// for their deprecated aliases, without it.
const legacyRefreshTokenCookiePath = "/public/auth" //nolint:gosec // G101: a URL path, not a credential.

// refreshTokenCookiePath is the path of the cookie for a response to c: the
// auth routes of the API version of the request, so that the cookie is sent
// to the routes that issued it and to no others.
func refreshTokenCookiePath(c *gin.Context) string {
	if c.Request != nil && strings.HasPrefix(c.Request.URL.Path, helpers.APIPrefix+"/") {
		return helpers.APIPrefix + legacyRefreshTokenCookiePath
	}
	return legacyRefreshTokenCookiePath
}

// Claims represents the JWT claims structure.
type Claims struct {
	UUID      string `json:"uuid"`
	Role      string `json:"role"`
	Type      string `json:"typ"`
	SessionID string `json:"sid,omitempty"`
	jwt.RegisteredClaims
}

// GenerateAccessToken generates a short-lived access token for a session.
func GenerateAccessToken(userID, role, sessionID string) (string, error) {
	return generateToken(userID, role, sessionID, TokenTypeAccess, accessTokenLifetime())
}

// GenerateRefreshToken generates a long-lived refresh token for a session.
func GenerateRefreshToken(userID, role, sessionID string) (string, error) {
	return generateToken(userID, role, sessionID, TokenTypeRefresh, RefreshTokenLifetime())
}

// refreshReuseGrace is how long the previous refresh token of a session is
// accepted after a rotation (#194).
func refreshReuseGrace() time.Duration {
	seconds := config.Config.JWT.RefreshToken.ReuseGraceSeconds
	if seconds <= 0 {
		seconds = config.DefaultRefreshReuseGraceSeconds
	}
	return time.Duration(seconds) * time.Second
}

func accessTokenLifetime() time.Duration {
	return time.Duration(config.Config.JWT.AccessToken.Expiration) * time.Minute
}

// RefreshTokenLifetime is how long a refresh token, and the session it
// belongs to, stays valid without being rotated.
func RefreshTokenLifetime() time.Duration {
	return time.Duration(config.Config.JWT.RefreshToken.Expiration) * time.Minute
}

// generateToken signs a token with the key of role. The "kid" header names
// that key, and the audience names the role's API.
func generateToken(userID, role, sessionID, tokenType string, lifetime time.Duration) (string, error) {
	keys, err := keysFor(role)
	if err != nil {
		return "", err
	}

	now := time.Now()
	claims := Claims{
		UUID:      userID,
		Role:      role,
		Type:      tokenType,
		SessionID: sessionID,
		RegisteredClaims: jwt.RegisteredClaims{
			// A unique ID keeps two tokens issued in the same second distinct,
			// which refresh token rotation relies on.
			ID:        uuid.NewString(),
			ExpiresAt: jwt.NewNumericDate(now.Add(lifetime)),
			Issuer:    config.Config.JWT.Issuer,
			Audience:  jwt.ClaimStrings{audienceFor(role)},
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}

	token := jwt.NewWithClaims(signingMethod, claims)
	token.Header["kid"] = keys.signingKID
	tokenString, err := token.SignedString(keys.signing)
	if err != nil {
		log.Printf("Error signing %s token: %v", tokenType, err)
		return "", fmt.Errorf("failed to sign %s token: %w", tokenType, err)
	}

	return tokenString, nil
}

// ParseToken verifies tokenString and returns its claims. The token must be
// signed with RS256 by a key of role, named by its "kid" header, and must
// carry the configured issuer, the audience of role, an expiry in the
// future, the role itself, and tokenType, so that a refresh token cannot call
// the API and a user token cannot reach admin routes.
func ParseToken(tokenString, role, tokenType string) (*Claims, error) {
	keys, err := keysFor(role)
	if err != nil {
		return nil, err
	}

	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{signingMethod.Alg()}),
		jwt.WithIssuer(config.Config.JWT.Issuer),
		jwt.WithAudience(audienceFor(role)),
		jwt.WithExpirationRequired(),
	)
	claims := &Claims{}
	token, err := parser.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		kid, _ := token.Header["kid"].(string)
		key, ok := keys.verifying[kid]
		if !ok {
			return nil, ErrUnknownKeyID
		}
		return key, nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if !token.Valid {
		return nil, ErrInvalidToken
	}
	if claims.Type != tokenType {
		return nil, ErrWrongTokenType
	}
	if claims.Role != role {
		return nil, ErrWrongRole
	}
	return claims, nil
}

// RefreshTokenHandler exchanges the refresh token cookie for a new access token
// and rotates the refresh token. A refresh token can be used only once:
// presenting one that was already rotated revokes the whole session. A user
// who can no longer sign in gets 403 and no new tokens.
func RefreshTokenHandler(c *gin.Context, sessions SessionStore) {
	if !KeysLoaded(RoleUser) {
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LOAD_RSA_KEYS"), nil)
		return
	}

	refreshToken, err := c.Cookie(RefreshTokenCookie)
	if err != nil || refreshToken == "" {
		helpers.SendError(c, http.StatusUnauthorized, "refresh_token_missing", i18n.M("REFRESH_TOKEN_NOT_PROVIDED"), nil)
		return
	}

	// Only users have refresh tokens and sessions.
	claims, err := ParseToken(refreshToken, RoleUser, TokenTypeRefresh)
	if err != nil || claims.SessionID == "" {
		ClearRefreshTokenCookie(c)
		helpers.SendError(c, http.StatusUnauthorized, "refresh_token_invalid", i18n.M("INVALID_OR_EXPIRED_REFRESH_TOKEN"), nil)
		return
	}

	// refused records a refused refresh of a genuine token, at best effort:
	// it changes nothing, so the answer does not wait on the audit log. A
	// token that does not parse has no user to record. A client that
	// disconnects does not cancel the record.
	refused := func(reason string) {
		e := SessionEvent(c, audit.ActionUserRefresh, claims.UUID, claims.SessionID)
		e.Outcome, e.Reason = audit.Failure, reason
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
		defer cancel()
		if err := sessions.Note(ctx, e); err != nil {
			log.Printf("Failed to record the audit event %s (%s): %v", e.Action, reason, err)
		}
	}

	// No new tokens for a user who can no longer sign in. Rotate checks the
	// session again, atomically, and detects reuse.
	switch err := sessions.Verify(c.Request.Context(), claims.SessionID, claims.UUID); {
	case errors.Is(err, ErrSessionInvalid):
		refused(reasonSessionInactive)
		ClearRefreshTokenCookie(c)
		helpers.SendError(c, http.StatusUnauthorized, "refresh_token_invalid", i18n.M("INVALID_OR_EXPIRED_REFRESH_TOKEN"), nil)
		return
	case errors.Is(err, ErrAccountUnavailable):
		code, message := AccountError(err)
		refused(code)
		ClearRefreshTokenCookie(c)
		helpers.SendError(c, http.StatusForbidden, code, message, nil)
		return
	case err != nil:
		log.Printf("Error verifying session %s: %v", claims.SessionID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_REFRESH_SESSION"), nil)
		return
	}

	accessToken, err := GenerateAccessToken(claims.UUID, claims.Role, claims.SessionID)
	if err != nil {
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_GENERATE_ACCESS_TOKEN"), nil)
		return
	}
	newRefreshToken, err := GenerateRefreshToken(claims.UUID, claims.Role, claims.SessionID)
	if err != nil {
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_GENERATE_REFRESH_TOKEN"), nil)
		return
	}

	err = sessions.Rotate(c.Request.Context(), SessionRotation{
		SessionID:            claims.SessionID,
		UserID:               claims.UUID,
		PresentedRefreshHash: HashToken(refreshToken),
		NewRefreshHash:       HashToken(newRefreshToken),
		NewAccessHash:        HashToken(accessToken),
		ExpiresAt:            time.Now().Add(RefreshTokenLifetime()),
		Grace:                refreshReuseGrace(),
	}, SessionEvent(c, audit.ActionUserRefresh, claims.UUID, claims.SessionID))
	if errors.Is(err, ErrConcurrentRefresh) {
		// Another request rotated the token a moment ago and set the cookie
		// to the new one, which this answer leaves as it is.
		helpers.SendResponse(c, http.StatusOK, i18n.M("ACCESS_TOKEN_REFRESHED"), gin.H{"access_token": accessToken})
		return
	}
	if errors.Is(err, ErrRefreshTokenReused) {
		// Rotate recorded the revocation.
		log.Printf("Refresh token reuse detected; session %s revoked", claims.SessionID)
	} else if errors.Is(err, ErrSessionInvalid) {
		refused(reasonSessionInactive)
	}
	if errors.Is(err, ErrSessionInvalid) {
		ClearRefreshTokenCookie(c)
		helpers.SendError(c, http.StatusUnauthorized, "refresh_token_invalid", i18n.M("INVALID_OR_EXPIRED_REFRESH_TOKEN"), nil)
		return
	}
	if err != nil {
		log.Printf("Error rotating session %s: %v", claims.SessionID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_REFRESH_SESSION"), nil)
		return
	}

	SetRefreshTokenCookie(c, newRefreshToken)
	helpers.SendResponse(c, http.StatusOK, i18n.M("ACCESS_TOKEN_REFRESHED"), gin.H{"access_token": accessToken})
}

// SetRefreshTokenCookie stores the refresh token in an HttpOnly, Secure,
// SameSite=Strict cookie that is only sent to the authentication routes.
func SetRefreshTokenCookie(c *gin.Context, refreshToken string) {
	maxAge := int(RefreshTokenLifetime().Seconds())
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(RefreshTokenCookie, refreshToken, maxAge, refreshTokenCookiePath(c), "", true, true)
}

// ClearRefreshTokenCookie tells the client to delete the refresh token
// cookie, under both paths, so that a logout through either version also
// ends a cookie that the other one set.
func ClearRefreshTokenCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteStrictMode)
	for _, path := range []string{helpers.APIPrefix + legacyRefreshTokenCookiePath, legacyRefreshTokenCookiePath} {
		c.SetCookie(RefreshTokenCookie, "", -1, path, "", true, true)
	}
}
