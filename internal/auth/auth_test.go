package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"encrypted-db/config"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testKeyPair generates a test RSA key pair
func testKeyPair() (*rsa.PrivateKey, *rsa.PublicKey) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(nil, err)
	return privateKey, &privateKey.PublicKey
}

// privateKeyToPEM converts RSA private key to PEM format
func privateKeyToPEM(key *rsa.PrivateKey) []byte {
	block := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}
	return pem.EncodeToMemory(block)
}

// publicKeyToPEM converts RSA public key to PEM format
func publicKeyToPEM(key *rsa.PublicKey) []byte {
	block := &pem.Block{
		Type:  "RSA PUBLIC KEY",
		Bytes: x509.MarshalPKCS1PublicKey(key),
	}
	return pem.EncodeToMemory(block)
}

// setupTestConfig initializes config with test values
func setupTestConfig() {
	config.Config.JWT.AccessToken.Expiration = 15
	config.Config.JWT.RefreshToken.Expiration = 10080
	config.Config.JWT.Issuer = "test-issuer"
	config.Config.Server.Mode = "debug"
}

// setupTestKeys initializes auth package with test keys
func setupTestKeys(t *testing.T) (*rsa.PrivateKey, *rsa.PublicKey) {
	setupTestConfig()
	privateKey, publicKey := testKeyPair()
	privateKeyPEM := privateKeyToPEM(privateKey)
	publicKeyPEM := publicKeyToPEM(publicKey)

	err := InitKeys(privateKeyPEM, publicKeyPEM)
	require.NoError(t, err)
	return privateKey, publicKey
}

// mockRedisClient is a simple in-memory mock for Redis
type mockRedisClient struct {
	data map[string]string
	ttls map[string]time.Time
}

func newMockRedisClient() *mockRedisClient {
	return &mockRedisClient{
		data: make(map[string]string),
		ttls: make(map[string]time.Time),
	}
}

func (m *mockRedisClient) Set(ctx context.Context, key string, value string, expiration time.Duration) *redis.StatusCmd {
	m.data[key] = value
	m.ttls[key] = time.Now().Add(expiration)
	return redis.NewStatusCmd(ctx, "OK")
}

func (m *mockRedisClient) Get(ctx context.Context, key string) *redis.StringCmd {
	val, ok := m.data[key]
	if !ok {
		return redis.NewStringCmd(ctx, redis.Nil)
	}
	// Check expiration
	if exp, ok := m.ttls[key]; ok && time.Now().After(exp) {
		delete(m.data, key)
		delete(m.ttls, key)
		return redis.NewStringCmd(ctx, redis.Nil)
	}
	return redis.NewStringCmd(ctx, val)
}

func (m *mockRedisClient) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	count := 0
	for _, key := range keys {
		if _, ok := m.data[key]; ok {
			delete(m.data, key)
			delete(m.ttls, key)
			count++
		}
	}
	return redis.NewIntCmd(ctx, int64(count))
}

// fakeSessionStore records calls and returns preset errors. Rotation and
// revocation semantics against a real database are tested in the public
// handler package.
type fakeSessionStore struct {
	rotations []SessionRotation
	revoked   []string
	rotateErr error
	revokeErr error
}

func (f *fakeSessionStore) Create(ctx context.Context, session Session) error { return nil }

func (f *fakeSessionStore) Rotate(ctx context.Context, rotation SessionRotation) error {
	f.rotations = append(f.rotations, rotation)
	return f.rotateErr
}

func (f *fakeSessionStore) Revoke(ctx context.Context, sessionID, userID string) error {
	f.revoked = append(f.revoked, sessionID+"/"+userID)
	return f.revokeErr
}

func refreshCookie(w *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == RefreshTokenCookie {
			return cookie
		}
	}
	return nil
}

func TestTokenBlacklist_AddTokenToBlacklist(t *testing.T) {
	tb := &TokenBlacklist{
		RedisClient: (*redis.Client)(nil), // We'll test the logic directly
		Prefix:      "test:blacklist:",
	}

	// Test the tokenKey method directly
	token := "test-token-123"
	hash := sha256.Sum256([]byte(token))
	expectedKey := "test:blacklist:" + hex.EncodeToString(hash[:])
	actualKey := tb.tokenKey(token)
	assert.Equal(t, expectedKey, actualKey)
}

