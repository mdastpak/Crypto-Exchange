package public

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/domain/account"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type PublicHandler struct {
	PostgresDB      *db.PostgresService
	RedisClient     *db.RedisService
	RabbitMQService *rabbitmq.RabbitMQService
	Sessions        auth.SessionStore
	// OTP issues and verifies sign-in codes.
	OTP *otp.Store
	// OTPSender delivers sign-in codes. It discards them unless the server
	// replaces it with the configured delivery.
	OTPSender notify.OTPSender
	// Currencies lists the public currencies.
	Currencies *currencies.Catalog
}

func NewHandler(is *models.InfraServices) *PublicHandler {
	h := &PublicHandler{
		PostgresDB:      is.Postgres,
		RedisClient:     is.Redis,
		RabbitMQService: is.RabbitMQ,
		OTPSender:       notify.DiscardSender{},
		Currencies:      currencies.ForServices(is),
	}
	if is.Postgres != nil {
		h.Sessions = auth.NewPostgresSessionStore(is.Postgres.DB)
	}
	if is.Redis != nil {
		h.OTP = otp.NewStore(is.Redis.Client, otp.SettingsFromConfig())
	}
	return h
}

type OTPRequest struct {
	Username string `json:"username"`
}

type VerifyOTPRequest struct {
	UUID string `json:"uuid"`
	OTP  string `json:"otp"`
}

