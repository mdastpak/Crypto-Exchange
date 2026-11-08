package public

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/clientip"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/contact"
	"github.com/mdastpak/crypto-exchange/internal/contactclaim"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/dberr"
	"github.com/mdastpak/crypto-exchange/internal/domain/account"
	"github.com/mdastpak/crypto-exchange/internal/favorites"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/pagination"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"

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
	// Blacklist holds the revoked access tokens, for the optional token of
	// the favorites filter.
	Blacklist *auth.TokenBlacklist
	// Platform serves the status of the platform controls (#235). Without
	// it, the status is open.
	Platform *platform.Gate
	// Ownership and Sealer recheck, at sign-in, that a mobile number still
	// belongs to the national ID of its account (#252, #253).
	Ownership identity.MobileOwnership
	Sealer    identity.Sealer
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

// @Summary Request a sign-in code
// @Description Sends a one-time code to an email address or a mobile number. Limited per hour and client address.
// @Tags sign-in
// @Accept json
// @Produce json
// @Param body body OTPRequest true "The email address or mobile number to send the code to"
// @Success 200 {object} models.APIResponse{info=models.SignInCodeInfo}
// @Failure 400 {object} models.APIResponse
// @Failure 429 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/auth [post]
func (h *PublicHandler) RequestOTP(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var req OTPRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Username == "" {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid request data: Email or Mobile is required", nil)
		return
	}
	// Every form of a contact that users type, such as a mobile number in
	// Persian digits or with +98, reaches the same account (#197).
	req.Username = contact.Normalize(req.Username)

	contactType, maskedContact := IdentifyInputType(req.Username)
	if contactType == "" {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid contact information", nil)
		return
	}
	if !h.OTPSender.Supports(contactType) {
		helpers.SendResponse(c, http.StatusBadRequest, fmt.Sprintf("Sign-in by %s is not available", contactType), nil)
		return
	}

	// The code request is the account's, when the contact belongs to one,
	// though anyone may make it. The event never holds the contact.
	event := audit.FromRequest(c)
	event.Action = audit.ActionUserSignInCode
	event.ActorType, event.ActorID = h.contactOwner(ctx, contactType, req.Username)

	// The per-contact limits count by the client that asks, so that a
	// stranger cannot use up the budget of the contact's owner (#187).
	issued, err := h.OTP.Issue(ctx, req.Username, clientip.Key(c.ClientIP(), config.Config.RateLimits.IPv6PrefixLength))
	switch {
	case errors.Is(err, otp.ErrTooManyRequests):
		h.noteSignIn(ctx, event, audit.Denied, reasonRateLimited)
		helpers.SendResponse(c, http.StatusTooManyRequests, "Too many sign-in codes requested. Please try again later.", nil)
		return
	case errors.Is(err, otp.ErrTooManyAttempts):
		h.noteSignIn(ctx, event, audit.Denied, reasonRateLimited)
		helpers.SendResponse(c, http.StatusTooManyRequests, "Too many failed attempts. Please try again later.", nil)
		return
	case err != nil:
		log.Printf("Failed to issue a sign-in code for %s %s: %v", contactType, maskedContact, err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to store OTP", nil)
		return
	}
	event.TargetType, event.TargetID = audit.TargetSignIn, issued.SessionID

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
		h.noteSignIn(ctx, event, audit.Failure, "delivery_failed")
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to send OTP", nil)
		return
	}
	h.noteSignIn(ctx, event, audit.Success, "")

	helpers.SendResponse(c, http.StatusOK, "OTP sent to your contact", gin.H{"type": contactType, "username": maskedContact, "uuid": issued.SessionID})
}

