package public

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"encrypted-db/config"
	"encrypted-db/internal/auth"
	"encrypted-db/internal/db"
	"encrypted-db/internal/helpers"
	"encrypted-db/internal/models"
	"encrypted-db/internal/rabbitmq"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type PublicHandler struct {
	PostgresDB      *db.PostgresService
	RedisClient     *db.RedisService
	RabbitMQService *rabbitmq.RabbitMQService
}

func NewHandler(is *models.InfraServices) *PublicHandler {
	return &PublicHandler{
		PostgresDB:      is.Postgres,
		RedisClient:     is.Redis,
		RabbitMQService: is.RabbitMQ,
	}
}

type OTPRequest struct {
	Username string `json:"username"`
}

type VerifyOTPRequest struct {
	UUID string `json:"uuid"`
	OTP  string `json:"otp"`
}

type User struct {
	ID        int
	HK        uuid.UUID
	Status    string
	Info      map[string]interface{}
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt sql.NullTime
}

const otpKeyPrefix = "otp:auth:"

func (h *PublicHandler) RequestOTP(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var req OTPRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Username == "" {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid request data: Username, Email or Mobile is required", nil)
		return
	}

	otpCode := GenerateOTP(config.Config.OTP.Auth.Length)
	userUUID := uuid.New().String()

	contactType, maskedContact := IdentifyInputType(req.Username)
	if contactType == "" {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid contact information", nil)
		return
	}

	redisKey := otpKeyPrefix + userUUID + ":" + otpCode

	authVal, err := EncryptWithAES(req.Username, userUUID)
	if err != nil || authVal == "" {
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to encrypt OTP data", err)
		return
	}

	otpData := map[string]interface{}{
		"auth": authVal,
		"uuid": userUUID,
	}

	otpJSON, err := json.Marshal(otpData)
	if err != nil {
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to marshal OTP data", nil)
		return
	}

	ttl := time.Duration(config.Config.OTP.Auth.TTL) * time.Second
	if err := h.RedisClient.Client.Set(ctx, redisKey, otpJSON, ttl).Err(); err != nil {
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to store OTP", nil)
		return
	}

	blacklistKey := "uuid_blacklist:" + userUUID
	h.RedisClient.Client.Set(ctx, blacklistKey, 0, ttl*2)

	log.Printf("OTP Code for %s: %s: %s", contactType, req.Username, otpCode)
	helpers.SendResponse(c, http.StatusOK, "OTP sent to your contact", gin.H{"type": contactType, "username": maskedContact, "uuid": userUUID})
}

func (h *PublicHandler) VerifyOTP(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var req VerifyOTPRequest
	reqUUID := c.Param("uuid")
	if err := c.ShouldBindJSON(&req); err != nil || reqUUID == "" || req.OTP == "" {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid request data: UUID and OTP are required", nil)
		return
	}

	if !validateUUID(reqUUID) {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid UUID format", nil)
		return
	}

	blacklistKey := "uuid_blacklist:" + reqUUID
	attempts, _ := h.RedisClient.Client.Get(ctx, blacklistKey).Int()
	if attempts >= config.Config.OTP.Auth.RetryLimit {
		helpers.SendResponse(c, http.StatusTooManyRequests, "Too many failed attempts. Please try again later.", nil)
		return
	}

	redisKey := otpKeyPrefix + reqUUID + ":" + req.OTP
	otpData, err := h.RedisClient.Client.Get(ctx, redisKey).Result()
	if err != nil {
		h.incrementBlacklistAttempts(ctx, reqUUID)
		helpers.SendResponse(c, http.StatusUnauthorized, "Invalid or expired OTP code", nil)
		return
	}

	var otpInfo map[string]interface{}
	if err := json.Unmarshal([]byte(otpData), &otpInfo); err != nil {
		helpers.SendResponse(c, http.StatusUnauthorized, "Failed to parse OTP data", nil)
		return
	}

	if otpInfo["uuid"] != reqUUID {
		helpers.SendResponse(c, http.StatusUnauthorized, "Invalid UUID", nil)
		return
	}

	contact, err := DecryptWithAES(otpInfo["auth"].(string), reqUUID)
	if err != nil || contact == "" {
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to decrypt OTP data", nil)
		return
	}

	h.RedisClient.Client.Del(ctx, redisKey)
	h.RedisClient.Client.Del(ctx, blacklistKey)

	contactType, _ := IdentifyInputType(contact)
	if contactType == "" {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid contact information", nil)
		return
	}

	userHK, err := h.checkOrCreateUser(ctx, contactType, contact)
	if err != nil || userHK == uuid.Nil {
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to create user", nil)
		return
	}

	accessToken, err := auth.GenerateAccessToken(userHK.String(), "user")
	if err != nil {
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to generate access token", nil)
		return
	}
	refreshToken, err := auth.GenerateRefreshToken(userHK.String(), "user")
	if err != nil {
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to generate refresh token", nil)
		return
	}

	auth.SetRefreshTokenCookie(c, refreshToken)

	helpers.SendResponse(c, http.StatusOK, "OTP verified successfully", gin.H{"access_token": accessToken, "refresh_token": refreshToken})
}

