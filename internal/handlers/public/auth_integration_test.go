package public

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"encrypted-db/config"
	"encrypted-db/internal/auth"
	"encrypted-db/internal/db"
	"encrypted-db/internal/models"
	"encrypted-db/internal/testutil/redistest"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader("X-Request-ID")
		if requestID == "" {
			requestID = fmt.Sprintf("%d", time.Now().UnixNano())
		}
		c.Set("request_id", requestID)
		c.Header("X-Request-ID", requestID)
		c.Next()
	}
}

type PublicHandlerTestSuite struct {
	suite.Suite
	router          *gin.Engine
	handler         *PublicHandler
	postgresService *db.PostgresService
	redisClient     *redis.Client
	sender          *recordingOTPSender
}

func (s *PublicHandlerTestSuite) SetupSuite() {
	gin.SetMode(gin.TestMode)

	config.Config = config.Configuration{}
	config.Config.Server.IP = "localhost"
	config.Config.Server.Port = "8080"
	config.Config.OTP.Auth.Length = 6
	config.Config.OTP.Auth.TTL = 300
	config.Config.OTP.Auth.RetryLimit = 5
	config.Config.JWT.AccessToken.Expiration = 15
	config.Config.JWT.RefreshToken.Expiration = 10080
	config.Config.JWT.Issuer = "test"

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(s.T(), err)
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})
	publicKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PUBLIC KEY",
		Bytes: x509.MarshalPKCS1PublicKey(&privateKey.PublicKey),
	})
	require.NoError(s.T(), auth.InitKeys(privateKeyPEM, publicKeyPEM))
}

func (s *PublicHandlerTestSuite) SetupTest() {
	s.redisClient = redis.NewClient(&redis.Options{
		Addr:     "localhost:6379",
		Password: "",
		DB:       redistest.Public,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(s.T(), s.redisClient.Ping(ctx).Err())

	// Sign-in writes to the sessions table, so the suite needs the schema
	// built by the real migrations.
	postgresService := migrateRealSchema(s.T())
	s.postgresService = postgresService

	redisService := &db.RedisService{Client: s.redisClient}

	services := &models.InfraServices{
		Postgres: postgresService,
		Redis:    redisService,
		RabbitMQ: nil,
	}

	s.handler = NewHandler(services)
	s.sender = &recordingOTPSender{}
	s.handler.OTPSender = s.sender
	s.router = gin.New()
	s.router.Use(gin.Recovery())
	s.router.Use(requestIDMiddleware())

	publicGroup := s.router.Group("/public")
	{
		publicGroup.POST("/auth", s.handler.RequestOTP)
		publicGroup.POST("/auth/:uuid", s.handler.VerifyOTP)
		publicGroup.POST("/auth/refresh", s.handler.RefreshToken)
		blacklist := auth.NewTokenBlacklist(s.redisClient)
		publicGroup.POST("/auth/logout", auth.JWTUserVerification(blacklist), func(c *gin.Context) {
			auth.LogoutHandler(c, blacklist, s.handler.Sessions)
		})
		publicGroup.GET("/currencies", s.handler.GetActiveCurrencies)
		publicGroup.GET("/currencies/:hk", s.handler.GetCurrencyByHK)
	}
}

func (s *PublicHandlerTestSuite) TearDownTest() {
	ctx := context.Background()
	s.redisClient.FlushDB(ctx)
	// migrateRealSchema closes the database service when the test ends.
}

func (s *PublicHandlerTestSuite) TearDownSuite() {
	s.redisClient.Close()
}

func (s *PublicHandlerTestSuite) makeRequest(method, path string, body interface{}, token string) *httptest.ResponseRecorder {
	var reqBody []byte
	if body != nil {
		reqBody, _ = json.Marshal(body)
	}

	req, _ := http.NewRequest(method, path, bytes.NewBuffer(reqBody))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	return w
}

func (s *PublicHandlerTestSuite) makeRequestWithCookie(method, path string, body interface{}, cookieName, cookieValue string) *httptest.ResponseRecorder {
	var reqBody []byte
	if body != nil {
		reqBody, _ = json.Marshal(body)
	}

	req, _ := http.NewRequest(method, path, bytes.NewBuffer(reqBody))
	req.Header.Set("Content-Type", "application/json")
	if cookieName != "" && cookieValue != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookieValue})
	}

	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	return w
}

// sentOTP returns the last code handed to the sender. Redis only holds a
// hash of the code.
func (s *PublicHandlerTestSuite) sentOTP() string {
	return lastSentCode(s.T(), s.sender)
}