// @Summary Verify a sign-in code
// @Description Returns an access token, and sets the refresh token in the HttpOnly refresh_token cookie.
// @Tags sign-in
// @Accept json
// @Produce json
// @Param uuid path string true "The sign-in session from the code request" format(uuid)
// @Param body body VerifyOTPRequest true "The code"
// @Success 200 {object} models.APIResponse{info=models.AccessTokenInfo}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse{info=SessionLimitInfo} "session_limit_reached: the user has the most active sessions allowed"
// @Failure 429 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse "identity_unavailable: the mobile number is due for a check against the national ID of its account, and the provider cannot answer now"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/auth/{uuid} [post]
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

	// attempt is the audit event of a sign-in that does not succeed. It
	// targets the sign-in, which links it to its code request. Until the
	// code is right, the caller is anonymous.
	attempt := audit.FromRequest(c)
	attempt.Action, attempt.ActorType = audit.ActionUserSignIn, audit.ActorAnonymous
	attempt.TargetType, attempt.TargetID = audit.TargetSignIn, reqUUID

	// Verify ends the session on success, so a code signs in only once, even
	// when it is sent in concurrent requests.
	contact, err := h.OTP.Verify(ctx, reqUUID, req.OTP)
	switch {
	case errors.Is(err, otp.ErrTooManyAttempts):
		h.noteSignIn(ctx, attempt, audit.Denied, reasonRateLimited)
		helpers.SendResponse(c, http.StatusTooManyRequests, "Too many failed attempts. Please try again later.", nil)
		return
	case errors.Is(err, otp.ErrInvalidCode):
		h.noteSignIn(ctx, attempt, audit.Failure, "code_invalid")
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

	userHK, err := h.checkOrCreateUser(ctx, audit.FromRequest(c), contactType, contact)
	if errors.Is(err, errAccountUnavailable) || errors.Is(err, errAdminAccount) {
		// The code was right, so the refusal is the account's.
		attempt.ActorType, attempt.ActorID = h.contactOwner(ctx, contactType, contact)
	}
	if errors.Is(err, errAccountFrozen) {
		h.noteSignIn(ctx, attempt, audit.Denied, restrictions.CodeAccountFrozen)
		helpers.SendError(c, http.StatusForbidden, restrictions.CodeAccountFrozen, restrictions.MessageAccountFrozen, nil)
		return
	}
	if errors.Is(err, errAccountUnavailable) {
		h.noteSignIn(ctx, attempt, audit.Failure, helpers.CodeAccountUnavailable)
		// The code was right, so the caller controls the contact and may
		// learn why the account cannot sign in.
		helpers.SendError(c, http.StatusForbidden, helpers.CodeAccountUnavailable, "This account cannot sign in. Please contact support.", nil)
		return
	}
	var locked *platform.Locked
	if errors.As(err, &locked) {
		h.noteSignIn(ctx, attempt, audit.Denied, locked.Code())
		platform.Send(c, locked)
		return
	}
	if errors.Is(err, errIdentityUnavailable) {
		h.noteSignIn(ctx, attempt, audit.Failure, codeIdentityUnavailable)
		helpers.SendError(c, http.StatusServiceUnavailable, codeIdentityUnavailable,
			"Your mobile number cannot be checked against your national ID right now. Please request a new code later, or sign in with your email address.", nil)
		return
	}
	if errors.Is(err, errAdminAccount) {
		h.noteSignIn(ctx, attempt, audit.Failure, "use_admin_sign_in")
		helpers.SendError(c, http.StatusForbidden, "use_admin_sign_in", "Administrators must use the admin sign-in.", nil)
		return
	}
	if err != nil || userHK == uuid.Nil {
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to create user", nil)
		return
	}

	attempt.ActorType, attempt.ActorID = audit.ActorUser, userHK.String()
	h.startSession(c, ctx, userHK, "", attempt)
}

// reasonRateLimited is the audit reason of a sign-in refused by its limits.
const reasonRateLimited = "rate_limited"

// noteSignIn records event, a sign-in event that changes nothing, with its
// outcome. It is best effort: a failure is logged, so that an unavailable
// audit log cannot keep users from signing in. A client that disconnects
// does not cancel the record of its attempt.
func (h *PublicHandler) noteSignIn(ctx context.Context, event audit.Event, outcome, reason string) {
	if h.PostgresDB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	event.Outcome, event.Reason = outcome, reason
	if err := audit.Record(ctx, h.PostgresDB.DB, event); err != nil {
		log.Printf("Failed to record the audit event %s (%s): %v", event.Action, outcome, err)
	}
}

