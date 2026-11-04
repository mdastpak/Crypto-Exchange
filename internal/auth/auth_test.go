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
	"sync"
	"testing"
	"time"

	"encrypted-db/config"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Generating RSA keys is slow, so the tests share a few key pairs.
var (
	keysOnce                                sync.Once
	userTestKey, adminTestKey, otherTestKey *rsa.PrivateKey
	retiredTestKey                          *rsa.PrivateKey
)

func testKeys(t testing.TB) {
	keysOnce.Do(func() {
		for _, key := range []**rsa.PrivateKey{&userTestKey, &adminTestKey, &otherTestKey, &retiredTestKey} {
			generated, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			*key = generated
		}
	})
}

func privateKeyToPEM(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func publicKeyToPEM(key *rsa.PublicKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(key)})
}

func pairOf(key *rsa.PrivateKey, retired ...*rsa.PrivateKey) KeyPair {
	pair := KeyPair{PrivateKey: privateKeyToPEM(key), PublicKey: publicKeyToPEM(&key.PublicKey)}
	for _, r := range retired {
		pair.Retired = append(pair.Retired, publicKeyToPEM(&r.PublicKey))
	}
	return pair
}

func setupTestConfig() {
	config.Config.JWT.AccessToken.Expiration = 15
	config.Config.JWT.RefreshToken.Expiration = 10080
	config.Config.JWT.Issuer = "test-issuer"
	config.Config.Server.Mode = "debug"
}

// setupTestKeys installs separate user and admin keys.
func setupTestKeys(t testing.TB) {
	setupTestConfig()
	testKeys(t)
	require.NoError(t, SetKeys(map[string]KeyPair{
		RoleUser:  pairOf(userTestKey),
		RoleAdmin: pairOf(adminTestKey),
	}))
}

func kidOf(t testing.TB, key *rsa.PrivateKey) string {
	kid, err := KeyID(&key.PublicKey)
	require.NoError(t, err)
	return kid
}

// validClaims returns the claims the server issues for role and tokenType.
func validClaims(role, tokenType string) Claims {
	now := time.Now()
	return Claims{
		UUID:      "user-123",
		Role:      role,
		Type:      tokenType,
		SessionID: "test-session",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    config.Config.JWT.Issuer,
			Audience:  jwt.ClaimStrings{audienceFor(role)},
		},
	}
}

// sign creates a token with any key, algorithm, kid, and claims, to build
// tokens the server would never issue.
func sign(t testing.TB, method jwt.SigningMethod, key interface{}, kid string, claims Claims) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	if kid != "" {
		token.Header["kid"] = kid
	}
	signed, err := token.SignedString(key)
	require.NoError(t, err)
	return signed
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

