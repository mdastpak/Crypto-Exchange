package user

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
	"github.com/mdastpak/crypto-exchange/internal/passkey"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/totp"
	"github.com/mdastpak/crypto-exchange/internal/userauth"

	"github.com/gin-gonic/gin"
)

// StepUpRequest proves the second factor of the signed-in user again.
type StepUpRequest struct {
	// Password is the account password. Exactly one of Password and
	// TOTPCode is sent.
	Password string `json:"password,omitempty"`
	// TOTPCode is a code of the authenticator app of a user who has one.
	TOTPCode string `json:"totp_code,omitempty" example:"123456"`
	// Passkey is the answer of the device to the options of
	// POST /user/step-up/passkey. It replaces the two above.
	Passkey json.RawMessage `json:"passkey,omitempty" swaggertype:"object"`
}

// SteppedUp is the answer to a step-up.
type SteppedUp struct {
	// ElevatedUntil is until when the session may run the operations that
	// the second factor protects.
	ElevatedUntil time.Time `json:"elevated_until"`
}

// RequireSecondFactor guards a route that the second factor protects
// (#307). A user who has a factor must have proved it in this session
// within security.second_factor.step_up_minutes: at the sign-in, or with
// POST /user/step-up; otherwise the answer is 403 step_up_required. funds
// marks the routes that move funds, such as deposits and withdrawals: in
// the mode required_for_funds, a user without a factor gets 403
// second_factor_required there, and sets a password first.
//
// A handler that changes money also calls Guard.Check in its transaction,
// since the elevation may end between this check and the change.
func (h *UserHandler) RequireSecondFactor(funds bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := sessionCaller(c)
		if !ok {
			c.Abort()
			return
		}
		if h.Guard == nil || h.PostgresDB == nil {
			// Never open a protected route because its guard is missing.
			helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("SECOND_FACTOR_UNAVAILABLE"), nil)
			c.Abort()
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
		defer cancel()
		err := h.Guard.Check(ctx, h.PostgresDB.DB, claims.UUID, claims.SessionID, funds)
		var refusal *userauth.Refusal
		switch {
		case errors.As(err, &refusal) && refusal.Code == userauth.CodeSecondFactorRequired:
			helpers.SendError(c, http.StatusForbidden, refusal.Code, i18n.M("SECOND_FACTOR_REQUIRED"), nil)
			c.Abort()
			return
		case errors.As(err, &refusal):
			helpers.SendError(c, http.StatusForbidden, refusal.Code, i18n.M("STEP_UP_REQUIRED"), nil)
			c.Abort()
			return
		case err != nil:
			log.Printf("Failed to check the second factor of user %s: %v", claims.UUID, err)
			helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("SECOND_FACTOR_UNAVAILABLE"), nil)
			c.Abort()
			return
		}
		c.Next()
	}
}