// contactOwner returns the actor type and hk of the account that owns a
// contact. Without one, or when it cannot be looked up, the actor is
// anonymous: the event is recorded either way, and never with the contact.
func (h *PublicHandler) contactOwner(ctx context.Context, contactType, contact string) (string, string) {
	if h.PostgresDB == nil || !signInContactTypes[contactType] {
		return audit.ActorAnonymous, ""
	}
	var hk uuid.UUID
	var status, role string
	var deleted, frozen bool
	err := h.PostgresDB.DB.QueryRowContext(ctx, userByContactQuery, contactType, contact).Scan(&hk, &status, &role, &deleted, &frozen)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("Failed to look up the account of a sign-in for the audit log: %v", err)
		}
		return audit.ActorAnonymous, ""
	}
	if role == auth.RoleAdmin {
		return audit.ActorAdmin, hk.String()
	}
	return audit.ActorUser, hk.String()
}

// userByContactQuery finds the user that owns a contact, through the unique
// index of user_contacts.
var userByContactQuery = `SELECT u.hk, u.status, u.role, u.deleted_at IS NOT NULL, ` + restrictions.FrozenSQL("u.hk") + `
	FROM user_contacts c JOIN users u ON u.hk = c.user_hk WHERE ` + db.ContactMatch

// signInContactTypes are the contacts that a user can sign in with.
var signInContactTypes = map[string]bool{string(account.ContactEmail): true, string(account.ContactMobile): true}

// errUserAlreadyExists reports that another request created a user with the
// same contact between the lookup and the insert.
var errUserAlreadyExists = errors.New("user with this contact already exists")

// errAccountUnavailable reports an account that is deleted, or whose status
// does not allow signing in.
var errAccountUnavailable = errors.New("account cannot sign in")

// errAccountFrozen reports an account that a restriction froze (#220).
var errAccountFrozen = fmt.Errorf("%w: frozen", errAccountUnavailable)

// errIdentityUnavailable reports a mobile number whose ownership is due for
// a recheck that the provider cannot answer now (#252).
var errIdentityUnavailable = errors.New("the mobile ownership cannot be checked now")

// codeIdentityUnavailable is the code of that refusal.
const codeIdentityUnavailable = "identity_unavailable"

// errAdminAccount reports an administrator account at user sign-in.
var errAdminAccount = errors.New("administrators cannot use user sign-in")

const uniqueViolationCode = "23505"

// checkOrCreateUser returns the user that owns a contact, and creates one
// when none does. origin holds the request's fields for the audit event of a
// new user.
func (h *PublicHandler) checkOrCreateUser(ctx context.Context, origin audit.Event, contactType, contact string) (uuid.UUID, error) {
	return h.signInUser(ctx, origin, contactType, contact, true)
}