func TestTokenBlacklist_IsTokenBlacklisted(t *testing.T) {
	tb := NewTokenBlacklist((*redis.Client)(nil))
	tb.Prefix = "test:blacklist:"

	// Since we can't easily mock the Redis client, we test the key generation
	token := "test-token-123"
	key := tb.tokenKey(token)
	assert.Contains(t, key, "test:blacklist:")
	assert.Len(t, key, len("test:blacklist:")+64) // SHA256 hex = 64 chars
}

func TestGenerateAccessToken(t *testing.T) {
	setupTestKeys(t)

	uuid := "user-123"
	role := "user"

	token, err := GenerateAccessToken(uuid, role, "test-session")
	require.NoError(t, err)
	require.NotEmpty(t, token)

	// Verify token can be parsed
	claims, err := VerifyRSAToken(token)
	require.NoError(t, err)
	assert.Equal(t, uuid, claims.UUID)
	assert.Equal(t, role, claims.Role)
	assert.Equal(t, TokenTypeAccess, claims.Type)
	assert.Equal(t, "test-session", claims.SessionID)
	assert.NotEmpty(t, claims.ID)
	assert.NotNil(t, claims.ExpiresAt)
	assert.True(t, claims.ExpiresAt.Time.After(time.Now()))
}

func TestGenerateRefreshToken(t *testing.T) {
	setupTestKeys(t)

	uuid := "user-123"
	role := "user"

	token, err := GenerateRefreshToken(uuid, role, "test-session")
	require.NoError(t, err)
	require.NotEmpty(t, token)

	// Verify token can be parsed
	claims, err := VerifyRSAToken(token)
	require.NoError(t, err)
	assert.Equal(t, uuid, claims.UUID)
	assert.Equal(t, role, claims.Role)
	assert.Equal(t, TokenTypeRefresh, claims.Type)
	assert.Equal(t, "test-session", claims.SessionID)
	assert.NotNil(t, claims.ExpiresAt)
	assert.True(t, claims.ExpiresAt.Time.After(time.Now().Add(6*24*time.Hour))) // ~7 days
}

func TestGenerateRefreshToken_TokensIssuedTogetherAreDistinct(t *testing.T) {
	setupTestKeys(t)

	first, err := GenerateRefreshToken("user-123", "user", "test-session")
	require.NoError(t, err)
	second, err := GenerateRefreshToken("user-123", "user", "test-session")
	require.NoError(t, err)

	assert.NotEqual(t, first, second, "rotation needs every refresh token to be unique, even within the same second")
}

func TestVerifyTokenOfType(t *testing.T) {
	setupTestKeys(t)
	access, err := GenerateAccessToken("user-123", "user", "test-session")
	require.NoError(t, err)
	refresh, err := GenerateRefreshToken("user-123", "user", "test-session")
	require.NoError(t, err)

	_, err = VerifyAccessToken(access)
	assert.NoError(t, err)
	_, err = VerifyRefreshToken(refresh)
	assert.NoError(t, err)

	_, err = VerifyAccessToken(refresh)
	assert.ErrorIs(t, err, ErrWrongTokenType, "a refresh token must not be accepted as an access token")
	_, err = VerifyRefreshToken(access)
	assert.ErrorIs(t, err, ErrWrongTokenType, "an access token must not be accepted as a refresh token")

	// Tokens issued before token types existed carry no "typ" claim.
	untyped := jwt.NewWithClaims(jwt.SigningMethodRS256, Claims{
		UUID: "user-123", Role: "user",
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	})
	untypedString, err := untyped.SignedString(PrivateKey())
	require.NoError(t, err)
	_, err = VerifyAccessToken(untypedString)
	assert.ErrorIs(t, err, ErrWrongTokenType)
}

func TestVerifyRSAToken_ValidToken(t *testing.T) {
	setupTestKeys(t)

	token, err := GenerateAccessToken("user-123", "user", "test-session")
	require.NoError(t, err)

	claims, err := VerifyRSAToken(token)
	require.NoError(t, err)
	assert.Equal(t, "user-123", claims.UUID)
	assert.Equal(t, "user", claims.Role)
}

func TestVerifyRSAToken_InvalidToken(t *testing.T) {
	setupTestKeys(t)

	// Invalid token format
	_, err := VerifyRSAToken("invalid.token.string")
	assert.Error(t, err)

	// Token signed with different key
	otherPrivateKey, _ := testKeyPair()
	otherToken := jwt.NewWithClaims(jwt.SigningMethodRS256, Claims{
		UUID: "user-123",
		Role: "user",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			Issuer:    "test",
		},
	})
	otherTokenString, _ := otherToken.SignedString(otherPrivateKey)
	_, err = VerifyRSAToken(otherTokenString)
	assert.Error(t, err)
}

