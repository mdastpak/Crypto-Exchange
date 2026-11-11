package user

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/contact"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/totp"
	"github.com/mdastpak/crypto-exchange/internal/userauth"

	"github.com/gin-gonic/gin"
)

const (
	codeTOTPInvalid    = "totp_invalid"
	codeTOTPLocked     = "totp_locked"
	codeTOTPEnabled    = "totp_enabled"
	codeTOTPNotEnabled = "totp_not_enabled"
)

// TOTPCodeRequest carries a code of the authenticator app.
type TOTPCodeRequest struct {
	Code string `json:"code" example:"123456"`
}

// TOTPChanged is the answer to turning the authenticator on or off.
type TOTPChanged struct {
	// SecurityHoldUntil is when the security hold on withdrawals ends.
	SecurityHoldUntil time.Time `json:"security_hold_until"`
}

// totpCaller returns the caller of an authenticator route and its audit
// event. It answers, and reports false, when the route cannot run.
func (h *UserHandler) totpCaller(c *gin.Context) (string, audit.Event, bool) {
	claims, ok := sessionCaller(c)
	if !ok {
		return "", audit.Event{}, false
	}
	if h.TOTP == nil || h.Passwords == nil || h.PostgresDB == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("SECOND_FACTOR_UNAVAILABLE"), nil)
		return "", audit.Event{}, false
	}
	event := audit.FromRequest(c)
	event.ActorType, event.ActorID = audit.ActorUser, claims.UUID
	event.TargetType, event.TargetID = audit.TargetUser, claims.UUID
	return claims.UUID, event, true
}

// @Summary Start enrolling an authenticator app
// @Description Gives the user a new secret for an authenticator app (TOTP: RFC 6238, SHA-1, 6 digits, 30 seconds), as text and as an otpauth URI for a QR code. The authenticator works once POST /user/totp/confirm has checked its first code; until then a new call replaces the secret. It comes after the account password, 409 second_factor_not_set without one, and needs a fresh proof of it: 403 step_up_required. 409 totp_enabled answers a user who has an authenticator.
// @Tags user-security
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=userauth.TOTPEnrollment}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "second_factor_required or step_up_required"
// @Failure 409 {object} models.APIResponse "totp_enabled or second_factor_not_set"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/totp [post]
func (h *UserHandler) BeginTOTP(c *gin.Context) {
	userHK, _, ok := h.totpCaller(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	// The authenticator comes after the password: an account that has one
	// always has a password and its recovery codes to fall back on.
	if has, err := h.Passwords.Has(ctx, userHK); err != nil {
		log.Printf("Failed to read the password of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("SECOND_FACTOR_UNAVAILABLE"), nil)
		return
	} else if !has {
		helpers.SendError(c, http.StatusConflict, codeSecondFactorNotSet, i18n.M("SECOND_FACTOR_NOT_SET"), nil)
		return
	}
	// The app shows the account under a contact of the user.
	var account string
	if err := h.PostgresDB.DB.QueryRowContext(ctx, `SELECT value FROM user_contacts
		WHERE user_hk = $1 AND removed_at IS NULL AND type IN ('email', 'mobile')
		ORDER BY is_primary DESC, id LIMIT 1`, userHK).Scan(&account); err != nil {
		account = "account"
	}
	enrollment, err := h.TOTP.Begin(ctx, userHK, account)
	switch {
	case errors.Is(err, userauth.ErrTOTPEnabled):
		helpers.SendError(c, http.StatusConflict, codeTOTPEnabled, i18n.M("TOTP_ALREADY_ENABLED"), nil)
		return
	case err != nil:
		log.Printf("Failed to start the authenticator of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("SECOND_FACTOR_UNAVAILABLE"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("TOTP_ENROLLMENT_STARTED"), enrollment)
}

// @Summary Turn the authenticator app on
// @Description Checks the first code of the secret that POST /user/totp gave, and turns the authenticator on. From then on the user can prove the second step with a code of the app instead of the account password: at a sign-in, at a step-up, and to reset a forgotten password. It is a change of the second factor, so the security hold starts. 403 totp_invalid answers a wrong code; 429 totp_locked after too many.
// @Tags user-security
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body TOTPCodeRequest true "The code that the app shows"
// @Success 200 {object} models.APIResponse{info=TOTPChanged}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "totp_invalid, second_factor_required, or step_up_required"
// @Failure 409 {object} models.APIResponse "totp_not_enabled: no secret waits for its code"
// @Failure 429 {object} models.APIResponse "totp_locked"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/totp/confirm [post]
func (h *UserHandler) ConfirmTOTP(c *gin.Context) {
	userHK, event, ok := h.totpCaller(c)
	if !ok {
		return
	}
	var req TOTPCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	code, valid := contact.Code(req.Code, totp.Digits)
	if !valid {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"),
			gin.H{"errors": map[string]string{"code": "must be " + strconv.Itoa(totp.Digits) + " digits"}})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	event.Action = audit.ActionUserTOTPEnable
	holdUntil, err := h.TOTP.Confirm(ctx, userHK, code, event)
	var locked *userauth.LockedError
	switch {
	case errors.Is(err, userauth.ErrNoTOTP):
		helpers.SendError(c, http.StatusConflict, codeTOTPNotEnabled, i18n.M("TOTP_NOT_STARTED"), nil)
		return
	case errors.Is(err, userauth.ErrWrongTOTP):
		h.noteSecurity(ctx, event, audit.Failure, codeTOTPInvalid)
		helpers.SendError(c, http.StatusForbidden, codeTOTPInvalid, i18n.M("TOTP_INVALID"), nil)
		return
	case errors.As(err, &locked):
		h.noteSecurity(ctx, event, audit.Denied, codeTOTPLocked)
		c.Header("Retry-After", strconv.Itoa(max(1, int(time.Until(locked.Until).Seconds())+1)))
		helpers.SendError(c, http.StatusTooManyRequests, codeTOTPLocked, i18n.M("TOTP_LOCKED"), nil)
		return
	case err != nil:
		log.Printf("Failed to confirm the authenticator of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("SECOND_FACTOR_UNAVAILABLE"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("TOTP_ENABLED"), TOTPChanged{SecurityHoldUntil: holdUntil})
}

// @Summary Turn the authenticator app off
// @Description Removes the authenticator of the user, also one that waits for its first code. The account password stays the second step. It needs a fresh proof of the second factor, and starts the security hold.
// @Tags user-security
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=TOTPChanged}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "second_factor_required or step_up_required"
// @Failure 409 {object} models.APIResponse "totp_not_enabled"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/totp [delete]
func (h *UserHandler) RemoveTOTP(c *gin.Context) {
	userHK, event, ok := h.totpCaller(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	holdUntil, err := h.TOTP.Remove(ctx, userHK, event)
	switch {
	case errors.Is(err, userauth.ErrNoTOTP):
		helpers.SendError(c, http.StatusConflict, codeTOTPNotEnabled, i18n.M("TOTP_NOT_ENABLED"), nil)
		return
	case err != nil:
		log.Printf("Failed to remove the authenticator of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("SECOND_FACTOR_UNAVAILABLE"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("TOTP_DISABLED"), TOTPChanged{SecurityHoldUntil: holdUntil})
}