// serve sends a request with the Authorization header to a router that
// protects /protected with middleware and returns the recorder.
func serve(middleware gin.HandlerFunc, authorization string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	if handler == nil {
		handler = func(c *gin.Context) { c.Status(http.StatusOK) }
	}
	router.POST("/protected", middleware, handler)
	req := httptest.NewRequest(http.MethodPost, "/protected", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestGenerateAccessToken(t *testing.T) {
	setupTestKeys(t)

	token, err := GenerateAccessToken("user-123", RoleUser, "test-session")
	require.NoError(t, err)

	claims, err := ParseToken(token, RoleUser, TokenTypeAccess)
	require.NoError(t, err)
	assert.Equal(t, "user-123", claims.UUID)
	assert.Equal(t, RoleUser, claims.Role)
	assert.Equal(t, TokenTypeAccess, claims.Type)
	assert.Equal(t, "test-session", claims.SessionID)
	assert.Equal(t, "test-issuer", claims.Issuer)
	assert.Equal(t, jwt.ClaimStrings{"user-api"}, claims.Audience)
	assert.NotEmpty(t, claims.ID)
	assert.True(t, claims.ExpiresAt.Time.After(time.Now()))

	parsed, _, err := jwt.NewParser().ParseUnverified(token, &Claims{})
	require.NoError(t, err)
	assert.Equal(t, kidOf(t, userTestKey), parsed.Header["kid"], "tokens name their signing key")
	assert.Equal(t, "RS256", parsed.Header["alg"])
}

func TestGenerateToken_UsesTheKeyOfTheRole(t *testing.T) {
	setupTestKeys(t)

	admin, err := GenerateAccessToken("admin-1", RoleAdmin, "")
	require.NoError(t, err)
	parsed, _, err := jwt.NewParser().ParseUnverified(admin, &Claims{})
	require.NoError(t, err)
	assert.Equal(t, kidOf(t, adminTestKey), parsed.Header["kid"])

	_, err = GenerateAccessToken("user-1", "superuser", "")
	assert.ErrorIs(t, err, ErrUnknownRole)

	require.NoError(t, InitKeys(privateKeyToPEM(userTestKey), publicKeyToPEM(&userTestKey.PublicKey)))
	_, err = GenerateAccessToken("admin-1", RoleAdmin, "")
	assert.ErrorIs(t, err, ErrKeysNotLoaded, "without admin keys no admin token can be issued")
}

func TestGenerateRefreshToken(t *testing.T) {
	setupTestKeys(t)

	token, err := GenerateRefreshToken("user-123", RoleUser, "test-session")
	require.NoError(t, err)

	claims, err := ParseToken(token, RoleUser, TokenTypeRefresh)
	require.NoError(t, err)
	assert.Equal(t, TokenTypeRefresh, claims.Type)
	assert.True(t, claims.ExpiresAt.Time.After(time.Now().Add(6*24*time.Hour)))
}

func TestGenerateRefreshToken_TokensIssuedTogetherAreDistinct(t *testing.T) {
	setupTestKeys(t)

	first, err := GenerateRefreshToken("user-123", RoleUser, "test-session")
	require.NoError(t, err)
	second, err := GenerateRefreshToken("user-123", RoleUser, "test-session")
	require.NoError(t, err)

	assert.NotEqual(t, first, second, "rotation needs every refresh token to be unique, even within the same second")
}

func TestParseToken_AcceptsServerTokens(t *testing.T) {
	setupTestKeys(t)
	testKeys(t)

	for _, role := range []string{RoleUser, RoleAdmin} {
		key := map[string]*rsa.PrivateKey{RoleUser: userTestKey, RoleAdmin: adminTestKey}[role]
		token := sign(t, jwt.SigningMethodRS256, key, kidOf(t, key), validClaims(role, TokenTypeAccess))
		_, err := ParseToken(token, role, TokenTypeAccess)
		assert.NoError(t, err, role)
	}
}

// TestParseToken_RejectsInvalidTokens covers each check of ParseToken with a
// token that is valid except for one property.
func TestParseToken_RejectsInvalidTokens(t *testing.T) {
	setupTestKeys(t)
	userKID := kidOf(t, userTestKey)
	adminKID := kidOf(t, adminTestKey)

	mutated := func(mutate func(*Claims)) Claims {
		claims := validClaims(RoleUser, TokenTypeAccess)
		mutate(&claims)
		return claims
	}

	cases := map[string]string{
		"wrong issuer": sign(t, jwt.SigningMethodRS256, userTestKey, userKID,
			mutated(func(c *Claims) { c.Issuer = "someone-else" })),
		"missing issuer": sign(t, jwt.SigningMethodRS256, userTestKey, userKID,
			mutated(func(c *Claims) { c.Issuer = "" })),
		"wrong audience": sign(t, jwt.SigningMethodRS256, userTestKey, userKID,
			mutated(func(c *Claims) { c.Audience = jwt.ClaimStrings{"admin-api"} })),
		"missing audience": sign(t, jwt.SigningMethodRS256, userTestKey, userKID,
			mutated(func(c *Claims) { c.Audience = nil })),
		"expired": sign(t, jwt.SigningMethodRS256, userTestKey, userKID,
			mutated(func(c *Claims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute)) })),
		"no expiry": sign(t, jwt.SigningMethodRS256, userTestKey, userKID,
			mutated(func(c *Claims) { c.ExpiresAt = nil })),
		"RS512 instead of RS256": sign(t, jwt.SigningMethodRS512, userTestKey, userKID,
			validClaims(RoleUser, TokenTypeAccess)),
		"HS256 keyed with the public key": sign(t, jwt.SigningMethodHS256, publicKeyToPEM(&userTestKey.PublicKey), userKID,
			validClaims(RoleUser, TokenTypeAccess)),
		"missing kid": sign(t, jwt.SigningMethodRS256, userTestKey, "",
			validClaims(RoleUser, TokenTypeAccess)),
		"unknown kid": sign(t, jwt.SigningMethodRS256, userTestKey, "not-a-key",
			validClaims(RoleUser, TokenTypeAccess)),
		"signed by an unknown key": sign(t, jwt.SigningMethodRS256, otherTestKey, kidOf(t, otherTestKey),
			validClaims(RoleUser, TokenTypeAccess)),
		"other key under the user kid": sign(t, jwt.SigningMethodRS256, otherTestKey, userKID,
			validClaims(RoleUser, TokenTypeAccess)),
		"admin token": sign(t, jwt.SigningMethodRS256, adminTestKey, adminKID,
			validClaims(RoleAdmin, TokenTypeAccess)),
		"refresh token": sign(t, jwt.SigningMethodRS256, userTestKey, userKID,
			validClaims(RoleUser, TokenTypeRefresh)),
		"admin role claim with the user key": sign(t, jwt.SigningMethodRS256, userTestKey, userKID,
			mutated(func(c *Claims) { c.Role = RoleAdmin })),
		"malformed": "not.a.token",
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseToken(token, RoleUser, TokenTypeAccess)
			assert.Error(t, err)
		})
	}
}