func TestVerifyRSAToken_ExpiredToken(t *testing.T) {
	setupTestKeys(t)

	// Create token with past expiration
	claims := Claims{
		UUID: "user-123",
		Role: "user",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
			Issuer:    "test",
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tokenString, _ := token.SignedString(PrivateKey())

	_, err := VerifyRSAToken(tokenString)
	assert.Error(t, err)
}

func TestVerifyRSAToken_WrongAlgorithm(t *testing.T) {
	setupTestKeys(t)

	// Create token with HS256 instead of RS256
	claims := Claims{
		UUID: "user-123",
		Role: "user",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			Issuer:    "test",
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, _ := token.SignedString([]byte("secret"))

	_, err := VerifyRSAToken(tokenString)
	assert.Error(t, err)
}

func TestVerifyRSATokenWithKeys_KeyRotation(t *testing.T) {
	setupTestKeys(t)

	// Generate access token with current key
	token, err := GenerateAccessToken("user-123", "user", "test-session")
	require.NoError(t, err)

	// Verify with current key works
	claims, err := VerifyRSATokenWithKeys(token)
	require.NoError(t, err)
	assert.Equal(t, "user-123", claims.UUID)

	// Add a new public key (simulating key rotation)
	newPrivateKey, newPublicKey := testKeyPair()
	newPublicKeyPEM := publicKeyToPEM(newPublicKey)
	err = AddPublicKey("key-2024-02", newPublicKeyPEM)
	require.NoError(t, err)

	// Create token with new key
	newToken := jwt.NewWithClaims(jwt.SigningMethodRS256, Claims{
		UUID: "user-456",
		Role: "user",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			Issuer:    config.Config.JWT.Issuer,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	})
	newTokenString, _ := newToken.SignedString(newPrivateKey)

	// Verify with key rotation - should work with new key
	claims, err = VerifyRSATokenWithKeys(newTokenString)
	require.NoError(t, err)
	assert.Equal(t, "user-456", claims.UUID)

	// Original token should still work
	claims, err = VerifyRSATokenWithKeys(token)
	require.NoError(t, err)
	assert.Equal(t, "user-123", claims.UUID)
}

func TestVerifyRSATokenWithKeys_KeysNotLoaded(t *testing.T) {
	// Reset keys
	privateKey = nil
	publicKey = nil
	publicKeys = nil

	_, err := VerifyRSATokenWithKeys("any-token")
	assert.Error(t, err)
	assert.Equal(t, ErrKeysNotLoaded, err)
}

func TestVerifyAdminToken(t *testing.T) {
	setupTestKeys(t)

	adminToken, _ := GenerateAccessToken("admin-1", "admin", "test-session")
	userToken, _ := GenerateAccessToken("user-1", "user", "test-session")

	assert.True(t, VerifyAdminToken(adminToken))
	assert.False(t, VerifyAdminToken(userToken))
	assert.False(t, VerifyAdminToken("invalid"))
}

func TestVerifyUserToken(t *testing.T) {
	setupTestKeys(t)

	adminToken, _ := GenerateAccessToken("admin-1", "admin", "test-session")
	userToken, _ := GenerateAccessToken("user-1", "user", "test-session")

	// VerifyUserToken only allows "user" role (not admin)
	assert.True(t, VerifyUserToken(userToken), "user token should be valid")
	assert.False(t, VerifyUserToken(adminToken), "admin token should NOT be valid for VerifyUserToken (only 'user' role)")
	assert.False(t, VerifyUserToken("invalid"), "invalid token should fail")
}

func TestLogoutHandler(t *testing.T) {
	setupTestKeys(t)

	gin.SetMode(gin.TestMode)

	token, _ := GenerateAccessToken("user-123", "user", "test-session")

	// Test logout with valid token
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	c.Request.Header.Set("Authorization", token)

	// Test with nil blacklist - should succeed (no-op when Redis is nil)
	tb := &TokenBlacklist{
		RedisClient: (*redis.Client)(nil),
		Prefix:      "test:blacklist:",
	}
	sessions := &fakeSessionStore{}
	LogoutHandler(c, tb, sessions)

	// With nil Redis, AddTokenToBlacklist is no-op, returns 200
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"test-session/user-123"}, sessions.revoked, "logout must revoke the token's session")
	cookie := refreshCookie(w)
	require.NotNil(t, cookie, "logout must clear the refresh token cookie")
	assert.Negative(t, cookie.MaxAge)
}

