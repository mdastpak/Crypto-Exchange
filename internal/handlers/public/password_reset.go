package public

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/contact"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/totp"
	"github.com/mdastpak/crypto-exchange/internal/userauth"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// PasswordResetRequest resets a forgotten account password.
type PasswordResetRequest struct {
	// Challenge is the challenge that a correct code answered with.
	Challenge string `json:"challenge"`
	// RecoveryCode is one of the unused recovery codes of the account.
	// Exactly one of RecoveryCode and TOTPCode is sent.
	RecoveryCode string `json:"recovery_code,omitempty" example:"ABCD-EFGH-JKLM-NPQR"`
	// TOTPCode is a code of the authenticator app, for an account whose
	// password_reset lists "totp".
	TOTPCode string `json:"totp_code,omitempty" example:"123456"`
	// Passkey is the answer of the device to the options of
	// POST /public/auth/second-step/passkey, for an account whose
	// password_reset lists "passkey". It replaces the two above.
	Passkey     json.RawMessage `json:"passkey,omitempty" swaggertype:"object"`
	NewPassword string          `json:"new_password"`
}

const codeRecoveryCodeInvalid = "recovery_code_invalid"

// sessionEnder ends the sessions of a user but one.
type sessionEnder interface {
	RevokeOthers(ctx context.Context, userID, keep string, event audit.Event) (int, error)
}