func TestPublicHandlerSuite(t *testing.T) {
	suite.Run(t, new(PublicHandlerTestSuite))
}

func (s *PublicHandlerTestSuite) TestRequestOTP_ValidEmail() {
	w := s.makeRequest(http.MethodPost, "/public/auth", map[string]string{
		"username": "test@example.com",
	}, "")

	assert.Equal(s.T(), http.StatusOK, w.Code)

	var resp models.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "OTP sent to your contact", resp.Message)
	assert.NotNil(s.T(), resp.Info)

	data := resp.Info.(map[string]interface{})
	assert.Equal(s.T(), "email", data["type"])
	assert.NotEmpty(s.T(), data["uuid"])
}

func (s *PublicHandlerTestSuite) TestRequestOTP_ValidMobile() {
	w := s.makeRequest(http.MethodPost, "/public/auth", map[string]string{
		"username": "09123456789",
	}, "")

	assert.Equal(s.T(), http.StatusOK, w.Code)

	var resp models.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "OTP sent to your contact", resp.Message)

	data := resp.Info.(map[string]interface{})
	assert.Equal(s.T(), "mobile", data["type"])
}

func (s *PublicHandlerTestSuite) TestRequestOTP_UsernameIsRejected() {
	w := s.makeRequest(http.MethodPost, "/public/auth", map[string]string{
		"username": "testuser123",
	}, "")

	// A code cannot be delivered to a username, so it cannot prove who is
	// signing in.
	assert.Equal(s.T(), http.StatusBadRequest, w.Code)

	var resp models.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "Invalid contact information", resp.Message)
}

func (s *PublicHandlerTestSuite) TestRequestOTP_InvalidInput() {
	w := s.makeRequest(http.MethodPost, "/public/auth", map[string]string{
		"username": "",
	}, "")

	assert.Equal(s.T(), http.StatusBadRequest, w.Code)

	var resp models.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(s.T(), err)
	assert.Contains(s.T(), resp.Message, "required")
}

func (s *PublicHandlerTestSuite) TestRequestOTP_InvalidFormat() {
	w := s.makeRequest(http.MethodPost, "/public/auth", map[string]string{
		"username": "invalid@",
	}, "")

	assert.Equal(s.T(), http.StatusBadRequest, w.Code)
}

func (s *PublicHandlerTestSuite) TestVerifyOTP_Success() {
	w := s.makeRequest(http.MethodPost, "/public/auth", map[string]string{
		"username": "test@example.com",
	}, "")
	require.Equal(s.T(), http.StatusOK, w.Code)

	var otpResp models.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &otpResp)
	require.NoError(s.T(), err)

	otpData := otpResp.Info.(map[string]interface{})
	otpUUID := otpData["uuid"].(string)
	otp := s.sentOTP()

	w = s.makeRequest(http.MethodPost, fmt.Sprintf("/public/auth/%s", otpUUID), map[string]string{
		"otp": otp,
	}, "")

	assert.Equal(s.T(), http.StatusOK, w.Code)

	var resp models.APIResponse
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "OTP verified successfully", resp.Message)
	assert.NotNil(s.T(), resp.Info)

	data := resp.Info.(map[string]interface{})
	assert.NotEmpty(s.T(), data["access_token"])
	assert.NotContains(s.T(), data, "refresh_token", "the refresh token is only delivered in the cookie")
	assert.NotEmpty(s.T(), refreshCookieValue(w), "the refresh token cookie must be set")
}

// refreshCookieValue returns the refresh token set by a response, if any.
func refreshCookieValue(w *httptest.ResponseRecorder) string {
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == auth.RefreshTokenCookie && cookie.MaxAge >= 0 {
			return cookie.Value
		}
	}
	return ""
}

func (s *PublicHandlerTestSuite) TestVerifyOTP_InvalidOTP() {
	w := s.makeRequest(http.MethodPost, "/public/auth", map[string]string{
		"username": "test2@example.com",
	}, "")
	require.Equal(s.T(), http.StatusOK, w.Code)

	var otpResp models.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &otpResp)
	require.NoError(s.T(), err)

	otpData := otpResp.Info.(map[string]interface{})
	otpUUID := otpData["uuid"].(string)

	w = s.makeRequest(http.MethodPost, fmt.Sprintf("/public/auth/%s", otpUUID), map[string]string{
		"otp": "000000",
	}, "")

	assert.Equal(s.T(), http.StatusUnauthorized, w.Code)

	var resp models.APIResponse
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "Invalid or expired OTP code", resp.Message)
}