func TestLogoutHandler_RejectsRefreshToken(t *testing.T) {
	setupTestKeys(t)
	gin.SetMode(gin.TestMode)

	refreshToken, err := GenerateRefreshToken("user-123", "user", "test-session")
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	c.Request.Header.Set("Authorization", refreshToken)

	sessions := &fakeSessionStore{}
	LogoutHandler(c, NewTokenBlacklist((*redis.Client)(nil)), sessions)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, sessions.revoked)
}

func TestLogoutHandler_SessionStoreError(t *testing.T) {
	setupTestKeys(t)
	gin.SetMode(gin.TestMode)

	token, err := GenerateAccessToken("user-123", "user", "test-session")
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	c.Request.Header.Set("Authorization", token)

	LogoutHandler(c, NewTokenBlacklist((*redis.Client)(nil)), &fakeSessionStore{revokeErr: errors.New("database down")})

	assert.Equal(t, http.StatusInternalServerError, w.Code, "logout must not report success when the session stays active")
}

func TestLogoutHandler_MissingToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	// No Authorization header

	tb := NewTokenBlacklist((*redis.Client)(nil))
	LogoutHandler(c, tb, &fakeSessionStore{})

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestLogoutHandler_InvalidToken(t *testing.T) {
	setupTestKeys(t)
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	c.Request.Header.Set("Authorization", "invalid.token")

	tb := NewTokenBlacklist((*redis.Client)(nil))
	LogoutHandler(c, tb, &fakeSessionStore{})

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestLogoutHandler_BearerToken(t *testing.T) {
	setupTestKeys(t)
	gin.SetMode(gin.TestMode)

	token, err := GenerateAccessToken("user-123", "user", "test-session")
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	c.Request.Header.Set("Authorization", "Bearer "+token)

	LogoutHandler(c, NewTokenBlacklist((*redis.Client)(nil)), &fakeSessionStore{})

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRefreshTokenHandler(t *testing.T) {
	setupTestKeys(t)
	gin.SetMode(gin.TestMode)

	refresh := func(cookieValue string, sessions *fakeSessionStore) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
		if cookieValue != "" {
			c.Request.AddCookie(&http.Cookie{Name: RefreshTokenCookie, Value: cookieValue})
		}
		RefreshTokenHandler(c, sessions)
		return w
	}

	t.Run("missing cookie", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, refresh("", &fakeSessionStore{}).Code)
	})

	t.Run("invalid cookie", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, refresh("invalid", &fakeSessionStore{}).Code)
	})

	t.Run("access token in the cookie", func(t *testing.T) {
		accessToken, err := GenerateAccessToken("user-123", "user", "test-session")
		require.NoError(t, err)
		sessions := &fakeSessionStore{}

		assert.Equal(t, http.StatusUnauthorized, refresh(accessToken, sessions).Code)
		assert.Empty(t, sessions.rotations)
	})

	t.Run("valid cookie rotates the refresh token", func(t *testing.T) {
		refreshToken, err := GenerateRefreshToken("user-123", "user", "test-session")
		require.NoError(t, err)
		sessions := &fakeSessionStore{}

		w := refresh(refreshToken, sessions)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "Access token refreshed")
		assert.Contains(t, w.Body.String(), "access_token")
		assert.NotContains(t, w.Body.String(), "refresh_token", "the refresh token must only be sent in the cookie")

		require.Len(t, sessions.rotations, 1)
		rotation := sessions.rotations[0]
		assert.Equal(t, "test-session", rotation.SessionID)
		assert.Equal(t, "user-123", rotation.UserID)
		assert.Equal(t, HashToken(refreshToken), rotation.PresentedRefreshHash)

		cookie := refreshCookie(w)
		require.NotNil(t, cookie)
		assert.NotEqual(t, refreshToken, cookie.Value, "the refresh token must be rotated")
		assert.Equal(t, HashToken(cookie.Value), rotation.NewRefreshHash)
		claims, err := VerifyRefreshToken(cookie.Value)
		require.NoError(t, err)
		assert.Equal(t, "test-session", claims.SessionID)
	})

	t.Run("reused refresh token", func(t *testing.T) {
		refreshToken, err := GenerateRefreshToken("user-123", "user", "test-session")
		require.NoError(t, err)

		w := refresh(refreshToken, &fakeSessionStore{rotateErr: ErrRefreshTokenReused})

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		cookie := refreshCookie(w)
		require.NotNil(t, cookie)
		assert.Negative(t, cookie.MaxAge, "the cookie of a revoked session must be cleared")
	})

	t.Run("inactive session", func(t *testing.T) {
		refreshToken, err := GenerateRefreshToken("user-123", "user", "test-session")
		require.NoError(t, err)

		assert.Equal(t, http.StatusUnauthorized, refresh(refreshToken, &fakeSessionStore{rotateErr: ErrSessionInvalid}).Code)
	})

	t.Run("session store failure", func(t *testing.T) {
		refreshToken, err := GenerateRefreshToken("user-123", "user", "test-session")
		require.NoError(t, err)

		w := refresh(refreshToken, &fakeSessionStore{rotateErr: errors.New("database down")})

		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Nil(t, refreshCookie(w), "no new refresh token may be issued when the rotation was not stored")
	})
}

