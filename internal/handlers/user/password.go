package user

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/clientip"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/userauth"

	"github.com/gin-gonic/gin"
)

// passwordTimeout bounds a request of the password routes, which hash.
const passwordTimeout = 15 * time.Second

// The codes of the password routes.
const (
	codePasswordInvalid = "password_invalid"
	codePasswordLocked  = "password_locked"
	// The proof of the first password (#401).
	codeContactProofRequired = "contact_proof_required"
	codeContactCodeInvalid   = "contact_code_invalid"
	codePasswordAlreadySet   = "password_set"
)

// passwordProofPrefix is the Redis key prefix of the code that proves the
// first password of an account, one per session.
const passwordProofPrefix = "password_proof:"

// SecuritySettings are the sign-in protections of the signed-in user.
type SecuritySettings struct {
	// Password is the account password, the second step of the sign-in.
	Password userauth.Status `json:"password"`
	// PasswordMinLength is the least length of a new password, in
	// characters.
	PasswordMinLength int `json:"password_min_length" example:"10"`
	// RecoveryCodesLeft is how many unused recovery codes the user has.
	RecoveryCodesLeft int `json:"recovery_codes_left" example:"10"`
	// TOTPEnabled is whether the user has an authenticator app, which
	// proves the second step instead of the password.
	TOTPEnabled bool `json:"totp_enabled"`
	// Passkeys is how many passkeys the user has, and PasskeysAvailable
	// whether this deployment offers them.
	Passkeys          int  `json:"passkeys" example:"1"`
	PasskeysAvailable bool `json:"passkeys_available"`
	// SecondFactor is where this session stands with the second factor.
	SecondFactor userauth.State `json:"second_factor"`
	// Recovery is the account recovery in progress, if any: someone
	// proved the contacts of the account to replace its password. Proving
	// the password, as POST /user/step-up does, cancels it.
	Recovery *userauth.Recovery `json:"recovery,omitempty"`
}

// SetPasswordRequest sets the account password, or changes it.
type SetPasswordRequest struct {
	// CurrentPassword is required to change a password, and left out to
	// set the first one.
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
	// ContactCode is required to set the first password: the code that
	// POST /user/password/code sent to a verified contact of the account.
	ContactCode string `json:"contact_code,omitempty" example:"123456"`
}

// PasswordCodeRequest asks for the code that proves the first password.
type PasswordCodeRequest struct {
	// SendTo is the verified contact that receives the code: email or
	// mobile. By default, the first verified contact of the account.
	SendTo string `json:"send_to,omitempty" example:"email" enums:"email,mobile"`
}

// PasswordCodeSent is the answer to a request of the code of the first
// password.
type PasswordCodeSent struct {
	// CodeSentTo is the masked contact that received the code.
	CodeSentTo string `json:"code_sent_to" example:"ali*****@example.com"`
	// ExpiresIn is the number of seconds the code is valid.
	ExpiresIn int `json:"expires_in" example:"60"`
}

// PasswordChanged is the answer to setting or changing the password.
type PasswordChanged struct {
	Password userauth.Status `json:"password"`
	// SessionsEnded is how many other sessions the change ended.
	SessionsEnded int `json:"sessions_ended" example:"1"`
	// SecurityHoldUntil is until when withdrawals are held after this
	// change of the second factor.
	SecurityHoldUntil time.Time `json:"security_hold_until"`
	// RecoveryCodes come with the first password, once: ten single-use
	// codes, one of which resets a forgotten password. They are not
	// stored, so the user must keep them.
	RecoveryCodes []string `json:"recovery_codes,omitempty"`
}