func (s *PublicHandlerTestSuite) TestVerifyOTP_ExpiredOTP() {
	w := s.makeRequest(http.MethodPost, "/public/auth", map[string]string{
		"username": "test3@example.com",
	}, "")
	require.Equal(s.T(), http.StatusOK, w.Code)

	var otpResp models.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &otpResp)
	require.NoError(s.T(), err)

	otpData := otpResp.Info.(map[string]interface{})
	otpUUID := otpData["uuid"].(string)

	// Let the session expire.
	s.redisClient.Del(context.Background(), "otp:session:"+otpUUID)

	w = s.makeRequest(http.MethodPost, fmt.Sprintf("/public/auth/%s", otpUUID), map[string]string{
		"otp": s.sentOTP(),
	}, "")

	assert.Equal(s.T(), http.StatusUnauthorized, w.Code, "an expired code is refused even when it is right")
}

func (s *PublicHandlerTestSuite) TestVerifyOTP_InvalidUUID() {
	w := s.makeRequest(http.MethodPost, "/public/auth/invalid-uuid", map[string]string{
		"otp": "123456",
	}, "")

	assert.Equal(s.T(), http.StatusBadRequest, w.Code)
}

func (s *PublicHandlerTestSuite) TestVerifyOTP_TooManyAttempts() {
	w := s.makeRequest(http.MethodPost, "/public/auth", map[string]string{
		"username": "test4@example.com",
	}, "")
	require.Equal(s.T(), http.StatusOK, w.Code)

	var otpResp models.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &otpResp)
	require.NoError(s.T(), err)

	otpData := otpResp.Info.(map[string]interface{})
	otpUUID := otpData["uuid"].(string)

	for i := 0; i < 5; i++ {
		s.makeRequest(http.MethodPost, fmt.Sprintf("/public/auth/%s", otpUUID), map[string]string{
			"otp": "wrong_otp",
		}, "")
	}

	w = s.makeRequest(http.MethodPost, fmt.Sprintf("/public/auth/%s", otpUUID), map[string]string{
		"otp": "wrong_otp",
	}, "")

	assert.Equal(s.T(), http.StatusTooManyRequests, w.Code)
}

func (s *PublicHandlerTestSuite) TestRefreshToken_Success() {
	w := s.makeRequest(http.MethodPost, "/public/auth", map[string]string{
		"username": "test5@example.com",
	}, "")
	require.Equal(s.T(), http.StatusOK, w.Code)

	var otpResp models.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &otpResp)
	require.NoError(s.T(), err)

	otpData := otpResp.Info.(map[string]interface{})
	otpUUID := otpData["uuid"].(string)
	otp := s.sentOTP()

	w = s.makeRequest(http.MethodPost, fmt.Sprintf("/public/auth/%s", otpUUID), map[string]string{
		"otp": otp,
	}, "")
	require.Equal(s.T(), http.StatusOK, w.Code)

	var loginResp models.APIResponse
	err = json.Unmarshal(w.Body.Bytes(), &loginResp)
	require.NoError(s.T(), err)

	refreshToken := refreshCookieValue(w)
	require.NotEmpty(s.T(), refreshToken)

	w = s.makeRequestWithCookie(http.MethodPost, "/public/auth/refresh", nil, auth.RefreshTokenCookie, refreshToken)

	assert.Equal(s.T(), http.StatusOK, w.Code)

	var resp models.APIResponse
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "Access token refreshed", resp.Message)
	assert.NotNil(s.T(), resp.Info)

	data := resp.Info.(map[string]interface{})
	assert.NotEmpty(s.T(), data["access_token"])
}

func (s *PublicHandlerTestSuite) TestRefreshToken_MissingToken() {
	w := s.makeRequest(http.MethodPost, "/public/auth/refresh", nil, "")

	assert.Equal(s.T(), http.StatusUnauthorized, w.Code)
}

func (s *PublicHandlerTestSuite) TestRefreshToken_InvalidToken() {
	w := s.makeRequest(http.MethodPost, "/public/auth/refresh", nil, "invalid.token.here")

	assert.Equal(s.T(), http.StatusUnauthorized, w.Code)
}