func TestParseToken_ReportsTheReason(t *testing.T) {
	setupTestKeys(t)
	userKID := kidOf(t, userTestKey)

	refresh := sign(t, jwt.SigningMethodRS256, userTestKey, userKID, validClaims(RoleUser, TokenTypeRefresh))
	_, err := ParseToken(refresh, RoleUser, TokenTypeAccess)
	assert.ErrorIs(t, err, ErrWrongTokenType)

	forged := validClaims(RoleUser, TokenTypeAccess)
	forged.Role = RoleAdmin
	_, err = ParseToken(sign(t, jwt.SigningMethodRS256, userTestKey, userKID, forged), RoleUser, TokenTypeAccess)
	assert.ErrorIs(t, err, ErrWrongRole)

	_, err = ParseToken(sign(t, jwt.SigningMethodRS256, userTestKey, "", validClaims(RoleUser, TokenTypeAccess)), RoleUser, TokenTypeAccess)
	assert.ErrorIs(t, err, ErrUnknownKeyID)
	assert.ErrorIs(t, err, ErrInvalidToken)

	_, err = ParseToken("x", "superuser", TokenTypeAccess)
	assert.ErrorIs(t, err, ErrUnknownRole)
}

// TestParseToken_AdminRejectsUserKeys checks that no token signed with a user
// key reaches admin routes, even with forged admin claims.
func TestParseToken_AdminRejectsUserKeys(t *testing.T) {
	setupTestKeys(t)
	userKID := kidOf(t, userTestKey)
	adminKID := kidOf(t, adminTestKey)
	forged := validClaims(RoleAdmin, TokenTypeAccess)

	for name, token := range map[string]string{
		"user token":                     mustToken(t, RoleUser),
		"forged admin claims, user kid":  sign(t, jwt.SigningMethodRS256, userTestKey, userKID, forged),
		"forged admin claims, admin kid": sign(t, jwt.SigningMethodRS256, userTestKey, adminKID, forged),
		"forged admin claims, no kid":    sign(t, jwt.SigningMethodRS256, userTestKey, "", forged),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseToken(token, RoleAdmin, TokenTypeAccess)
			assert.Error(t, err)
		})
	}
}

func mustToken(t testing.TB, role string) string {
	token, err := GenerateAccessToken("user-123", role, "test-session")
	require.NoError(t, err)
	return token
}

func TestParseToken_AcceptsRetiredKeysUntilTheyAreRemoved(t *testing.T) {
	setupTestConfig()
	testKeys(t)
	require.NoError(t, SetKeys(map[string]KeyPair{RoleUser: pairOf(retiredTestKey)}))
	issuedBeforeRotation := mustToken(t, RoleUser)

	require.NoError(t, SetKeys(map[string]KeyPair{RoleUser: pairOf(userTestKey, retiredTestKey)}))
	_, err := ParseToken(issuedBeforeRotation, RoleUser, TokenTypeAccess)
	assert.NoError(t, err, "a token signed with a retired key stays valid")
	_, err = ParseToken(mustToken(t, RoleUser), RoleUser, TokenTypeAccess)
	assert.NoError(t, err, "new tokens are signed with the new key")

	require.NoError(t, SetKeys(map[string]KeyPair{RoleUser: pairOf(userTestKey)}))
	_, err = ParseToken(issuedBeforeRotation, RoleUser, TokenTypeAccess)
	assert.ErrorIs(t, err, ErrUnknownKeyID, "a removed key no longer verifies tokens")
}

