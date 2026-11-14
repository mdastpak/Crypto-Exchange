package public

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/antiphishing"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/clientip"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/contact"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/dberr"
	"github.com/mdastpak/crypto-exchange/internal/geoip"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/pagination"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/signin"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// PublicHandler serves the public routes: sign-in, its second step, the
// password reset, the account recovery, and the public currencies. Every
// dependency is an interface (deps.go, #48); a nil one makes its routes
// unavailable.
type PublicHandler struct {
	// Config is the configuration of the process (#48). NewHandler sets it;
	// nil reads as the zero configuration.
	Config *config.Configuration
	// Accounts finds and creates the accounts of the sign-ins. Without it,
	// sign-in is unavailable.
	Accounts Accounts
	// AntiPhishing reads the anti-phishing code that the sign-in code of a
	// user's account shows (#375). Nil shows none.
	AntiPhishing antiphishing.Reader
	// Audit records the events of the sign-ins.
	Audit AuditLog
	// Redis holds the challenges of the second step and of the session
	// limit, which any instance finishes.
	Redis    redis.UniversalClient
	Sessions auth.SessionStore
	// AuthenticateToken checks the optional token of the favorites filter:
	// its signature, the blacklist, and its session.
	AuthenticateToken func(ctx context.Context, token string) (*auth.Claims, error)
	// OTP issues and verifies sign-in codes.
	OTP CodeStore
	// OTPSender delivers sign-in codes. It discards them unless the server
	// replaces it with the configured delivery.
	OTPSender notify.OTPSender
	// Currencies lists the public currencies.
	Currencies CurrencyCatalog
	// Platform serves the status of the platform controls (#235). Without
	// it, the status is open.
	Platform StatusSource
	// Ownership checks, for a strict account recovery, that a mobile
	// number belongs to the national ID of its account (#252, #253).
	Ownership identity.MobileOwnership
	// Passwords checks the account passwords, the second step of a sign-in
	// (#307). The server sets it, with the slow hash that this package
	// must not import. Without it, a code alone signs in.
	Passwords PasswordStore
	// TOTP checks the authenticator apps, a second step that a user can
	// prove instead of the password.
	TOTP AuthenticatorStore
	// GeoIP finds the country of a sign-up (#316). Nil knows none.
	GeoIP *geoip.Resolver
	// Passkeys checks the passkeys, another second step that a user can
	// prove instead of the password.
	Passkeys PasskeyVerifier
	// Guard elevates the session of a sign-in that proved the second
	// factor, for the operations that it protects.
	Guard Elevator
}

// NewHandler returns the handler with the stores of the services in is. A
// service that is not there leaves its dependencies nil, never a typed nil.
func NewHandler(is *models.InfraServices) *PublicHandler {
	h := &PublicHandler{
		Config:    is.Settings(),
		OTPSender: notify.DiscardSender{},
	}
	if is.Postgres != nil {
		h.Accounts = &signin.Accounts{DB: is.Postgres.DB, Config: is.Settings()}
		h.Audit = audit.Log{DB: is.Postgres.DB}
		h.Sessions = auth.NewPostgresSessionStore(is.Postgres.DB)
	}
	if catalog := currencies.ForServices(is); catalog != nil {
		h.Currencies = catalog
	}
	if is.Redis != nil {
		h.Redis = is.Redis.Client
		h.OTP = otp.NewStore(is.Redis.Client, otp.SettingsOf(is.Settings()))
	}
	return h
}

type OTPRequest struct {
	Username string `json:"username"`
}