// GetSecurity returns the sign-in protections of the signed-in user.
//
// @Summary Get the sign-in protections of the signed-in user
// @Description Whether the account has a password, the second step of its sign-in, and since when.
// @Tags user security
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=SecuritySettings}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/security [get]
func (h *UserHandler) GetSecurity(c *gin.Context) {
	claims, ok := sessionCaller(c)
	if !ok {
		return
	}
	if h.Passwords == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("FAILED_TO_READ_SECURITY_SETTINGS"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	status, err := h.Passwords.Status(ctx, claims.UUID)
	if err != nil {
		log.Printf("Failed to read the security settings of user %s: %v", claims.UUID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_SECURITY_SETTINGS"), nil)
		return
	}
	settings := SecuritySettings{Password: status, PasswordMinLength: h.Passwords.MinPasswordLength()}
	settings.RecoveryCodesLeft, err = h.Passwords.CodesLeft(ctx, claims.UUID)
	if err != nil {
		log.Printf("Failed to count the recovery codes of user %s: %v", claims.UUID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_SECURITY_SETTINGS"), nil)
		return
	}
	if h.TOTP != nil {
		// An authenticator that fails its seal check is not shown as on
		// (#337); the check recorded the finding.
		if settings.TOTPEnabled, err = h.TOTP.Enabled(ctx, claims.UUID); errors.Is(err, rowseal.ErrUnverified) {
			settings.TOTPEnabled = false
		} else if err != nil {
			log.Printf("Failed to read the authenticator of user %s: %v", claims.UUID, err)
			helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_SECURITY_SETTINGS"), nil)
			return
		}
	}
	if h.Passkeys != nil && h.Passkeys.Enabled() {
		settings.PasskeysAvailable = true
		if settings.Passkeys, err = h.Passkeys.Count(ctx, claims.UUID); err != nil {
			log.Printf("Failed to read the passkeys of user %s: %v", claims.UUID, err)
			helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_SECURITY_SETTINGS"), nil)
			return
		}
	}
	if settings.Recovery, err = h.Passwords.OpenRecovery(ctx, claims.UUID); err != nil {
		log.Printf("Failed to read the recovery of user %s: %v", claims.UUID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_SECURITY_SETTINGS"), nil)
		return
	}
	if h.Guard != nil {
		settings.SecondFactor, err = h.Guard.StateOf(ctx, claims.UUID, claims.SessionID)
		if err != nil {
			log.Printf("Failed to read the second factor of user %s: %v", claims.UUID, err)
			helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_SECURITY_SETTINGS"), nil)
			return
		}
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("SECURITY_SETTINGS_RETRIEVED"), settings)
}

// SendPasswordCode sends the code that proves the first password of the
// signed-in user to a verified contact of the account (#401). A session
// alone cannot give an account its first second factor: the password ends
// every other session and becomes the second step of the sign-in, so
// someone who stole a session would otherwise lock the owner out.
//
// The code is a session of the contact codes, bound to this session in
// Redis, which every instance shares, so that PUT /user/password may reach
// another instance. A new request replaces the code of the session.
//
// @Summary Send the code of the first password
// @Description Sends a code to a verified contact of the account (send_to, or the first one), which PUT /user/password needs as contact_code to set the first password. The code is valid for expires_in seconds and works only in this session; a new request replaces it. 409 password_set answers an account that has a password: a change needs current_password instead.
// @Tags user security
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body PasswordCodeRequest false "The contact that receives the code"
// @Success 200 {object} models.APIResponse{info=PasswordCodeSent}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "password_set, contact_not_found, or channel_unavailable"
// @Failure 429 {object} models.APIResponse "rate_limited"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/password/code [post]
func (h *UserHandler) SendPasswordCode(c *gin.Context) {
	claims, ok := sessionCaller(c)
	if !ok {
		return
	}
	var req PasswordCodeRequest
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
			return
		}
	}
	if req.SendTo != "" && req.SendTo != notify.ContactEmail && req.SendTo != notify.ContactMobile {
		sendContactValidation(c, map[string]string{"send_to": "must be email or mobile"})
		return
	}
	if h.Passwords == nil || !h.contactsReady(c) {
		if h.Passwords == nil {
			helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("FAILED_TO_SET_PASSWORD"), nil)
		}
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	has, err := h.Passwords.Has(ctx, claims.UUID)
	if errors.Is(err, rowseal.ErrUnverified) {
		// The password is gone outside the backend (#337).
		helpers.SendError(c, http.StatusForbidden, helpers.CodeAccountUnavailable, i18n.M("ACCOUNT_CANNOT_SIGN_IN"), nil)
		return
	}
	if err != nil {
		log.Printf("Failed to read the password of user %s: %v", claims.UUID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_SEND_THE_CODE"), nil)
		return
	}
	if has {
		helpers.SendError(c, http.StatusConflict, codePasswordAlreadySet, i18n.M("PASSWORD_ALREADY_SET"), nil)
		return
	}
	contacts, _, err := h.Store.Contacts(ctx, claims.UUID)
	if err != nil {
		log.Printf("Failed to read the contacts of user %s: %v", claims.UUID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_SEND_THE_CODE"), nil)
		return
	}
	var to storedContact
	if req.SendTo != "" {
		to, ok = findContact(contacts, req.SendTo)
	} else if ok = len(contacts) > 0; ok {
		to = contacts[0]
	}
	if !ok {
		helpers.SendError(c, http.StatusConflict, codeContactNotFound, i18n.M("VERIFYING_CONTACT_NOT_YOURS"), nil)
		return
	}
	if !h.OTPSender.Supports(to.typ) {
		helpers.SendError(c, http.StatusConflict, codeChannelMissing, i18n.M("CODES_CANNOT_BE_SENT_BY", i18n.A("channel", i18n.Key("CHANNEL_"+strings.ToUpper(to.typ)))), nil)
		return
	}
	client := clientip.Key(c.ClientIP(), h.settings().RateLimits.IPv6PrefixLength)
	issued, err := h.issue(ctx, claims.UUID, to.value, to.typ, client)
	if sendIssueError(c, err) {
		return
	}
	key := passwordProofPrefix + claims.SessionID
	pipe := h.Redis.TxPipeline()
	pipe.Del(ctx, key)
	pipe.HSet(ctx, key, map[string]interface{}{"user": claims.UUID, "session": issued.SessionID, "contact": to.value})
	pipe.Expire(ctx, key, h.ContactOTP.TTL())
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("Failed to store the password code of user %s: %v", claims.UUID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_SEND_THE_CODE"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("PASSWORD_CODE_SENT"),
		PasswordCodeSent{CodeSentTo: maskContact(to.typ, to.value), ExpiresIn: int(h.ContactOTP.TTL().Seconds())})
}

// errProofRequired and errProofInvalid are the refusals of a first
// password without its proof.
var (
	errProofRequired = &errChange{status: http.StatusForbidden, code: codeContactProofRequired, message: i18n.M("PASSWORD_CODE_REQUIRED")}
	errProofInvalid  = &errChange{status: http.StatusForbidden, code: codeContactCodeInvalid, message: i18n.M("INVALID_OR_EXPIRED_CODE")}
)

// proveFirstPassword checks the code of the first password of the session
// and uses it up. Exactly one request uses a code: the one that deletes
// it from Redis.
func (h *UserHandler) proveFirstPassword(ctx context.Context, claims *auth.Claims, code string) error {
	if code == "" || h.ContactOTP == nil || h.Redis == nil {
		return errProofRequired
	}
	key := passwordProofPrefix + claims.SessionID
	fields, err := h.Redis.HGetAll(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("read the password code: %w", err)
	}
	if len(fields) == 0 || fields["user"] != claims.UUID {
		return errProofRequired
	}
	got, err := h.ContactOTP.Verify(ctx, fields["session"], code)
	switch {
	case errors.Is(err, otp.ErrInvalidCode):
		return errProofInvalid
	case errors.Is(err, otp.ErrTooManyAttempts):
		h.Redis.Del(ctx, key)
		return &errChange{status: http.StatusTooManyRequests, code: codeRateLimited, message: i18n.M("TOO_MANY_WRONG_CODES")}
	case err != nil:
		return fmt.Errorf("verify the password code: %w", err)
	}
	if !strings.EqualFold(got, fields["contact"]) {
		return errProofInvalid
	}
	deleted, err := h.Redis.Del(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("use the password code: %w", err)
	}
	if deleted == 0 {
		return errProofRequired
	}
	return nil
}

// SetPassword sets the account password of the signed-in user, or changes
// it. From then on, the sign-in needs the password after the code. A
// change ends every other session of the user.
//
// @Summary Set or change the account password
// @Description The account password is a second step of the sign-in, after the code, never a replacement for it. The first password needs contact_code, the code that POST /user/password/code sent to a verified contact of the account: 403 contact_proof_required without one, 403 contact_code_invalid for a wrong or expired one, and 429 rate_limited after too many. A change needs current_password. A new password has at least security.password.min_length characters (10 by default) and at most 256 bytes, and at least five different characters. A change ends every other session of the user. A wrong current password answers 403 password_invalid and counts toward the lock of the password: 429 password_locked with Retry-After.
// @Tags user security
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body SetPasswordRequest true "The new password, and the current one for a change"
// @Success 200 {object} models.APIResponse{info=PasswordChanged}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "password_invalid: the current password is wrong"
// @Failure 429 {object} models.APIResponse "password_locked, or a rate limit"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/password [put]
func (h *UserHandler) SetPassword(c *gin.Context) {
	claims, ok := sessionCaller(c)
	if !ok {
		return
	}
	var req SetPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	if h.Passwords == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("FAILED_TO_SET_PASSWORD"), nil)
		return
	}
	invalid := func(field, problem string) {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"),
			gin.H{"errors": map[string]string{field: problem}})
	}
	if req.NewPassword == "" {
		invalid("new_password", "is required")
		return
	}
	if len(req.CurrentPassword) > userauth.MaxPasswordBytes {
		invalid("current_password", "is too long")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), passwordTimeout)
	defer cancel()
	// A change proves the current password in this session, which must not
	// cancel a recovery that began after it (#401).
	ctx = userauth.InSession(ctx, claims.SessionID)
	event := audit.FromRequest(c)
	event.ActorType, event.ActorID = audit.ActorUser, claims.UUID

	if req.CurrentPassword == "" {
		// The first password needs a fresh proof of a verified contact
		// (#401). The policy is checked first, so that a password it
		// refuses does not use up the code.
		if err := userauth.CheckPolicy(req.NewPassword, h.Passwords.MinPasswordLength()); err == nil {
			// A password that fails its seal check is refused by Set below.
			has, err := h.Passwords.Has(ctx, claims.UUID)
			if err != nil && !errors.Is(err, rowseal.ErrUnverified) {
				log.Printf("Failed to read the password of user %s: %v", claims.UUID, err)
				helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_SET_PASSWORD"), nil)
				return
			}
			if err == nil && !has {
				err := h.proveFirstPassword(ctx, claims, req.ContactCode)
				if errors.Is(err, errProofRequired) || errors.Is(err, errProofInvalid) {
					h.notePassword(ctx, event, audit.Denied, codeContactProofRequired)
				}
				if sendRefusal(c, err) {
					return
				}
				if err != nil {
					log.Printf("Failed to check the password code of user %s: %v", claims.UUID, err)
					helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_SET_PASSWORD"), nil)
					return
				}
			}
		}
	}

	result, err := h.Passwords.Set(ctx, claims.UUID, req.CurrentPassword, req.NewPassword, event)
	var locked *userauth.LockedError
	switch {
	case errors.Is(err, userauth.ErrPasswordTooShort):
		invalid("new_password", fmt.Sprintf("must be at least %d characters", h.Passwords.MinPasswordLength()))
		return
	case errors.Is(err, userauth.ErrPasswordTooLong):
		invalid("new_password", fmt.Sprintf("must be valid text of at most %d bytes", userauth.MaxPasswordBytes))
		return
	case errors.Is(err, userauth.ErrPasswordTooSimple):
		invalid("new_password", "must have at least five different characters")
		return
	case errors.Is(err, userauth.ErrCurrentRequired):
		invalid("current_password", "is required to change the password")
		return
	case errors.Is(err, userauth.ErrSamePassword):
		invalid("new_password", "must differ from the current password")
		return
	case errors.Is(err, userauth.ErrWrongPassword):
		h.notePassword(ctx, event, audit.Failure, codePasswordInvalid)
		helpers.SendError(c, http.StatusForbidden, codePasswordInvalid, i18n.M("CURRENT_PASSWORD_INVALID"), nil)
		return
	case errors.As(err, &locked):
		h.notePassword(ctx, event, audit.Denied, codePasswordLocked)
		c.Header("Retry-After", strconv.Itoa(max(1, int(time.Until(locked.Until).Seconds())+1)))
		helpers.SendError(c, http.StatusTooManyRequests, codePasswordLocked, i18n.M("PASSWORD_LOCKED"), nil)
		return
	case errors.Is(err, rowseal.ErrUnverified):
		// The password is gone, or changed, outside the backend (#337): a
		// new one would hide it.
		h.notePassword(ctx, event, audit.Failure, helpers.CodeAccountUnavailable)
		helpers.SendError(c, http.StatusForbidden, helpers.CodeAccountUnavailable, i18n.M("ACCOUNT_CANNOT_SIGN_IN"), nil)
		return
	case err != nil:
		log.Printf("Failed to set the password of user %s: %v", claims.UUID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_SET_PASSWORD"), nil)
		return
	}

	// Whoever else holds a session of the account signed in without the
	// new password. The password is already changed, so a failure here is
	// reported and the user can end the sessions again.
	ended := 0
	if h.Sessions != nil {
		revoke := auth.SessionEvent(c, audit.ActionUserSessionRevoke, claims.UUID, "")
		ended, err = h.Sessions.RevokeOthers(ctx, claims.UUID, claims.SessionID, revoke)
		if err != nil {
			log.Printf("Failed to end the other sessions of user %s after a password change: %v", claims.UUID, err)
		}
	}
	status, err := h.Passwords.Status(ctx, claims.UUID)
	if err != nil {
		status = userauth.Status{Set: true}
	}
	message := i18n.M("PASSWORD_SET")
	if result.Changed {
		message = i18n.M("PASSWORD_CHANGED")
	}
	helpers.SendResponse(c, http.StatusOK, message, PasswordChanged{Password: status, SessionsEnded: ended,
		SecurityHoldUntil: result.HoldUntil, RecoveryCodes: result.RecoveryCodes})
}

// notePassword records a refused change of the password. It is best
// effort, as the record of a refused sign-in is.
func (h *UserHandler) notePassword(ctx context.Context, event audit.Event, outcome, reason string) {
	if h.Audit == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	event.Action, event.Outcome, event.Reason = audit.ActionUserPasswordChange, outcome, reason
	event.TargetType, event.TargetID = audit.TargetUser, event.ActorID
	if err := h.Audit.Record(ctx, event); err != nil {
		log.Printf("Failed to record the audit event %s (%s): %v", event.Action, outcome, err)
	}
}

// RecoveryCodes are new recovery codes of the signed-in user.
type RecoveryCodes struct {
	// RecoveryCodes are ten single-use codes. They are shown once.
	RecoveryCodes []string `json:"recovery_codes"`
}

// GenerateRecoveryCodes replaces the recovery codes of the signed-in user.
//
// @Summary Generate new recovery codes
// @Description Replaces the ten recovery codes of the account, one of which resets a forgotten password. The codes that were not used stop working, and the new ones are shown once. It needs a fresh proof of the password (403 step_up_required), and a user without a password gets 409 second_factor_not_set.
// @Tags user security
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=RecoveryCodes}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 409 {object} models.APIResponse "second_factor_not_set"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/recovery-codes [post]
func (h *UserHandler) GenerateRecoveryCodes(c *gin.Context) {
	claims, ok := sessionCaller(c)
	if !ok {
		return
	}
	if h.Passwords == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("SECOND_FACTOR_UNAVAILABLE"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	event := audit.FromRequest(c)
	event.ActorType, event.ActorID = audit.ActorUser, claims.UUID
	codes, err := h.Passwords.GenerateRecoveryCodes(ctx, claims.UUID, event)
	if errors.Is(err, userauth.ErrNoPassword) {
		helpers.SendError(c, http.StatusConflict, codeSecondFactorNotSet, i18n.M("SECOND_FACTOR_NOT_SET"), nil)
		return
	}
	if err != nil {
		log.Printf("Failed to generate the recovery codes of user %s: %v", claims.UUID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_GENERATE_RECOVERY_CODES"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("RECOVERY_CODES_GENERATED"), RecoveryCodes{RecoveryCodes: codes})
}