func TestSetKeys_RejectsInvalidKeys(t *testing.T) {
	setupTestConfig()
	testKeys(t)
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)

	cases := map[string]map[string]KeyPair{
		"mismatched pair": {RoleUser: {PrivateKey: privateKeyToPEM(userTestKey), PublicKey: publicKeyToPEM(&otherTestKey.PublicKey)}},
		"shared key":      {RoleUser: pairOf(userTestKey), RoleAdmin: pairOf(userTestKey)},
		"retired key of another role": {
			RoleUser:  pairOf(userTestKey),
			RoleAdmin: pairOf(adminTestKey, userTestKey),
		},
		"1024-bit key":        {RoleUser: pairOf(small)},
		"unknown role":        {"superuser": pairOf(userTestKey)},
		"invalid PEM":         {RoleUser: {PrivateKey: []byte("x"), PublicKey: []byte("y")}},
		"invalid retired key": {RoleUser: {PrivateKey: privateKeyToPEM(userTestKey), PublicKey: publicKeyToPEM(&userTestKey.PublicKey), Retired: [][]byte{[]byte("x")}}},
	}
	for name, pairs := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, SetKeys(pairs))
		})
	}
}

func TestSetKeys_ReplacesKeysAtomically(t *testing.T) {
	setupTestKeys(t)
	token := mustToken(t, RoleUser)

	// Readers and a writer run together; the race detector reports any
	// unsynchronized access to the key state.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := ParseToken(token, RoleUser, TokenTypeAccess); err != nil {
					t.Errorf("ParseToken: %v", err)
					return
				}
				if _, err := GenerateAccessToken("user-123", RoleUser, ""); err != nil {
					t.Errorf("GenerateAccessToken: %v", err)
					return
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		require.NoError(t, SetKeys(map[string]KeyPair{RoleUser: pairOf(userTestKey), RoleAdmin: pairOf(adminTestKey)}))
	}
	close(stop)
	wg.Wait()
}

func TestKeysLoaded(t *testing.T) {
	currentKeys.Store(nil)
	assert.False(t, KeysLoaded(RoleUser))
	_, err := ParseToken("x", RoleUser, TokenTypeAccess)
	assert.ErrorIs(t, err, ErrKeysNotLoaded)

	setupTestKeys(t)
	assert.True(t, KeysLoaded(RoleUser))
	assert.True(t, KeysLoaded(RoleAdmin))
	assert.False(t, KeysLoaded("superuser"))
}

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"Bearer abc.def.ghi":  "abc.def.ghi",
		"bearer abc.def.ghi":  "abc.def.ghi",
		"BEARER abc.def.ghi":  "abc.def.ghi",
		"Bearer  abc.def.ghi": "abc.def.ghi",
		"abc.def.ghi":         "",
		"Basic dXNlcjpwYXNz":  "",
		"Bearer":              "",
		"Bearer ":             "",
		"Bearer abc def":      "",
		"Token abc.def.ghi":   "",
	}
	for header, want := range cases {
		token, ok := bearerToken(header)
		assert.Equal(t, want, token, header)
		assert.Equal(t, want != "", ok, header)
	}
}