func (s *PublicHandlerTestSuite) TestLogout_Success() {
	w := s.makeRequest(http.MethodPost, "/public/auth", map[string]string{
		"username": "test6@example.com",
	}, "")
	require.Equal(s.T(), http.StatusOK, w.Code)

	var otpResp models.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &otpResp)
	require.NoError(s.T(), err)

	otpData := otpResp.Info.(map[string]interface{})
	otpUUID := otpData["uuid"].(string)
	otp := s.sentOTP()

	w = s.makeRequest(http.MethodPost, fmt.Sprintf("/public/auth/%s", otpUUID), map[string]string{
		"otp": otp,
	}, "")
	require.Equal(s.T(), http.StatusOK, w.Code)

	var loginResp models.APIResponse
	err = json.Unmarshal(w.Body.Bytes(), &loginResp)
	require.NoError(s.T(), err)

	loginData := loginResp.Info.(map[string]interface{})
	accessToken := loginData["access_token"].(string)
	refreshToken := refreshCookieValue(w)
	require.NotEmpty(s.T(), refreshToken)

	w = s.makeRequest(http.MethodPost, "/public/auth/logout", nil, accessToken)

	assert.Equal(s.T(), http.StatusOK, w.Code)

	var resp models.APIResponse
	err = json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "Logged out successfully", resp.Message)

	ctx := context.Background()
	blacklisted, err := auth.NewTokenBlacklist(s.redisClient).IsTokenBlacklisted(ctx, accessToken)
	require.NoError(s.T(), err)
	assert.True(s.T(), blacklisted)

	w = s.makeRequestWithCookie(http.MethodPost, "/public/auth/refresh", nil, auth.RefreshTokenCookie, refreshToken)
	assert.Equal(s.T(), http.StatusUnauthorized, w.Code, "the refresh token must stop working after logout")
}

func (s *PublicHandlerTestSuite) TestGetActiveCurrencies_Empty() {
	w := s.makeRequest(http.MethodGet, "/public/currencies", nil, "")

	assert.Equal(s.T(), http.StatusOK, w.Code)

	var resp models.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "Active currencies retrieved successfully.", resp.Message)

	currencies := resp.Info.([]interface{})
	assert.Empty(s.T(), currencies)
}

func (s *PublicHandlerTestSuite) TestGetCurrencyByHK_NotFound() {
	w := s.makeRequest(http.MethodGet, "/public/currencies/"+uuid.New().String(), nil, "")

	assert.Equal(s.T(), http.StatusNotFound, w.Code)

	var resp models.APIResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "Currency not found", resp.Message)
}

// cacheCurrency writes a currency to the cache the way the admin handler does.
func (s *PublicHandlerTestSuite) cacheCurrency(c models.Currency) {
	data, err := json.Marshal(c)
	require.NoError(s.T(), err)
	require.NoError(s.T(), s.redisClient.Set(context.Background(), "base_definitions:currency:"+c.HK, data, 0).Err())
}

// The cache should only hold public currencies, but an entry that is out of
// date must not leak a hidden currency either.
func (s *PublicHandlerTestSuite) TestCurrencies_HideCurrenciesThatAreNotPublic() {
	deletedAt := "2026-01-01T00:00:00Z"
	approved := models.Currency{HK: uuid.New().String(), Status: models.CurrencyStatusApproved, Info: models.CurrencyInfo{Symbol: "BTC"}}
	hidden := []models.Currency{
		{HK: uuid.New().String(), Status: models.CurrencyStatusPending, Info: models.CurrencyInfo{Symbol: "ETH"}},
		{HK: uuid.New().String(), Status: models.CurrencyStatusSuspend, Info: models.CurrencyInfo{Symbol: "XRP"}},
		{HK: uuid.New().String(), Status: models.CurrencyStatusApproved, Info: models.CurrencyInfo{Symbol: "LTC"}, DeletedAt: &deletedAt},
	}
	s.cacheCurrency(approved)
	for _, c := range hidden {
		s.cacheCurrency(c)
	}

	w := s.makeRequest(http.MethodGet, "/public/currencies", nil, "")
	require.Equal(s.T(), http.StatusOK, w.Code)
	var list struct {
		Info []models.Currency `json:"info"`
	}
	require.NoError(s.T(), json.Unmarshal(w.Body.Bytes(), &list))
	require.Len(s.T(), list.Info, 1)
	assert.Equal(s.T(), approved.HK, list.Info[0].HK)

	w = s.makeRequest(http.MethodGet, "/public/currencies/"+approved.HK, nil, "")
	assert.Equal(s.T(), http.StatusOK, w.Code)

	for _, c := range hidden {
		w := s.makeRequest(http.MethodGet, "/public/currencies/"+c.HK, nil, "")
		assert.Equal(s.T(), http.StatusNotFound, w.Code, c.Info.Symbol)
		var resp models.APIResponse
		require.NoError(s.T(), json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(s.T(), "Currency not found", resp.Message, "a hidden currency must look like a missing one")
	}
}