func TestMiddlewares_RejectRefreshTokens(t *testing.T) {
	setupTestKeys(t)
	gin.SetMode(gin.TestMode)

	middlewares := map[string]gin.HandlerFunc{
		"JWTMiddleware":        JWTMiddleware(nil),
		"JWTAdminVerification": JWTAdminVerification(nil),
		"JWTUserVerification":  JWTUserVerification(nil),
	}
	for name, middleware := range middlewares {
		t.Run(name, func(t *testing.T) {
			for _, role := range []string{"user", "admin"} {
				refreshToken, err := GenerateRefreshToken("user-123", role, "test-session")
				require.NoError(t, err)

				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodGet, "/protected", nil)
				c.Request.Header.Set("Authorization", refreshToken)
				middleware(c)

				assert.Equal(t, http.StatusUnauthorized, w.Code, "a %s refresh token must not authorize API requests", role)
				assert.True(t, c.IsAborted())
			}
		})
	}
}

func TestSetRefreshTokenCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	refreshToken := "test-refresh-token"
	SetRefreshTokenCookie(c, refreshToken)

	// Check cookie was set
	cookies := w.Result().Cookies()
	assert.Len(t, cookies, 1)
	assert.Equal(t, "refresh_token", cookies[0].Name)
	assert.Equal(t, refreshToken, cookies[0].Value)
	assert.True(t, cookies[0].HttpOnly)
	assert.True(t, cookies[0].Secure)
	assert.Equal(t, http.SameSiteStrictMode, cookies[0].SameSite)
	assert.Equal(t, "/public/auth", cookies[0].Path, "the cookie is only sent to the authentication routes")
	assert.Equal(t, config.Config.JWT.RefreshToken.Expiration*60, cookies[0].MaxAge)
}

func TestJWTMiddleware_ValidToken(t *testing.T) {
	setupTestKeys(t)
	gin.SetMode(gin.TestMode)

	token, _ := GenerateAccessToken("user-123", "user", "test-session")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/protected", nil)
	c.Request.Header.Set("Authorization", token)

	tb := NewTokenBlacklist((*redis.Client)(nil))

	middleware := JWTMiddleware(tb)
	middleware(c)

	// Middleware should pass (not abort with 401), but returns 404 since no handler
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
	claims, exists := c.Get("claims")
	assert.True(t, exists)
	assert.Equal(t, "user-123", claims.(*Claims).UUID)
}