func (h *PublicHandler) RequestOTP(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var req OTPRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Username == "" {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid request data: Email or Mobile is required", nil)
		return
	}

	contactType, maskedContact := IdentifyInputType(req.Username)
	if contactType == "" {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid contact information", nil)
		return
	}
	if !h.OTPSender.Supports(contactType) {
		helpers.SendResponse(c, http.StatusBadRequest, fmt.Sprintf("Sign-in by %s is not available", contactType), nil)
		return
	}

	issued, err := h.OTP.Issue(ctx, req.Username)
	switch {
	case errors.Is(err, otp.ErrTooManyRequests):
		helpers.SendResponse(c, http.StatusTooManyRequests, "Too many sign-in codes requested. Please try again later.", nil)
		return
	case errors.Is(err, otp.ErrTooManyAttempts):
		helpers.SendResponse(c, http.StatusTooManyRequests, "Too many failed attempts. Please try again later.", nil)
		return
	case err != nil:
		log.Printf("Failed to issue a sign-in code for %s %s: %v", contactType, maskedContact, err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to store OTP", nil)
		return
	}

	// Never log the code or the raw contact: either one in a log helps an
	// attacker take over the account.
	err = h.OTPSender.SendOTP(ctx, notify.OTPMessage{
		ContactType:   contactType,
		Contact:       req.Username,
		MaskedContact: maskedContact,
		Code:          issued.Code,
	})
	if err != nil {
		log.Printf("Failed to send the sign-in code to %s %s: %v", contactType, maskedContact, err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to send OTP", nil)
		return
	}

	helpers.SendResponse(c, http.StatusOK, "OTP sent to your contact", gin.H{"type": contactType, "username": maskedContact, "uuid": issued.SessionID})
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

	// Verify ends the session on success, so a code signs in only once, even
	// when it is sent in concurrent requests.
	contact, err := h.OTP.Verify(ctx, reqUUID, req.OTP)
	switch {
	case errors.Is(err, otp.ErrTooManyAttempts):
		helpers.SendResponse(c, http.StatusTooManyRequests, "Too many failed attempts. Please try again later.", nil)
		return
	case errors.Is(err, otp.ErrInvalidCode):
		helpers.SendError(c, http.StatusUnauthorized, "code_invalid", "Invalid or expired OTP code", nil)
		return
	case err != nil:
		log.Printf("Failed to verify a sign-in code: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to verify OTP", nil)
		return
	}

	contactType, _ := IdentifyInputType(contact)
	if contactType == "" {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid contact information", nil)
		return
	}

	userHK, err := h.checkOrCreateUser(ctx, contactType, contact)
	if errors.Is(err, errAccountUnavailable) {
		// The code was right, so the caller controls the contact and may
		// learn why the account cannot sign in.
		helpers.SendError(c, http.StatusForbidden, helpers.CodeAccountUnavailable, "This account cannot sign in. Please contact support.", nil)
		return
	}
	if errors.Is(err, errAdminAccount) {
		helpers.SendError(c, http.StatusForbidden, "use_admin_sign_in", "Administrators must use the admin sign-in.", nil)
		return
	}
	if err != nil || userHK == uuid.Nil {
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to create user", nil)
		return
	}

	sessionID := uuid.NewString()
	accessToken, err := auth.GenerateAccessToken(userHK.String(), auth.RoleUser, sessionID)
	if err != nil {
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to generate access token", nil)
		return
	}
	refreshToken, err := auth.GenerateRefreshToken(userHK.String(), auth.RoleUser, sessionID)
	if err != nil {
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to generate refresh token", nil)
		return
	}

	err = h.Sessions.Create(ctx, auth.Session{
		ID:               sessionID,
		UserID:           userHK.String(),
		AuthMethod:       "OTP",
		AccessTokenHash:  auth.HashToken(accessToken),
		RefreshTokenHash: auth.HashToken(refreshToken),
		UserAgent:        c.Request.UserAgent(),
		IP:               c.ClientIP(),
		ExpiresAt:        time.Now().Add(auth.RefreshTokenLifetime()),
	})
	if err != nil {
		log.Printf("Failed to create session: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to create session", nil)
		return
	}

	// The refresh token is only delivered in the HttpOnly cookie, never in the
	// body, so scripts running in the page cannot read it.
	auth.SetRefreshTokenCookie(c, refreshToken)

	helpers.SendResponse(c, http.StatusOK, "OTP verified successfully", gin.H{"access_token": accessToken})
}

// userByContactQuery finds the user that owns a contact, through the unique
// index of user_contacts.
const userByContactQuery = `SELECT u.hk, u.status, u.role, u.deleted_at IS NOT NULL
	FROM user_contacts c JOIN users u ON u.hk = c.user_hk WHERE ` + db.ContactMatch

// signInContactTypes are the contacts that a user can sign in with.
var signInContactTypes = map[string]bool{string(account.ContactEmail): true, string(account.ContactMobile): true}

// errUserAlreadyExists reports that another request created a user with the
// same contact between the lookup and the insert.
var errUserAlreadyExists = errors.New("user with this contact already exists")

// errAccountUnavailable reports an account that is deleted, or whose status
// does not allow signing in.
var errAccountUnavailable = errors.New("account cannot sign in")

// errAdminAccount reports an administrator account at user sign-in.
var errAdminAccount = errors.New("administrators cannot use user sign-in")

const uniqueViolationCode = "23505"

func (h *PublicHandler) checkOrCreateUser(ctx context.Context, contactType, contact string) (uuid.UUID, error) {
	log.Println("Checking if user exists or creating a new user")

	if !signInContactTypes[contactType] {
		return uuid.Nil, fmt.Errorf("unsupported contact type: %s", contactType)
	}
	var userHK uuid.UUID
	var status, role string
	var deleted bool
	find := func() error {
		return h.PostgresDB.DB.QueryRowContext(ctx, userByContactQuery, contactType, contact).Scan(&userHK, &status, &role, &deleted)
	}

	err := find()
	if err == sql.ErrNoRows {
		log.Println("User does not exist, creating new user")
		userHK, createErr := h.createUser(ctx, contactType, contact)
		if !errors.Is(createErr, errUserAlreadyExists) {
			return userHK, createErr
		}
		// A concurrent sign-in for the same contact won the insert; continue
		// with that user so its status checks still apply.
		log.Println("User was created by a concurrent request, loading it")
		err = find()
	}
	if err != nil {
		log.Printf("Database error: %v", err)
		return uuid.Nil, fmt.Errorf("database error. please try again")
	}

	if deleted || !account.UserStatus(status).CanSignIn() {
		return uuid.Nil, fmt.Errorf("%w: status %s, deleted %t", errAccountUnavailable, status, deleted)
	}
	if role == auth.RoleAdmin {
		// Administrators sign in at /admin/auth with a second factor; a
		// user token for their account is never issued.
		return uuid.Nil, errAdminAccount
	}

	log.Printf("User already exists with ID: %s", userHK.String())
	return userHK, nil
}

// createUser creates an active user with its first contact, in one
// transaction. The contact is verified: users are created after a correct
// sign-in code, which proves control of it.
func (h *PublicHandler) createUser(ctx context.Context, contactType, contact string) (uuid.UUID, error) {
	tx, err := h.PostgresDB.DB.BeginTx(ctx, nil)
	if err != nil {
		return uuid.Nil, fmt.Errorf("start the transaction: %w", err)
	}
	defer tx.Rollback()

	var userHK uuid.UUID
	if err := tx.QueryRowContext(ctx, `INSERT INTO users (status) VALUES ('ACTIVE') RETURNING hk`).Scan(&userHK); err != nil {
		log.Printf("Failed to insert new user: %v", err)
		return uuid.Nil, fmt.Errorf("failed to create user. please try again")
	}
	if err := db.InsertContact(ctx, tx, userHK.String(), contactType, contact, true); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolationCode {
			return uuid.Nil, errUserAlreadyExists
		}
		log.Printf("Failed to insert the contact of a new user: %v", err)
		return uuid.Nil, fmt.Errorf("failed to create user. please try again")
	}
	if err := tx.Commit(); err != nil {
		log.Printf("Failed to commit a new user: %v", err)
		return uuid.Nil, fmt.Errorf("failed to create user. please try again")
	}
	return userHK, nil
}

func (h *PublicHandler) RefreshToken(c *gin.Context) {
	auth.RefreshTokenHandler(c, h.Sessions)
}

func validateUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// IdentifyInputType returns the contact type of input ("email" or "mobile")
// and a masked form that is safe to log and show, or two empty strings when
// input is neither. Usernames are not accepted: a code cannot be delivered to
// a username, so it cannot prove who is signing in.
func IdentifyInputType(input string) (string, string) {
	emailRegex := `^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`
	mobileRegex := `^09\d{9}$`

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

	return "", ""
}

// currencyReadTimeout bounds a currency read, which may load the list from
// PostgreSQL.
const currencyReadTimeout = 10 * time.Second

// GetActiveCurrencies lists the public currencies, oldest first.
func (h *PublicHandler) GetActiveCurrencies(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), currencyReadTimeout)
	defer cancel()

	list, err := h.Currencies.List(ctx)
	if err != nil {
		log.Printf("Error listing the public currencies: %v", err)
		helpers.SendResponse(c, http.StatusServiceUnavailable, "Currencies are temporarily unavailable.", nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, "Active currencies retrieved successfully.", list)
}

// GetCurrencyByHK returns one public currency. A currency that is not public
// gets the same 404 as one that does not exist.
func (h *PublicHandler) GetCurrencyByHK(c *gin.Context) {
	id, err := uuid.Parse(c.Param("hk"))
	if err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid UUID format for HK", nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), currencyReadTimeout)
	defer cancel()

	currency, found, err := h.Currencies.Get(ctx, id.String())
	if err != nil {
		log.Printf("Error reading the public currency %s: %v", id, err)
		helpers.SendResponse(c, http.StatusServiceUnavailable, "Currencies are temporarily unavailable.", nil)
		return
	}
	if !found {
		helpers.SendResponse(c, http.StatusNotFound, "Currency not found", nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, "Currency retrieved successfully.", currency)
}