// signInUser is checkOrCreateUser. recheck says whether the ownership of a
// mobile number may still be checked: once the number was taken off an
// account that no longer owns it, the sign-in continues without it.
func (h *PublicHandler) signInUser(ctx context.Context, origin audit.Event, contactType, contact string, recheck bool) (uuid.UUID, error) {
	log.Println("Checking if user exists or creating a new user")

	if !signInContactTypes[contactType] {
		return uuid.Nil, fmt.Errorf("unsupported contact type: %s", contactType)
	}
	var userHK uuid.UUID
	var status, role string
	var deleted, frozen bool
	find := func() error {
		return h.PostgresDB.DB.QueryRowContext(ctx, userByContactQuery, contactType, contact).Scan(&userHK, &status, &role, &deleted, &frozen)
	}

	err := find()
	if err == sql.ErrNoRows {
		log.Println("User does not exist, creating new user")
		userHK, createErr := h.createUser(ctx, origin, contactType, contact)
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
	if frozen {
		return uuid.Nil, errAccountFrozen
	}
	if role == auth.RoleAdmin {
		// Administrators sign in at /admin/auth with a second factor; a
		// user token for their account is never issued.
		return uuid.Nil, errAdminAccount
	}

	if recheck && contactType == string(account.ContactMobile) {
		moved, err := h.recheckOwnership(ctx, origin, userHK.String(), contact)
		if err != nil {
			return uuid.Nil, err
		}
		if moved {
			// The number no longer reaches that account: whoever holds it
			// now signs in as for an unknown number, to a new account.
			return h.signInUser(ctx, origin, contactType, contact, false)
		}
	}

	log.Printf("User already exists with ID: %s", userHK.String())
	return userHK, nil
}

// recheckOwnership checks, at most once every
// identity.mobile_ownership.recheck_days, that a mobile number still
// belongs to the national ID of the account ownerHK that holds it (#252).
// A number that no longer does is taken off the account, which keeps its
// data, and recheckOwnership reports true. An account without a national
// ID is at level 0 and holds no funds, so it is not checked. When the
// provider cannot answer a check that is due, the sign-in is refused
// rather than risk showing the account to a new subscriber of the number.
func (h *PublicHandler) recheckOwnership(ctx context.Context, origin audit.Event, ownerHK, mobile string) (bool, error) {
	if h.PostgresDB == nil {
		return false, nil
	}
	owner, found, err := contactclaim.FindOwner(ctx, h.PostgresDB.DB, string(account.ContactMobile), mobile)
	if err != nil || !found || owner.UserHK != ownerHK {
		return false, err
	}
	recheck := config.Config.Identity.MobileOwnership.Recheck()
	if owner.OwnershipCheckedAt != nil && time.Since(*owner.OwnershipCheckedAt) < recheck {
		return false, nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, config.Config.Identity.MobileOwnership.Timeout())
	defer cancel()
	verdict, ref, err := contactclaim.Check(checkCtx, h.Ownership, h.Sealer, h.PostgresDB.DB, ownerHK, mobile)
	if errors.Is(err, identity.ErrUnavailable) {
		log.Printf("A due ownership check of a mobile number at sign-in failed: %v", err)
		return false, errIdentityUnavailable
	}
	if err != nil {
		return false, err
	}
	switch verdict {
	case contactclaim.NoNationalID:
		return false, nil
	case contactclaim.Owned:
		if err := contactclaim.RecordCheck(ctx, h.PostgresDB.DB, ownerHK, mobile, h.Ownership.Provider(), ref); err != nil {
			log.Printf("Failed to record an ownership check: %v", err)
		}
		return false, nil
	}

	tx, err := h.PostgresDB.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := contactclaim.LockUsers(ctx, tx, ownerHK); err != nil {
		return false, err
	}
	detachOrigin := origin
	detachOrigin.ActorType, detachOrigin.ActorID = audit.ActorSystem, ""
	_, detached, err := contactclaim.Detach(ctx, tx, ownerHK, string(account.ContactMobile), mobile, contactclaim.ReasonOwnershipMismatch,
		detachOrigin, time.Duration(config.Config.Security.ChangeHoldHours)*time.Hour)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	// A concurrent sign-in may have taken it off first; either way, the
	// account no longer holds it.
	if !detached {
		log.Printf("A mobile number was already taken off its account by a concurrent sign-in")
	}
	return true, nil
}

// createUser creates an active user with its first contact, and records its
// creation, in one transaction. The contact is verified: users are created
// after a correct sign-in code, which proves control of it.
func (h *PublicHandler) createUser(ctx context.Context, origin audit.Event, contactType, contact string) (uuid.UUID, error) {
	tx, err := h.PostgresDB.DB.BeginTx(ctx, nil)
	if err != nil {
		return uuid.Nil, fmt.Errorf("start the transaction: %w", err)
	}
	defer tx.Rollback()

	// A lockdown of sign-up refuses new accounts (#235). It is checked in
	// the transaction, so that a sign-up in flight when the lockdown is
	// created either commits before it or is refused.
	if err := platform.Check(ctx, tx, platform.FeatureSignUp, platform.Subject{IP: origin.IP}); err != nil {
		return uuid.Nil, err
	}

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
	created := origin
	created.Action, created.Outcome = audit.ActionUserCreate, audit.Success
	created.ActorType, created.ActorID = audit.ActorUser, userHK.String()
	created.TargetType, created.TargetID = audit.TargetUser, userHK.String()
	if err := audit.Record(ctx, tx, created); err != nil {
		log.Printf("Failed to record the creation of a user: %v", err)
		return uuid.Nil, fmt.Errorf("failed to create user. please try again")
	}
	if err := tx.Commit(); err != nil {
		log.Printf("Failed to commit a new user: %v", err)
		return uuid.Nil, fmt.Errorf("failed to create user. please try again")
	}
	return userHK, nil
}

// @Summary Refresh the access token
// @Description Exchanges the refresh_token cookie for a new access token, and rotates the cookie. A rotated refresh token that is presented again revokes the session, except within jwt.refresh_token.reuse_grace_seconds of its rotation, when it gets a new access token and no new cookie.
// @Tags sign-in
// @Produce json
// @Success 200 {object} models.APIResponse{info=models.AccessTokenInfo}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/auth/refresh [post]
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
	return contact.Identify(input)
}