// contactLookupQueries uses the same expressions as the unique contact indexes
// (migration 11), so each lookup is served by an index.
var contactLookupQueries = map[string]string{
	"email":    `SELECT hk, status FROM users WHERE info->'contact'->>'email' = $1`,
	"mobile":   `SELECT hk, status FROM users WHERE info->'contact'->>'mobile' = $1`,
	"username": `SELECT hk, status FROM users WHERE info->'contact'->>'username' = $1`,
}

// errUserAlreadyExists reports that another request created a user with the
// same contact between the lookup and the insert.
var errUserAlreadyExists = errors.New("user with this contact already exists")

const uniqueViolationCode = "23505"

func (h *PublicHandler) checkOrCreateUser(ctx context.Context, contactType, contact string) (uuid.UUID, error) {
	log.Println("Checking if user exists or creating a new user")

	query, ok := contactLookupQueries[contactType]
	if !ok {
		return uuid.Nil, fmt.Errorf("unsupported contact type: %s", contactType)
	}
	var user User

	err := h.PostgresDB.DB.QueryRowContext(ctx, query, contact).Scan(&user.HK, &user.Status)
	if err == sql.ErrNoRows {
		log.Println("User does not exist, creating new user")
		userHK, createErr := h.createUser(ctx, contactType, contact)
		if !errors.Is(createErr, errUserAlreadyExists) {
			return userHK, createErr
		}
		// A concurrent sign-in for the same contact won the insert; continue
		// with that user so its status checks still apply.
		log.Println("User was created by a concurrent request, loading it")
		err = h.PostgresDB.DB.QueryRowContext(ctx, query, contact).Scan(&user.HK, &user.Status)
	}
	if err != nil {
		log.Printf("Database error: %v", err)
		return uuid.Nil, fmt.Errorf("database error. please try again")
	}

	switch user.Status {
	case "disabled":
		return uuid.Nil, fmt.Errorf("user is disabled. please contact support")
	case "deleted":
		return uuid.Nil, fmt.Errorf("user is deleted. please contact support")
	case "suspend":
		return uuid.Nil, fmt.Errorf("user is suspended. please contact support")
	}

	log.Printf("User already exists with ID: %s", user.HK.String())
	return user.HK, nil
}

func (h *PublicHandler) createUser(ctx context.Context, contactType, contact string) (uuid.UUID, error) {
	info := map[string]interface{}{
		"contact": map[string]interface{}{
			contactType: contact,
		},
	}
	infoData, err := json.Marshal(info)
	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to marshal user info: %w", err)
	}

	query := `INSERT INTO users (info, status) VALUES ($1, 'approved') RETURNING hk`
	var userHK uuid.UUID
	err = h.PostgresDB.DB.QueryRowContext(ctx, query, infoData).Scan(&userHK)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolationCode {
			return uuid.Nil, errUserAlreadyExists
		}
		log.Printf("Failed to insert new user: %v", err)
		return uuid.Nil, fmt.Errorf("failed to create user. please try again")
	}

	log.Printf("New user created successfully with ID: %s", userHK)
	return userHK, nil
}