// @Summary Reset a forgotten account password
// @Description A user who forgot the account password proves the account twice: with the code sent to a contact, which answered 202 with a challenge, and with one of the recovery codes, or with a code of the authenticator app (totp_code) for an account whose password_reset lists totp, or with a passkey (passkey) for one that lists passkey. The new password follows the policy of PUT /user/password. The recovery code is used up, every session of the user ends, the security hold starts, and the answer signs in. A wrong recovery code answers 401 recovery_code_invalid, leaves the challenge usable, and counts toward the lock of the password: 429 password_locked with Retry-After. An account without unused recovery codes needs account recovery.
// @Tags sign-in
// @Accept json
// @Produce json
// @Param body body PasswordResetRequest true "The challenge, a recovery code, and the new password"
// @Success 200 {object} models.APIResponse{info=models.AccessTokenInfo} "The access token. The refresh token is set as an HttpOnly cookie."
// @Header 200 {string} Set-Cookie "The refresh token, HttpOnly"
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse "challenge_invalid, recovery_code_invalid, totp_invalid, or passkey_invalid"
// @Failure 403 {object} models.APIResponse
// @Failure 429 {object} models.APIResponse "password_locked, or a rate limit"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/auth/password-reset [post]
func (h *PublicHandler) ResetPassword(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	var req PasswordResetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	errs := map[string]string{}
	if req.Challenge == "" || len(req.Challenge) > 128 {
		errs["challenge"] = "is required"
	}
	byTOTP, byPasskey := req.TOTPCode != "", len(req.Passkey) > 0
	switch {
	case byPasskey && (byTOTP || req.RecoveryCode != ""):
		errs["passkey"] = "cannot be sent with recovery_code or totp_code"
	case byPasskey:
	case byTOTP && req.RecoveryCode != "":
		errs["totp_code"] = "cannot be sent with recovery_code"
	case byTOTP:
		var ok bool
		if req.TOTPCode, ok = contact.Code(req.TOTPCode, totp.Digits); !ok {
			errs["totp_code"] = "must be " + strconv.Itoa(totp.Digits) + " digits"
		}
	case req.RecoveryCode == "" || len(req.RecoveryCode) > 64:
		errs["recovery_code"] = "is required"
	}
	if req.NewPassword == "" {
		errs["new_password"] = "is required"
	}
	invalid := func() {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": errs})
	}
	if len(errs) > 0 {
		invalid()
		return
	}
	if h.RedisClient == nil || h.PostgresDB == nil || h.Passwords == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("SIGN_IN_IS_NOT_AVAILABLE"), nil)
		return
	}

	userHK, key, attempt, ok := h.challengedUser(c, ctx, req.Challenge)
	if !ok {
		return
	}
	attempt.Action = audit.ActionUserPasswordReset
	event := audit.FromRequest(c)
	event.ActorType, event.ActorID = audit.ActorUser, userHK.String()

	var err error
	if byTOTP || byPasskey {
		// The policy first: a password that is refused uses no proof.
		if err = userauth.CheckPolicy(req.NewPassword, h.Passwords.MinPasswordLength()); err == nil {
			if byPasskey && !h.verifyPasskey(c, ctx, userHK, req.Challenge, req.Passkey, attempt) {
				return
			}
			if byTOTP && !h.verifyTOTP(c, ctx, userHK, req.TOTPCode, attempt) {
				return
			}
			_, err = h.Passwords.ResetWithFactor(ctx, userHK.String(), req.NewPassword, event)
		}
	} else {
		_, err = h.Passwords.Reset(ctx, userHK.String(), req.RecoveryCode, req.NewPassword, event)
	}
	var locked *userauth.LockedError
	switch {
	case errors.Is(err, userauth.ErrPasswordTooShort):
		errs["new_password"] = "must be at least " + strconv.Itoa(h.Passwords.MinPasswordLength()) + " characters"
		invalid()
		return
	case errors.Is(err, userauth.ErrPasswordTooLong):
		errs["new_password"] = "must be valid text of at most " + strconv.Itoa(userauth.MaxPasswordBytes) + " bytes"
		invalid()
		return
	case errors.Is(err, userauth.ErrPasswordTooSimple):
		errs["new_password"] = "must have at least five different characters"
		invalid()
		return
	case errors.As(err, &locked):
		h.noteSignIn(ctx, attempt, audit.Denied, codePasswordLocked)
		c.Header("Retry-After", strconv.Itoa(max(1, int(time.Until(locked.Until).Seconds())+1)))
		helpers.SendError(c, http.StatusTooManyRequests, codePasswordLocked, i18n.M("PASSWORD_LOCKED"), nil)
		return
	case errors.Is(err, userauth.ErrWrongRecoveryCode):
		h.noteSignIn(ctx, attempt, audit.Failure, codeRecoveryCodeInvalid)
		helpers.SendError(c, http.StatusUnauthorized, codeRecoveryCodeInvalid, i18n.M("RECOVERY_CODE_INVALID"), nil)
		return
	case errors.Is(err, userauth.ErrNoPassword):
		// Nothing to reset: the challenge was for a password that the
		// account no longer has.
		helpers.SendError(c, http.StatusUnauthorized, "challenge_invalid", i18n.M("CHALLENGE_INVALID_OR_EXPIRED"), nil)
		return
	case errors.Is(err, rowseal.ErrUnverified):
		// The password row was changed outside the backend (#139). A
		// reset would seal over it, so it waits for an administrator.
		h.noteSignIn(ctx, attempt, audit.Failure, helpers.CodeAccountUnavailable)
		helpers.SendError(c, http.StatusForbidden, helpers.CodeAccountUnavailable, i18n.M("ACCOUNT_CANNOT_SIGN_IN"), nil)
		return
	case err != nil:
		log.Printf("Failed to reset the password of a user: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_RESET_PASSWORD"), nil)
		return
	}

	// The password is reset. The challenge is spent, and whoever held a
	// session of the account signed in with the old password, or without
	// one.
	if err := h.RedisClient.Client.Del(ctx, key).Err(); err != nil {
		log.Printf("Failed to remove a second-step challenge after a reset: %v", err)
	}
	if ender, ok := h.Sessions.(sessionEnder); ok {
		revoke := auth.SessionEvent(c, audit.ActionUserSessionRevoke, userHK.String(), "")
		if _, err := ender.RevokeOthers(ctx, userHK.String(), uuid.Nil.String(), revoke); err != nil {
			// Never a new session beside the ones that the reset was
			// meant to end.
			log.Printf("Failed to end the sessions of a user after a password reset: %v", err)
			helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_RESET_PASSWORD"), nil)
			return
		}
	}
	attempt.Action = audit.ActionUserSignIn
	h.startSession(c, ctx, userHK, "", attempt)
}