type VerifyOTPRequest struct {
	UUID string `json:"uuid"`
	OTP  string `json:"otp"`
	// Accept is, for a contact without an account, the versions of the
	// legal documents that its holder accepted, by kind. A client that
	// does not know them leaves it out, and gets 428 signup_terms_required
	// with the documents.
	Accept map[string]string `json:"accept,omitempty" example:"terms:2026-11,privacy:2026-11"`
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
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("EMAIL_OR_MOBILE_REQUIRED"), nil)
		return
	}
	// Every form of a contact that users type, such as a mobile number in
	// Persian digits or with +98, reaches the same account (#197).
	req.Username = contact.Normalize(req.Username)

	contactType, maskedContact := IdentifyInputType(req.Username)
	if contactType == "" {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_CONTACT_INFORMATION"), nil)
		return
	}
	if !h.OTPSender.Supports(contactType) {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("SIGN_IN_BY_NOT_AVAILABLE", i18n.A("channel", i18n.Key("CHANNEL_"+strings.ToUpper(contactType)))), nil)
		return
	}

	// The code request is the account's, when the contact belongs to one,
	// though anyone may make it. The event never holds the contact.
	event := audit.FromRequest(c)
	event.Action = audit.ActionUserSignInCode
	event.ActorType, event.ActorID = h.contactOwner(ctx, contactType, req.Username)

	// The per-contact limits count by the client that asks, so that a
	// stranger cannot use up the budget of the contact's owner (#187).
	issued, err := h.OTP.Issue(ctx, req.Username, clientip.Key(c.ClientIP(), h.settings().RateLimits.IPv6PrefixLength))
	switch {
	case errors.Is(err, otp.ErrTooManyRequests):
		h.noteSignIn(ctx, event, audit.Denied, reasonRateLimited)
		helpers.SendResponse(c, http.StatusTooManyRequests, i18n.M("TOO_MANY_SIGN_IN_CODES"), nil)
		return
	case errors.Is(err, otp.ErrTooManyAttempts):
		h.noteSignIn(ctx, event, audit.Denied, reasonRateLimited)
		helpers.SendResponse(c, http.StatusTooManyRequests, i18n.M("TOO_MANY_FAILED_ATTEMPTS"), nil)
		return
	case err != nil:
		log.Printf("Failed to issue a sign-in code for %s %s: %v", contactType, maskedContact, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_STORE_OTP"), nil)
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
		// Only a verified email of a user's account shows the code: it
		// reaches the owner, whoever asked for it.
		AntiPhishingCode: h.antiPhishingCode(ctx, event.ActorType, event.ActorID, contactType, req.Username),
	})
	if err != nil {
		log.Printf("Failed to send the sign-in code to %s %s: %v", contactType, maskedContact, err)
		h.noteSignIn(ctx, event, audit.Failure, "delivery_failed")
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_SEND_OTP"), nil)
		return
	}
	h.noteSignIn(ctx, event, audit.Success, "")

	helpers.SendResponse(c, http.StatusOK, i18n.M("OTP_SENT_TO_YOUR_CONTACT"), gin.H{"type": contactType, "username": maskedContact, "uuid": issued.SessionID})
}

// @Summary Verify a sign-in code
// @Description Returns an access token, and sets the refresh token in the HttpOnly refresh_token cookie.
// @Tags sign-in
// @Accept json
// @Produce json
// @Param uuid path string true "The sign-in session from the code request" format(uuid)
// @Param body body VerifyOTPRequest true "The code"
// @Success 200 {object} models.APIResponse{info=models.AccessTokenInfo}
// @Success 202 {object} models.APIResponse{info=SecondStepInfo} "The code is right and the account has a second step: no token, and a challenge for POST /public/auth/second-step"
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse{info=SessionLimitInfo} "session_limit_reached: the user has the most active sessions allowed"
// @Failure 428 {object} models.APIResponse{info=SignUpTermsInfo} "signup_terms_required: the contact has no account, and one is created with the accepted legal documents; see POST /public/auth/sign-up"
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
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("UUID_AND_OTP_REQUIRED"), nil)
		return
	}

	if !validateUUID(reqUUID) {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_UUID_FORMAT"), nil)
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
		helpers.SendResponse(c, http.StatusTooManyRequests, i18n.M("TOO_MANY_FAILED_ATTEMPTS"), nil)
		return
	case errors.Is(err, otp.ErrInvalidCode):
		h.noteSignIn(ctx, attempt, audit.Failure, "code_invalid")
		helpers.SendError(c, http.StatusUnauthorized, "code_invalid", i18n.M("INVALID_OR_EXPIRED_OTP_CODE"), nil)
		return
	case err != nil:
		log.Printf("Failed to verify a sign-in code: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_VERIFY_OTP"), nil)
		return
	}

	contactType, _ := IdentifyInputType(contact)
	if contactType == "" {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_CONTACT_INFORMATION"), nil)
		return
	}

	h.finishSignIn(c, ctx, contactType, contact, h.signUpOf(c, req.Accept), attempt)
}