func (h *PublicHandler) incrementBlacklistAttempts(ctx context.Context, uuidStr string) {
	blacklistKey := "uuid_blacklist:" + uuidStr
	attempts, _ := h.RedisClient.Client.Get(ctx, blacklistKey).Int()
	attempts++
	h.RedisClient.Client.Set(ctx, blacklistKey, attempts, 24*time.Hour)

	if attempts > config.Config.OTP.Auth.RetryLimit {
		log.Printf("Retry limit exceeded for UUID: %s", uuidStr)
	}
}

func (h *PublicHandler) RefreshToken(c *gin.Context) {
	auth.RefreshTokenHandler(c)
}

func validateUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

func GenerateOTP(length int) string {
	const digits = "0123456789"
	b := make([]byte, length)
	randomBytes := make([]byte, length)
	if _, err := rand.Read(randomBytes); err != nil {
		return ""
	}
	for i := range b {
		b[i] = digits[int(randomBytes[i])%len(digits)]
	}
	return string(b)
}

func IdentifyInputType(input string) (string, string) {
	emailRegex := `^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`
	mobileRegex := `^09\d{9}$`
	usernameRegex := `^[a-zA-Z0-9_]{6,20}$`

	mask := func(visibleStart, visibleEnd int, str string) string {
		if len(str) <= visibleStart+visibleEnd {
			return str
		}
		maskedPart := strings.Repeat("*", 5)
		return str[:visibleStart] + maskedPart + str[len(str)-visibleEnd:]
	}

	if matched, _ := regexp.MatchString(emailRegex, input); matched {
		atIndex := strings.Index(input, "@")
		if atIndex > 1 {
			return "email", mask(3, len(input)-atIndex, input)
		}
	}

	if matched, _ := regexp.MatchString(mobileRegex, input); matched {
		return "mobile", mask(3, 3, input)
	}

	if matched, _ := regexp.MatchString(usernameRegex, input); matched {
		firstChar := rune(input[0])
		if unicode.IsDigit(firstChar) || firstChar == '_' {
			return "", ""
		}

		consecutiveUnderscores := regexp.MustCompile(`__`)
		if consecutiveUnderscores.MatchString(input) {
			return "", ""
		}

		containsAlphanumeric := regexp.MustCompile(`[a-zA-Z0-9]`)
		if containsAlphanumeric.MatchString(input) {
			return "username", mask(2, 3, input)
		}
	}

	return "", ""
}

func (h *PublicHandler) GetActiveCurrencies(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	pattern := "base_definitions:currency:*"

	currencies := make([]models.Currency, 0)
	iter := h.RedisClient.Client.Scan(ctx, 0, pattern, 100).Iterator()
	for iter.Next(ctx) {
		data, err := h.RedisClient.Client.Get(ctx, iter.Val()).Result()
		if err != nil {
			log.Printf("Error retrieving currency from cache for key %s: %v", iter.Val(), err)
			continue
		}

		var currency models.Currency
		if err := json.Unmarshal([]byte(data), &currency); err != nil {
			log.Printf("Error unmarshalling currency data for key %s: %v", iter.Val(), err)
			continue
		}

		currencies = append(currencies, currency)
	}

	if err := iter.Err(); err != nil {
		log.Printf("Error during Redis key iteration: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to retrieve currencies from cache.", nil)
		return
	}

	helpers.SendResponse(c, http.StatusOK, "Active currencies retrieved successfully.", currencies)
}

func (h *PublicHandler) GetCurrencyByHK(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	hk := c.Param("hk")
	cacheKey := fmt.Sprintf("base_definitions:currency:%s", hk)

	data, err := h.RedisClient.Client.Get(ctx, cacheKey).Result()
	if err == redis.Nil {
		helpers.SendResponse(c, http.StatusNotFound, "Currency not found", nil)
		return
	} else if err != nil {
		log.Printf("Error retrieving currency from cache for HK %s: %v", hk, err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to retrieve currency from cache.", nil)
		return
	}

	var currency models.Currency
	if err := json.Unmarshal([]byte(data), &currency); err != nil {
		log.Printf("Error unmarshalling currency data for HK %s: %v", hk, err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to process currency information.", nil)
		return
	}

	helpers.SendResponse(c, http.StatusOK, "Currency retrieved successfully.", currency)
}
