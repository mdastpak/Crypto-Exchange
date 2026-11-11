package user

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/userauth"

	"github.com/gin-gonic/gin"
)

// passwordTimeout bounds a request of the password routes, which hash.
const passwordTimeout = 15 * time.Second

// The codes of the password routes.
const (
	codePasswordInvalid = "password_invalid"
	codePasswordLocked  = "password_locked"
)

// SecuritySettings are the sign-in protections of the signed-in user.
type SecuritySettings struct {
	// Password is the account password, the second step of the sign-in.
	Password userauth.Status `json:"password"`
	// PasswordMinLength is the least length of a new password, in
	// characters.
	PasswordMinLength int `json:"password_min_length" example:"10"`
	// RecoveryCodesLeft is how many unused recovery codes the user has.
	RecoveryCodesLeft int `json:"recovery_codes_left" example:"10"`
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
	settings.RecoveryCodesLeft, err = h.Passwords.RecoveryCodesLeft(ctx, h.PostgresDB.DB, claims.UUID)
	if err != nil {
		log.Printf("Failed to count the recovery codes of user %s: %v", claims.UUID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_SECURITY_SETTINGS"), nil)
		return
	}
	if settings.Recovery, err = h.Passwords.OpenRecovery(ctx, claims.UUID); err != nil {
		log.Printf("Failed to read the recovery of user %s: %v", claims.UUID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_SECURITY_SETTINGS"), nil)
		return
	}
	if h.Guard != nil {
		settings.SecondFactor, err = h.Guard.State(ctx, h.PostgresDB.DB, claims.UUID, claims.SessionID)
		if err != nil {
			log.Printf("Failed to read the second factor of user %s: %v", claims.UUID, err)
			helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_SECURITY_SETTINGS"), nil)
			return
		}
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("SECURITY_SETTINGS_RETRIEVED"), settings)
}

// SetPassword sets the account password of the signed-in user, or changes
// it. From then on, the sign-in needs the password after the code. A
// change ends every other session of the user.
//
// @Summary Set or change the account password
// @Description The account password is a second step of the sign-in, after the code, never a replacement for it. The first password needs only the session; a change needs current_password. A new password has at least security.password.min_length characters (10 by default) and at most 256 bytes, and at least five different characters. A change ends every other session of the user. A wrong current password answers 403 password_invalid and counts toward the lock of the password: 429 password_locked with Retry-After.
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
	event := audit.FromRequest(c)
	event.ActorType, event.ActorID = audit.ActorUser, claims.UUID

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
	if h.PostgresDB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	event.Action, event.Outcome, event.Reason = audit.ActionUserPasswordChange, outcome, reason
	event.TargetType, event.TargetID = audit.TargetUser, event.ActorID
	if err := audit.Record(ctx, h.PostgresDB.DB, event); err != nil {
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