// finishSignIn signs in the holder of a contact whose code was right, to
// its account or to a new one, and answers. signUp is what a new account
// needs. attempt is the audit event of a refusal.
func (h *PublicHandler) finishSignIn(c *gin.Context, ctx context.Context, contactType, contact string, signUp signUpInfo, attempt audit.Event) {
	userHK, err := h.checkOrCreateUser(ctx, audit.FromRequest(c), contactType, contact, signUp)
	if errors.Is(err, errTermsRequired) {
		// The code was right and the contact has no account: one is
		// created only once its holder accepted the documents (#316).
		h.askSignUpTerms(c, ctx, contactType, contact, attempt)
		return
	}
	if errors.Is(err, errSignUpCountry) {
		h.noteSignIn(ctx, attempt, audit.Denied, codeSignUpCountry)
		helpers.SendError(c, http.StatusForbidden, codeSignUpCountry, i18n.M("SIGN_UP_COUNTRY_UNSUPPORTED"), nil)
		return
	}
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
		helpers.SendError(c, http.StatusForbidden, helpers.CodeAccountUnavailable, i18n.M("ACCOUNT_CANNOT_SIGN_IN"), nil)
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
			i18n.M("SIGN_IN_MOBILE_CHECK_UNAVAILABLE"), nil)
		return
	}
	if errors.Is(err, errAdminAccount) {
		h.noteSignIn(ctx, attempt, audit.Failure, "use_admin_sign_in")
		helpers.SendError(c, http.StatusForbidden, "use_admin_sign_in", i18n.M("ADMINS_USE_ADMIN_SIGN_IN"), nil)
		return
	}
	if err != nil || userHK == uuid.Nil {
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_USER"), nil)
		return
	}

	attempt.ActorType, attempt.ActorID = audit.ActorUser, userHK.String()
	// A user with a second step gets no token for the code alone (#307).
	method, err := h.sessionMethod(ctx, userHK)
	if errors.Is(err, rowseal.ErrUnverified) {
		// The password of the account is gone while its sealed record says
		// it exists (#337), or the account changed outside the backend: the
		// code alone never signs in to it.
		h.noteSignIn(ctx, attempt, audit.Failure, helpers.CodeAccountUnavailable)
		helpers.SendError(c, http.StatusForbidden, helpers.CodeAccountUnavailable, i18n.M("ACCOUNT_CANNOT_SIGN_IN"), nil)
		return
	}
	if err != nil {
		// Never a session without knowing whether a second step applies.
		log.Printf("Failed to read the second step of a sign-in: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return
	}
	if method != methodCode {
		h.askSecondStep(c, ctx, userHK, contactType, contact, attempt)
		return
	}
	h.startSession(c, ctx, userHK, "", attempt)
}

// reasonRateLimited is the audit reason of a sign-in refused by its limits.
const reasonRateLimited = "rate_limited"

// noteSignIn records event, a sign-in event that changes nothing, with its
// outcome. It is best effort: a failure is logged, so that an unavailable
// audit log cannot keep users from signing in. A client that disconnects
// does not cancel the record of its attempt.
func (h *PublicHandler) noteSignIn(ctx context.Context, event audit.Event, outcome, reason string) {
	if h.Audit == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	event.Outcome, event.Reason = outcome, reason
	if err := h.Audit.Record(ctx, event); err != nil {
		log.Printf("Failed to record the audit event %s (%s): %v", event.Action, outcome, err)
	}
}

// antiPhishingCode returns the anti-phishing code of a user's account, for
// a sign-in code to a verified email of the account (#375), or "" for none.
// An account that cannot be read shows none: the code still goes out.
func (h *PublicHandler) antiPhishingCode(ctx context.Context, actorType, userHK, contactType, contact string) string {
	if h.AntiPhishing == nil || actorType != audit.ActorUser {
		return ""
	}
	phrase, err := h.AntiPhishing.ShowTo(ctx, userHK, contactType, contact)
	if err != nil {
		log.Printf("Failed to read the anti-phishing code of user %s for a sign-in code: %v", userHK, err)
		return ""
	}
	return phrase
}

// contactOwner returns the actor type and hk of the account that owns a
// contact, for the audit event of a sign-in; see signin.Accounts.Owner.
func (h *PublicHandler) contactOwner(ctx context.Context, contactType, contact string) (string, string) {
	if h.Accounts == nil {
		return audit.ActorAnonymous, ""
	}
	return h.Accounts.Owner(ctx, contactType, contact)
}