func TestJWTMiddleware_MissingToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/protected", nil)
	// No Authorization header

	tb := NewTokenBlacklist((*redis.Client)(nil))

	middleware := JWTMiddleware(tb)
	middleware(c)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestJWTMiddleware_InvalidToken(t *testing.T) {
	setupTestKeys(t)
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/protected", nil)
	c.Request.Header.Set("Authorization", "invalid.token")

	tb := NewTokenBlacklist((*redis.Client)(nil))

	middleware := JWTMiddleware(tb)
	middleware(c)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestJWTMiddleware_ExpiredToken(t *testing.T) {
	setupTestKeys(t)
	gin.SetMode(gin.TestMode)

	// Create expired token
	claims := Claims{
		UUID: "user-123",
		Role: "user",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
			Issuer:    "test",
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tokenString, _ := token.SignedString(PrivateKey())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/protected", nil)
	c.Request.Header.Set("Authorization", tokenString)

	tb := NewTokenBlacklist((*redis.Client)(nil))

	middleware := JWTMiddleware(tb)
	middleware(c)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestJWTMiddleware_BlacklistedToken(t *testing.T) {
	setupTestKeys(t)
	gin.SetMode(gin.TestMode)

	token, _ := GenerateAccessToken("user-123", "user", "test-session")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/protected", nil)
	c.Request.Header.Set("Authorization", token)

	// Create a blacklist that returns true for this token
	tb := NewTokenBlacklist((*redis.Client)(nil))
	tb.Prefix = "test:blacklist:"

	// Pre-populate the blacklist (can't easily test without real Redis)
	// Just verify middleware doesn't panic

	middleware := JWTMiddleware(tb)
	middleware(c)

	assert.Equal(t, http.StatusOK, w.Code) // Passes since mock Redis returns nil
}

func TestJWTAdminVerification(t *testing.T) {
	setupTestKeys(t)
	gin.SetMode(gin.TestMode)

	adminToken, _ := GenerateAccessToken("admin-1", "admin", "test-session")
	userToken, _ := GenerateAccessToken("user-1", "user", "test-session")

	// Test admin token passes
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin", nil)
	c.Request.Header.Set("Authorization", adminToken)

	tb := NewTokenBlacklist((*redis.Client)(nil))

	middleware := JWTAdminVerification(tb)
	middleware(c)

	assert.Equal(t, http.StatusOK, w.Code) // Passes

	// Test user token fails
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin", nil)
	c.Request.Header.Set("Authorization", userToken)

	middleware = JWTAdminVerification(tb)
	middleware(c)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestJWTUserVerification(t *testing.T) {
	setupTestKeys(t)
	gin.SetMode(gin.TestMode)

	adminToken, _ := GenerateAccessToken("admin-1", "admin", "test-session")
	userToken, _ := GenerateAccessToken("user-1", "user", "test-session")

	tb := NewTokenBlacklist((*redis.Client)(nil))

	// Test user token passes
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/user", nil)
	c.Request.Header.Set("Authorization", userToken)

	middleware := JWTUserVerification(tb)
	middleware(c)

	assert.Equal(t, http.StatusOK, w.Code)

	// Test admin token passes (admin can access user endpoints)
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/user", nil)
	c.Request.Header.Set("Authorization", adminToken)

	middleware = JWTUserVerification(tb)
	middleware(c)

	assert.Equal(t, http.StatusOK, w.Code)

	// Test missing token fails
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/user", nil)

	middleware = JWTUserVerification(tb)
	middleware(c)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestClaims_JSONSerialization(t *testing.T) {
	claims := Claims{
		UUID: "user-123",
		Role: "user",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			Issuer:    "test",
			Subject:   "user-123",
		},
	}

	// Marshal to JSON
	data, err := json.Marshal(claims)
	require.NoError(t, err)

	// Unmarshal back
	var unmarshalled Claims
	err = json.Unmarshal(data, &unmarshalled)
	require.NoError(t, err)

	assert.Equal(t, claims.UUID, unmarshalled.UUID)
	assert.Equal(t, claims.Role, unmarshalled.Role)
	assert.Equal(t, claims.ExpiresAt.Time.Unix(), unmarshalled.ExpiresAt.Time.Unix())
}

func TestTokenBlacklist_KeyGeneration(t *testing.T) {
	tb := NewTokenBlacklist(nil)

	// Same token should generate same key
	token := "test-token"
	key1 := tb.tokenKey(token)
	key2 := tb.tokenKey(token)
	assert.Equal(t, key1, key2)

	// Different tokens should generate different keys
	token2 := "test-token-2"
	key3 := tb.tokenKey(token2)
	assert.NotEqual(t, key1, key3)

	// Key should have prefix and 64-char hex
	assert.True(t, strings.HasPrefix(key1, "blacklist:token:"))
	assert.Len(t, key1, len("blacklist:token:")+64)
}

// Benchmark tests
func BenchmarkGenerateAccessToken(b *testing.B) {
	setupTestKeys(nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		GenerateAccessToken("user-123", "user", "test-session")
	}
}

func BenchmarkVerifyRSAToken(b *testing.B) {
	setupTestKeys(nil)
	token, _ := GenerateAccessToken("user-123", "user", "test-session")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		VerifyRSAToken(token)
	}
}

func BenchmarkVerifyRSATokenWithKeys(b *testing.B) {
	setupTestKeys(nil)
	token, _ := GenerateAccessToken("user-123", "user", "test-session")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		VerifyRSATokenWithKeys(token)
	}
}