// StepUp proves the second factor of the signed-in user again, and lets
// the session run the operations that it protects for a while.
//
// @Summary Prove the second factor again
// @Description The operations that the second factor protects, such as changing a contact and, once they exist, deposits and withdrawals, need a session that proved the factor within security.second_factor.step_up_minutes (5 by default). A sign-in with the password counts; afterwards this route renews it with the account password. A wrong password answers 403 password_invalid and counts toward the lock of the password: 429 password_locked with Retry-After. A user without a password gets 409 second_factor_not_set.
// @Tags user security
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body StepUpRequest true "The account password"
// @Success 200 {object} models.APIResponse{info=SteppedUp}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "password_invalid"
// @Failure 409 {object} models.APIResponse "second_factor_not_set"
// @Failure 429 {object} models.APIResponse "password_locked, or a rate limit"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/step-up [post]
func (h *UserHandler) StepUp(c *gin.Context) {
	claims, ok := sessionCaller(c)
	if !ok {
		return
	}
	var req StepUpRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	byTOTP, byPasskey := req.TOTPCode != "", len(req.Passkey) > 0
	errs := map[string]string{}
	switch {
	case byPasskey && (byTOTP || req.Password != ""):
		errs["passkey"] = "cannot be sent with password or totp_code"
	case byPasskey:
	case byTOTP && req.Password != "":
		errs["totp_code"] = "cannot be sent with password"
	case byTOTP:
		var valid bool
		if req.TOTPCode, valid = contact.Code(req.TOTPCode, totp.Digits); !valid {
			errs["totp_code"] = "must be " + strconv.Itoa(totp.Digits) + " digits"
		}
	case req.Password == "" || len(req.Password) > userauth.MaxPasswordBytes:
		errs["password"] = "is required"
	}
	if len(errs) > 0 {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": errs})
		return
	}
	if h.Passwords == nil || h.Guard == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("SECOND_FACTOR_UNAVAILABLE"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), passwordTimeout)
	defer cancel()
	event := auth.SessionEvent(c, audit.ActionUserStepUp, claims.UUID, claims.SessionID)

	var err error
	if byPasskey {
		if h.Passkeys == nil {
			helpers.SendError(c, http.StatusServiceUnavailable, codePasskeyUnavailable, i18n.M("PASSKEYS_UNAVAILABLE"), nil)
			return
		}
		err = h.Passkeys.FinishAssertion(ctx, claims.UUID, sessionScope(claims.SessionID), req.Passkey)
	} else if byTOTP {
		if h.TOTP == nil {
			helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("SECOND_FACTOR_UNAVAILABLE"), nil)
			return
		}
		err = h.TOTP.Verify(ctx, claims.UUID, req.TOTPCode)
	} else {
		err = h.Passwords.Verify(ctx, claims.UUID, req.Password)
	}
	var locked *userauth.LockedError
	switch {
	case errors.Is(err, passkey.ErrDisabled):
		helpers.SendError(c, http.StatusServiceUnavailable, codePasskeyUnavailable, i18n.M("PASSKEYS_UNAVAILABLE"), nil)
		return
	case errors.Is(err, passkey.ErrInvalid), errors.Is(err, passkey.ErrNoCeremony), errors.Is(err, passkey.ErrCloned), errors.Is(err, passkey.ErrNoPasskey):
		reason := codePasskeyInvalid
		if errors.Is(err, passkey.ErrCloned) {
			reason = "passkey_cloned"
		}
		h.noteSecurity(ctx, event, audit.Failure, reason)
		helpers.SendError(c, http.StatusForbidden, codePasskeyInvalid, i18n.M("PASSKEY_INVALID"), nil)
		return
	case errors.Is(err, userauth.ErrNoTOTP), errors.Is(err, userauth.ErrWrongTOTP):
		// A user without an authenticator answers like a wrong code.
		h.noteSecurity(ctx, event, audit.Failure, codeTOTPInvalid)
		helpers.SendError(c, http.StatusForbidden, codeTOTPInvalid, i18n.M("TOTP_INVALID"), nil)
		return
	case byTOTP && errors.As(err, &locked):
		h.noteSecurity(ctx, event, audit.Denied, codeTOTPLocked)
		c.Header("Retry-After", strconv.Itoa(max(1, int(time.Until(locked.Until).Seconds())+1)))
		helpers.SendError(c, http.StatusTooManyRequests, codeTOTPLocked, i18n.M("TOTP_LOCKED"), nil)
		return
	case errors.Is(err, userauth.ErrNoPassword):
		helpers.SendError(c, http.StatusConflict, codeSecondFactorNotSet, i18n.M("SECOND_FACTOR_NOT_SET"), nil)
		return
	case errors.Is(err, userauth.ErrWrongPassword):
		h.noteSecurity(ctx, event, audit.Failure, codePasswordInvalid)
		helpers.SendError(c, http.StatusForbidden, codePasswordInvalid, i18n.M("PASSWORD_INVALID"), nil)
		return
	case errors.As(err, &locked):
		h.noteSecurity(ctx, event, audit.Denied, codePasswordLocked)
		c.Header("Retry-After", strconv.Itoa(max(1, int(time.Until(locked.Until).Seconds())+1)))
		helpers.SendError(c, http.StatusTooManyRequests, codePasswordLocked, i18n.M("PASSWORD_LOCKED"), nil)
		return
	case errors.Is(err, rowseal.ErrUnverified):
		// A password changed outside the backend proves nothing (#139).
		h.noteSecurity(ctx, event, audit.Failure, "password_unverified")
		helpers.SendError(c, http.StatusForbidden, codePasswordInvalid, i18n.M("PASSWORD_INVALID"), nil)
		return
	case err != nil:
		log.Printf("Failed to check the password of a step-up of user %s: %v", claims.UUID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("SECOND_FACTOR_UNAVAILABLE"), nil)
		return
	}
	until, err := h.Guard.Elevate(ctx, claims.UUID, claims.SessionID)
	if err != nil {
		log.Printf("Failed to elevate a session of user %s: %v", claims.UUID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("SECOND_FACTOR_UNAVAILABLE"), nil)
		return
	}
	h.noteSecurity(ctx, event, audit.Success, "")
	helpers.SendResponse(c, http.StatusOK, i18n.M("USER_STEPPED_UP"), SteppedUp{ElevatedUntil: until})
}

// codeSecondFactorNotSet answers a step-up of a user without a factor.
const codeSecondFactorNotSet = "second_factor_not_set"

// noteSecurity records event with its outcome. It is best effort, as the
// record of a refused sign-in is.
func (h *UserHandler) noteSecurity(ctx context.Context, event audit.Event, outcome, reason string) {
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