// The refusals of a sign-in, from signin.
var (
	errAccountUnavailable  = signin.ErrAccountUnavailable
	errAccountFrozen       = signin.ErrAccountFrozen
	errIdentityUnavailable = signin.ErrIdentityUnavailable
	errAdminAccount        = signin.ErrAdminAccount
	errTermsRequired       = signin.ErrTermsRequired
	errSignUpCountry       = signin.ErrSignUpCountry
)

// codeIdentityUnavailable is the code of the refusal of a mobile number
// whose ownership cannot be checked now (#252).
const codeIdentityUnavailable = "identity_unavailable"

// checkOrCreateUser returns the user that owns a contact, and creates one
// when none does; see signin.Accounts.FindOrCreate. origin holds the
// request's fields for the audit event of a new user.
func (h *PublicHandler) checkOrCreateUser(ctx context.Context, origin audit.Event, contactType, contact string, signUp signUpInfo) (uuid.UUID, error) {
	if h.Accounts == nil {
		return uuid.Nil, errors.New("sign-in is not available")
	}
	return h.Accounts.FindOrCreate(ctx, origin, contactType, contact, signUp)
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
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": errs})
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
		helpers.SendError(c, http.StatusServiceUnavailable, dberr.CodeServiceUnavailable, i18n.M("CURRENCIES_UNAVAILABLE"), nil)
		return
	}
	// A delisted currency leaves the list, but can still be read by its hk.
	list = slices.DeleteFunc(slices.Clone(list), func(c models.Currency) bool { return !currencies.Listed(c) })
	result, err := query.Page(list, page)
	if errors.Is(err, pagination.ErrInvalidCursor) {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"),
			gin.H{"errors": map[string]string{"cursor": "is not a cursor of this list"}})
		return
	}
	if err != nil {
		log.Printf("Error paging the public currencies: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LIST_CURRENCIES"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("ACTIVE_CURRENCIES_RETRIEVED_SUCCESSFULLY"), result)
}

// favoritesOf keeps only the favorites of the user of the request's access
// token in q (#217). The token gets the checks of the user routes. It
// answers, and reports false, when there is no valid token.
func (h *PublicHandler) favoritesOf(c *gin.Context, ctx context.Context, q *currencies.Query) bool {
	token, ok := auth.BearerToken(c.GetHeader("Authorization"))
	if !ok {
		c.Header("WWW-Authenticate", "Bearer")
		helpers.SendError(c, http.StatusUnauthorized, "token_missing", i18n.M("FAVORITES_FILTER_NEEDS_TOKEN"), nil)
		return false
	}
	claims, err := h.AuthenticateToken(ctx, token)
	switch {
	case errors.Is(err, auth.ErrAccountUnavailable):
		code, message := auth.AccountError(err)
		helpers.SendError(c, http.StatusForbidden, code, message, nil)
		return false
	case errors.Is(err, auth.ErrSessionInvalid):
		helpers.SendError(c, http.StatusUnauthorized, "session_inactive", i18n.M("UNAUTHORIZED_SESSION_IS_NOT_ACTIVE"), nil)
		return false
	case err != nil && (errors.Is(err, auth.ErrInvalidToken) || errors.Is(err, auth.ErrWrongTokenType) || errors.Is(err, auth.ErrWrongRole) || claims == nil):
		c.Header("WWW-Authenticate", "Bearer")
		helpers.SendError(c, http.StatusUnauthorized, helpers.CodeTokenInvalid, i18n.M("UNAUTHORIZED_INVALID_TOKEN"), nil)
		return false
	case err != nil:
		log.Printf("Failed to check the token of a favorites filter: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LIST_CURRENCIES"), nil)
		return false
	}
	hks, err := h.Accounts.FavoriteCurrencies(ctx, claims.UUID)
	if err != nil {
		log.Printf("Failed to read the favorites of a user: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LIST_CURRENCIES"), nil)
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
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_CURRENCY_ID"), nil)
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
		helpers.SendError(c, http.StatusServiceUnavailable, dberr.CodeServiceUnavailable, i18n.M("CURRENCIES_UNAVAILABLE"), nil)
		return
	}
	if !found {
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("CURRENCY_NOT_FOUND"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("CURRENCY_RETRIEVED_SUCCESSFULLY"), currency)
}

// zeroConfig is the configuration of a handler without one.
var zeroConfig config.Configuration

// settings returns the configuration of the handler, or the zero
// configuration without one.
func (h *PublicHandler) settings() *config.Configuration {
	if h.Config == nil {
		return &zeroConfig
	}
	return h.Config
}