func TestMiddlewares_AcceptOnlyBearerTokensOfTheirRole(t *testing.T) {
	setupTestKeys(t)
	userToken := mustToken(t, RoleUser)
	adminToken := mustToken(t, RoleAdmin)

	cases := []struct {
		name          string
		middleware    gin.HandlerFunc
		authorization string
		want          int
	}{
		{"user route, user token", JWTUserVerification(nil), "Bearer " + userToken, http.StatusOK},
		{"user route, admin token", JWTUserVerification(nil), "Bearer " + adminToken, http.StatusUnauthorized},
		{"user route, bare token", JWTUserVerification(nil), userToken, http.StatusUnauthorized},
		{"user route, Basic scheme", JWTUserVerification(nil), "Basic " + userToken, http.StatusUnauthorized},
		{"user route, no header", JWTUserVerification(nil), "", http.StatusUnauthorized},
		{"admin route, admin token", JWTAdminVerification(nil), "Bearer " + adminToken, http.StatusOK},
		{"admin route, user token", JWTAdminVerification(nil), "Bearer " + userToken, http.StatusUnauthorized},
		{"admin route, bare token", JWTAdminVerification(nil), adminToken, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serve(tc.middleware, tc.authorization, nil)
			assert.Equal(t, tc.want, w.Code, w.Body.String())
			if tc.want == http.StatusUnauthorized {
				assert.Equal(t, "Bearer", w.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

func TestMiddlewares_RejectRefreshTokens(t *testing.T) {
	setupTestKeys(t)

	for role, middleware := range map[string]gin.HandlerFunc{
		RoleUser:  JWTUserVerification(nil),
		RoleAdmin: JWTAdminVerification(nil),
	} {
		refreshToken, err := GenerateRefreshToken("user-123", role, "test-session")
		require.NoError(t, err)

		w := serve(middleware, "Bearer "+refreshToken, nil)

		assert.Equal(t, http.StatusUnauthorized, w.Code, "a %s refresh token must not authorize API requests", role)
	}
}

func TestMiddleware_ProvidesClaimsToHandlers(t *testing.T) {
	setupTestKeys(t)
	token := mustToken(t, RoleUser)

	var claims *Claims
	var seenToken string
	w := serve(JWTUserVerification(nil), "Bearer "+token, func(c *gin.Context) {
		claims, _ = GetClaims(c)
		seenToken, _ = accessToken(c)
		c.Status(http.StatusOK)
	})

	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, claims)
	assert.Equal(t, "user-123", claims.UUID)
	assert.Equal(t, token, seenToken)
}

func TestLogoutHandler(t *testing.T) {
	setupTestKeys(t)
	blacklist := NewTokenBlacklist((*redis.Client)(nil))
	logout := func(sessions *fakeSessionStore) gin.HandlerFunc {
		return func(c *gin.Context) { LogoutHandler(c, blacklist, sessions) }
	}

	t.Run("bearer access token", func(t *testing.T) {
		sessions := &fakeSessionStore{}
		w := serve(JWTUserVerification(blacklist), "Bearer "+mustToken(t, RoleUser), logout(sessions))

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, []string{"test-session/user-123"}, sessions.revoked, "logout must revoke the token's session")
		cookie := refreshCookie(w)
		require.NotNil(t, cookie, "logout must clear the refresh token cookie")
		assert.Negative(t, cookie.MaxAge)
	})

	t.Run("bare token", func(t *testing.T) {
		sessions := &fakeSessionStore{}
		w := serve(JWTUserVerification(blacklist), mustToken(t, RoleUser), logout(sessions))

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Empty(t, sessions.revoked)
	})

	t.Run("refresh token", func(t *testing.T) {
		refreshToken, err := GenerateRefreshToken("user-123", RoleUser, "test-session")
		require.NoError(t, err)
		sessions := &fakeSessionStore{}

		w := serve(JWTUserVerification(blacklist), "Bearer "+refreshToken, logout(sessions))

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Empty(t, sessions.revoked)
	})

	t.Run("session store failure", func(t *testing.T) {
		sessions := &fakeSessionStore{revokeErr: errors.New("database down")}
		w := serve(JWTUserVerification(blacklist), "Bearer "+mustToken(t, RoleUser), logout(sessions))

		assert.Equal(t, http.StatusInternalServerError, w.Code, "logout must not report success when the session stays active")
	})

	t.Run("without the authentication middleware", func(t *testing.T) {
		sessions := &fakeSessionStore{}
		w := serve(func(c *gin.Context) { c.Next() }, "Bearer "+mustToken(t, RoleUser), logout(sessions))

		assert.Equal(t, http.StatusUnauthorized, w.Code, "logout trusts only tokens verified by the middleware")
		assert.Empty(t, sessions.revoked)
	})
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
		sessions := &fakeSessionStore{}

		assert.Equal(t, http.StatusUnauthorized, refresh(mustToken(t, RoleUser), sessions).Code)
		assert.Empty(t, sessions.rotations)
	})

	t.Run("admin refresh token", func(t *testing.T) {
		adminRefresh, err := GenerateRefreshToken("admin-1", RoleAdmin, "test-session")
		require.NoError(t, err)
		sessions := &fakeSessionStore{}

		assert.Equal(t, http.StatusUnauthorized, refresh(adminRefresh, sessions).Code, "only users have refresh sessions")
		assert.Empty(t, sessions.rotations)
	})

	t.Run("valid cookie rotates the refresh token", func(t *testing.T) {
		refreshToken, err := GenerateRefreshToken("user-123", RoleUser, "test-session")
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
		claims, err := ParseToken(cookie.Value, RoleUser, TokenTypeRefresh)
		require.NoError(t, err)
		assert.Equal(t, "test-session", claims.SessionID)
	})

	t.Run("reused refresh token", func(t *testing.T) {
		refreshToken, err := GenerateRefreshToken("user-123", RoleUser, "test-session")
		require.NoError(t, err)

		w := refresh(refreshToken, &fakeSessionStore{rotateErr: ErrRefreshTokenReused})

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		cookie := refreshCookie(w)
		require.NotNil(t, cookie)
		assert.Negative(t, cookie.MaxAge, "the cookie of a revoked session must be cleared")
	})

	t.Run("inactive session", func(t *testing.T) {
		refreshToken, err := GenerateRefreshToken("user-123", RoleUser, "test-session")
		require.NoError(t, err)

		assert.Equal(t, http.StatusUnauthorized, refresh(refreshToken, &fakeSessionStore{rotateErr: ErrSessionInvalid}).Code)
	})

	t.Run("session store failure", func(t *testing.T) {
		refreshToken, err := GenerateRefreshToken("user-123", RoleUser, "test-session")
		require.NoError(t, err)

		w := refresh(refreshToken, &fakeSessionStore{rotateErr: errors.New("database down")})

		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Nil(t, refreshCookie(w), "no new refresh token may be issued when the rotation was not stored")
	})
}

func TestSetRefreshTokenCookie(t *testing.T) {
	setupTestConfig()
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	SetRefreshTokenCookie(c, "test-refresh-token")

	cookies := w.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.Equal(t, "refresh_token", cookies[0].Name)
	assert.Equal(t, "test-refresh-token", cookies[0].Value)
	assert.True(t, cookies[0].HttpOnly)
	assert.True(t, cookies[0].Secure)
	assert.Equal(t, http.SameSiteStrictMode, cookies[0].SameSite)
	assert.Equal(t, "/public/auth", cookies[0].Path, "the cookie is only sent to the authentication routes")
	assert.Equal(t, config.Config.JWT.RefreshToken.Expiration*60, cookies[0].MaxAge)
}

func TestClaims_JSONSerialization(t *testing.T) {
	claims := validClaims(RoleUser, TokenTypeAccess)

	data, err := json.Marshal(claims)
	require.NoError(t, err)
	var unmarshalled Claims
	require.NoError(t, json.Unmarshal(data, &unmarshalled))

	assert.Equal(t, claims.UUID, unmarshalled.UUID)
	assert.Equal(t, claims.Role, unmarshalled.Role)
	assert.Equal(t, claims.Type, unmarshalled.Type)
	assert.Equal(t, claims.Audience, unmarshalled.Audience)
	assert.Equal(t, claims.ExpiresAt.Time.Unix(), unmarshalled.ExpiresAt.Time.Unix())
}

func TestTokenBlacklist_KeyGeneration(t *testing.T) {
	tb := NewTokenBlacklist(nil)

	key1 := tb.tokenKey("test-token")
	assert.Equal(t, key1, tb.tokenKey("test-token"), "the same token maps to the same key")
	assert.NotEqual(t, key1, tb.tokenKey("test-token-2"))

	hash := sha256.Sum256([]byte("test-token"))
	assert.Equal(t, "blacklist:token:"+hex.EncodeToString(hash[:]), key1)
	assert.True(t, strings.HasPrefix(key1, "blacklist:token:"))
}

func BenchmarkGenerateAccessToken(b *testing.B) {
	setupTestKeys(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = GenerateAccessToken("user-123", RoleUser, "test-session")
	}
}

func BenchmarkParseToken(b *testing.B) {
	setupTestKeys(b)
	token := mustToken(b, RoleUser)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ParseToken(token, RoleUser, TokenTypeAccess)
	}
}