// currencyReadTimeout bounds a currency read, which may load the list from
// PostgreSQL.
const currencyReadTimeout = 10 * time.Second

// CurrencyPage is a page of public currencies. It describes
// pagination.Page[models.Currency] in the OpenAPI document, which swag
// cannot read from the generic type.
type CurrencyPage struct {
	Items []models.Currency `json:"items"`
	// NextCursor asks for the next page. It is left out on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// GetActiveCurrencies lists the public currencies, oldest first by default,
// with the filters of #218.
//
// @Summary List the public currencies
// @Description The approved currencies, oldest first by default, a page at a time. The filters are optional and combine, and an unknown parameter is refused with 400 validation_failed. Text filters ignore case, and match Persian text typed with Arabic letters, Persian or Arabic digits, or a zero-width non-joiner. With q and no sort, the currencies whose symbol is q, then those whose symbol starts with it, come first. Every instance answers the same, from the shared cache or from the database.
// @Tags currencies
// @Produce json
// @Param q query string false "Text in the symbol, English name, or Persian name, normalized; symbols that match it come first" maxLength(64)
// @Param symbol query []string false "Symbols, in any case, repeated or comma-separated" collectionFormat(csv)
// @Param en_name query string false "Text in the English name, in any case" maxLength(64)
// @Param fa_name query string false "Text in the Persian name, with Arabic letters and Persian or Arabic digits matching the Persian ones" maxLength(64)
// @Param contract query string false "The contract address, in any case" maxLength(128)
// @Param has_contract query bool false "Only currencies with a contract, or only those without"
// @Param is_token query bool false "Only tokens, or only currencies that are not"
// @Param if_fiat query bool false "Only fiat currencies, or only those that are not"
// @Param created_since query string false "Created at or after this RFC 3339 time" format(date-time)
// @Param created_until query string false "Created before this RFC 3339 time" format(date-time)
// @Param updated_since query string false "Updated at or after this RFC 3339 time" format(date-time)
// @Param favorites query bool false "true: only the favorites of the signed-in user, which needs their access token"
// @Param sort query string false "The order, with - for descending; the hk breaks ties" Enums(symbol, -symbol, en_name, -en_name, created_at, -created_at, updated_at, -updated_at)
// @Param limit query int false "The number of currencies, 50 by default" minimum(1) maximum(200)
// @Param cursor query string false "The next_cursor of the previous page"
// @Success 200 {object} models.APIResponse{info=CurrencyPage}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse "favorites=true without a valid user access token"
// @Failure 403 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/currencies [get]
func (h *PublicHandler) GetActiveCurrencies(c *gin.Context) {
	page, errs := pagination.Parse(c)
	query, queryErrs := currencies.ParseQuery(c.Request.URL.Query(), "favorites")
	onlyFavorites := false
	if raw := c.Query("favorites"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			if queryErrs == nil {
				queryErrs = map[string]string{}
			}
			queryErrs["favorites"] = "must be true or false"
		}
		onlyFavorites = v
	}
	if errs == nil {
		errs = map[string]string{}
	}
	for field, problem := range queryErrs {
		errs[field] = problem
	}
	if len(errs) > 0 {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, "Validation failed", gin.H{"errors": errs})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), currencyReadTimeout)
	defer cancel()
	if onlyFavorites && !h.favoritesOf(c, ctx, &query) {
		return
	}
	list, err := h.Currencies.List(ctx)
	if err != nil {
		log.Printf("Error listing the public currencies: %v", err)
		// The store of the currencies could not be read, as while Redis is
		// away: the client may retry (#130).
		c.Header("Retry-After", strconv.Itoa(dberr.StoreUnavailable.RetryAfterSeconds))
		helpers.SendError(c, http.StatusServiceUnavailable, dberr.CodeServiceUnavailable, "Currencies are temporarily unavailable.", nil)
		return
	}
	// A delisted currency leaves the list, but can still be read by its hk.
	list = slices.DeleteFunc(slices.Clone(list), func(c models.Currency) bool { return !currencies.Listed(c) })
	result, err := query.Page(list, page)
	if errors.Is(err, pagination.ErrInvalidCursor) {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, "Validation failed",
			gin.H{"errors": map[string]string{"cursor": "is not a cursor of this list"}})
		return
	}
	if err != nil {
		log.Printf("Error paging the public currencies: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to list currencies.", nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, "Active currencies retrieved successfully.", result)
}

