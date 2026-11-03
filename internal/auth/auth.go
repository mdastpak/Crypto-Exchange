package auth

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"encrypted-db/config"
	"encrypted-db/internal/helpers"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var (
	ErrInvalidToken      = errors.New("invalid token")
	ErrTokenExpired      = errors.New("token expired")
	ErrKeysNotLoaded     = errors.New("RSA keys not loaded")
	ErrInvalidSigningAlg = errors.New("unexpected signing method")
	ErrWrongTokenType    = errors.New("wrong token type")
)

// Token types carried in the "typ" claim. Access tokens authorize API
// requests; refresh tokens can only be exchanged for new tokens.
const (
	TokenTypeAccess  = "access"
	TokenTypeRefresh = "refresh"
)

// RefreshTokenCookie is the HttpOnly cookie that carries the refresh token.
// The token is never returned in a response body.
const RefreshTokenCookie = "refresh_token"

// refreshTokenCookiePath limits the cookie to the routes that use it:
// /public/auth/refresh and /public/auth/logout.
const refreshTokenCookiePath = "/public/auth"

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

func accessTokenLifetime() time.Duration {
	return time.Duration(config.Config.JWT.AccessToken.Expiration) * time.Minute
}

// RefreshTokenLifetime is how long a refresh token, and the session it
// belongs to, stays valid without being rotated.
func RefreshTokenLifetime() time.Duration {
	return time.Duration(config.Config.JWT.RefreshToken.Expiration) * time.Minute
}

func generateToken(userID, role, sessionID, tokenType string, lifetime time.Duration) (string, error) {
	if !KeysLoaded() {
		return "", ErrKeysNotLoaded
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
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tokenString, err := token.SignedString(PrivateKey())
	if err != nil {
		log.Printf("Error signing %s token: %v", tokenType, err)
		return "", fmt.Errorf("failed to sign %s token: %w", tokenType, err)
	}

	return tokenString, nil
}

// VerifyRSAToken verifies the signature and expiry of a JWT, whatever its type.
// Use VerifyAccessToken or VerifyRefreshToken to also require a token type.
func VerifyRSAToken(tokenString string) (*Claims, error) {
	if !KeysLoaded() {
		return nil, ErrKeysNotLoaded
	}

	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, ErrInvalidSigningAlg
		}
		return PublicKey(), nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to parse token: %w", err)
	}

	if !token.Valid {
		return nil, ErrInvalidToken
	}

	return claims, nil
}

// VerifyAccessToken verifies a JWT and requires it to be an access token, so a
// refresh token cannot be used to call the API.
func VerifyAccessToken(tokenString string) (*Claims, error) {
	return verifyTokenOfType(tokenString, TokenTypeAccess)
}

// VerifyRefreshToken verifies a JWT and requires it to be a refresh token.
func VerifyRefreshToken(tokenString string) (*Claims, error) {
	return verifyTokenOfType(tokenString, TokenTypeRefresh)
}

func verifyTokenOfType(tokenString, tokenType string) (*Claims, error) {
	claims, err := VerifyRSAToken(tokenString)
	if err != nil {
		return nil, err
	}
	if claims.Type != tokenType {
		return nil, ErrWrongTokenType
	}
	return claims, nil
}

// RefreshTokenHandler exchanges the refresh token cookie for a new access token
// and rotates the refresh token. A refresh token can be used only once:
// presenting one that was already rotated revokes the whole session.
func RefreshTokenHandler(c *gin.Context, sessions SessionStore) {
	if !KeysLoaded() {
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to load RSA keys", nil)
		return
	}

	refreshToken, err := c.Cookie(RefreshTokenCookie)
	if err != nil || refreshToken == "" {
		helpers.SendResponse(c, http.StatusUnauthorized, "Refresh token not provided", nil)
		return
	}

	claims, err := VerifyRefreshToken(refreshToken)
	if err != nil || claims.SessionID == "" {
		ClearRefreshTokenCookie(c)
		helpers.SendResponse(c, http.StatusUnauthorized, "Invalid or expired refresh token", nil)
		return
	}

	accessToken, err := GenerateAccessToken(claims.UUID, claims.Role, claims.SessionID)
	if err != nil {
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to generate access token", nil)
		return
	}
	newRefreshToken, err := GenerateRefreshToken(claims.UUID, claims.Role, claims.SessionID)
	if err != nil {
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to generate refresh token", nil)
		return
	}

	err = sessions.Rotate(c.Request.Context(), SessionRotation{
		SessionID:            claims.SessionID,
		UserID:               claims.UUID,
		PresentedRefreshHash: HashToken(refreshToken),
		NewRefreshHash:       HashToken(newRefreshToken),
		NewAccessHash:        HashToken(accessToken),
		ExpiresAt:            time.Now().Add(RefreshTokenLifetime()),
	})
	if errors.Is(err, ErrRefreshTokenReused) {
		log.Printf("Refresh token reuse detected; session %s revoked", claims.SessionID)
	}
	if errors.Is(err, ErrSessionInvalid) {
		ClearRefreshTokenCookie(c)
		helpers.SendResponse(c, http.StatusUnauthorized, "Invalid or expired refresh token", nil)
		return
	}
	if err != nil {
		log.Printf("Error rotating session %s: %v", claims.SessionID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to refresh the session", nil)
		return
	}

	SetRefreshTokenCookie(c, newRefreshToken)
	helpers.SendResponse(c, http.StatusOK, "Access token refreshed", gin.H{"access_token": accessToken})
}

// SetRefreshTokenCookie stores the refresh token in an HttpOnly, Secure,
// SameSite=Strict cookie that is only sent to the authentication routes.
func SetRefreshTokenCookie(c *gin.Context, refreshToken string) {
	maxAge := int(RefreshTokenLifetime().Seconds())
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(RefreshTokenCookie, refreshToken, maxAge, refreshTokenCookiePath, "", true, true)
}

// ClearRefreshTokenCookie tells the client to delete the refresh token cookie.
func ClearRefreshTokenCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(RefreshTokenCookie, "", -1, refreshTokenCookiePath, "", true, true)
}

// VerifyAdminToken verifies if the token is a valid access token with admin role.
func VerifyAdminToken(tokenString string) bool {
	claims, err := VerifyAccessToken(tokenString)
	if err != nil {
		return false
	}
	return claims.Role == "admin"
}

// VerifyUserToken verifies if the token is a valid access token with user role.
func VerifyUserToken(tokenString string) bool {
	claims, err := VerifyAccessToken(tokenString)
	if err != nil {
		return false
	}
	return claims.Role == "user"
}