// favoritesOf keeps only the favorites of the user of the request's access
// token in q (#217). The token gets the checks of the user routes. It
// answers, and reports false, when there is no valid token.
func (h *PublicHandler) favoritesOf(c *gin.Context, ctx context.Context, q *currencies.Query) bool {
	token, ok := auth.BearerToken(c.GetHeader("Authorization"))
	if !ok {
		c.Header("WWW-Authenticate", "Bearer")
		helpers.SendError(c, http.StatusUnauthorized, "token_missing", "favorites=true needs your access token.", nil)
		return false
	}
	claims, err := auth.AuthenticateUserToken(ctx, token, h.Blacklist, h.Sessions)
	switch {
	case errors.Is(err, auth.ErrAccountUnavailable):
		code, message := auth.AccountError(err)
		helpers.SendError(c, http.StatusForbidden, code, message, nil)
		return false
	case errors.Is(err, auth.ErrSessionInvalid):
		helpers.SendError(c, http.StatusUnauthorized, "session_inactive", "Unauthorized - Session is not active", nil)
		return false
	case err != nil && (errors.Is(err, auth.ErrInvalidToken) || errors.Is(err, auth.ErrWrongTokenType) || errors.Is(err, auth.ErrWrongRole) || claims == nil):
		c.Header("WWW-Authenticate", "Bearer")
		helpers.SendError(c, http.StatusUnauthorized, helpers.CodeTokenInvalid, "Unauthorized - Invalid token", nil)
		return false
	case err != nil:
		log.Printf("Failed to check the token of a favorites filter: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to list currencies.", nil)
		return false
	}
	hks, err := favorites.CurrencyHKs(ctx, h.PostgresDB.DB, claims.UUID)
	if err != nil {
		log.Printf("Failed to read the favorites of a user: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to list currencies.", nil)
		return false
	}
	q.HKs = hks
	return true
}

// GetCurrencyByHK returns one public currency. A currency that is not public
// gets the same 404 as one that does not exist.
//
// @Summary Get a public currency
// @Tags currencies
// @Produce json
// @Param hk path string true "The currency's UUID" format(uuid)
// @Success 200 {object} models.APIResponse{info=models.Currency}
// @Failure 400 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/currencies/{hk} [get]
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
		// The store of the currencies could not be read, as while Redis is
		// away: the client may retry (#130).
		c.Header("Retry-After", strconv.Itoa(dberr.StoreUnavailable.RetryAfterSeconds))
		helpers.SendError(c, http.StatusServiceUnavailable, dberr.CodeServiceUnavailable, "Currencies are temporarily unavailable.", nil)
		return
	}
	if !found {
		helpers.SendResponse(c, http.StatusNotFound, "Currency not found", nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, "Currency retrieved successfully.", currency)
}
