package user

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/clientip"
	"github.com/mdastpak/crypto-exchange/internal/confirm"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/gin-gonic/gin"
)

// The error codes of the confirmations (#161).
const (
	CodeConfirmationCodeInvalid       = "confirmation_code_invalid"
	CodeConfirmationLocked            = "confirmation_locked"
	CodeConfirmationExpired           = "confirmation_expired"
	CodeConfirmationAlreadyConfirmed  = "confirmation_already_confirmed"
	CodeConfirmationMethodUnavailable = "confirmation_method_unavailable"
	CodeConfirmationMethodRequired    = "confirmation_method_required"
	codeConfirmationsUnavailable      = "confirmations_unavailable"
)

// ConfirmationMethodRequest chooses how withdrawals are confirmed.
type ConfirmationMethodRequest struct {
	Method confirm.Method `json:"method" enums:"passkey,totp,sms,email" example:"totp"`
}

// ConfirmRequest is the code of a confirmation.
type ConfirmRequest struct {
	// Code is the code of an SMS, email, or authenticator confirmation.
	Code string `json:"code,omitempty" example:"482913"`
	// Passkey is the answer of the device to the options of a
	// confirmation by passkey (#373). It replaces the code.
	Passkey json.RawMessage `json:"passkey,omitempty" swaggertype:"object"`
}

func (h *UserHandler) confirmationsCaller(c *gin.Context) (string, bool) {
	claims, ok := sessionCaller(c)
	if !ok {
		return "", false
	}
	if h.Confirmations == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codeConfirmationsUnavailable, i18n.M("CONFIRMATIONS_UNAVAILABLE"), nil)
		return "", false
	}
	return claims.UUID, true
}

// GetConfirmationMethod returns how the signed-in user confirms
// withdrawals (#161).
//
// @Summary Get my confirmation method
// @Description How the withdrawals of the signed-in user are confirmed: the method they chose, the one that confirmations use (the chosen one while it is available, or else the first of totp, sms, and email that is), and the ones available. totp needs an authenticator app (POST /user/totp), sms a verified mobile number and the SMS template of withdrawals, and email a verified email address.
// @Tags user-security
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=confirm.Settings}
// @Failure 401 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse "confirmations_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/confirmation-method [get]
func (h *UserHandler) GetConfirmationMethod(c *gin.Context) {
	userHK, ok := h.confirmationsCaller(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	st, err := h.Confirmations.Settings(ctx, userHK)
	if err != nil {
		log.Printf("Failed to read the confirmation method of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_SECURITY_SETTINGS"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("CONFIRMATION_METHOD_RETRIEVED"), st)
}

// SetConfirmationMethod chooses how the signed-in user confirms withdrawals
// (#161).
//
// @Summary Choose my confirmation method
// @Description Chooses totp, sms, or email for the confirmations of withdrawals. The method must be available (409 confirmation_method_unavailable), and the change needs a fresh proof of the second factor (403 step_up_required). Audited.
// @Tags user-security
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body ConfirmationMethodRequest true "The method"
// @Success 200 {object} models.APIResponse{info=confirm.Settings}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 409 {object} models.APIResponse "confirmation_method_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/confirmation-method [put]
func (h *UserHandler) SetConfirmationMethod(c *gin.Context) {
	userHK, ok := h.confirmationsCaller(c)
	if !ok {
		return
	}
	var req ConfirmationMethodRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	if err := h.Confirmations.SetMethod(ctx, userHK, req.Method, audit.FromRequest(c)); err != nil {
		sendConfirmationError(c, userHK, err)
		return
	}
	st, err := h.Confirmations.Settings(ctx, userHK)
	if err != nil {
		log.Printf("Failed to read the confirmation method of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_SECURITY_SETTINGS"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("CONFIRMATION_METHOD_SET"), st)
}

// ConfirmOperation confirms an operation of the signed-in user, such as a
// withdrawal, with its code (#161).
//
// @Summary Confirm an operation with its code
// @Description Checks the code of a confirmation: the code sent by SMS or email, or a code of the authenticator app. The confirmation is bound to its operation, so the code confirms that withdrawal and no other, once. A wrong code answers 403 confirmation_code_invalid; after five, or when the code store or the authenticator locks, 429 confirmation_locked. A confirmation lasts as long as a code. The codes are never logged or audited.
// @Tags user-withdrawals
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "The id of the confirmation"
// @Param body body ConfirmRequest true "The code"
// @Success 200 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "confirmation_code_invalid"
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "confirmation_expired or confirmation_already_confirmed"
// @Failure 429 {object} models.APIResponse "confirmation_locked"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/confirmations/{id} [post]
func (h *UserHandler) ConfirmOperation(c *gin.Context) {
	userHK, ok := h.confirmationsCaller(c)
	if !ok {
		return
	}
	var req ConfirmRequest
	if err := c.ShouldBindJSON(&req); err != nil || (req.Code == "" && len(req.Passkey) == 0) {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	proof := confirm.Proof{Code: req.Code, Passkey: req.Passkey}
	if err := h.Confirmations.Confirm(ctx, userHK, c.Param("id"), proof, audit.FromRequest(c)); err != nil {
		sendConfirmationError(c, userHK, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("OPERATION_CONFIRMED"), nil)
}

// ResendConfirmation sends a new code of an SMS or email confirmation
// (#161).
//
// @Summary Send a new confirmation code
// @Description Sends a new code of a confirmation by SMS or email, which also gives it a new lifetime. The limits of the codes apply (429 confirmation_locked). A confirmation by the authenticator app has nothing to send (409 confirmation_method_unavailable).
// @Tags user-withdrawals
// @Produce json
// @Security BearerAuth
// @Param id path string true "The id of the confirmation"
// @Success 200 {object} models.APIResponse{info=confirm.Challenge}
// @Failure 401 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "confirmation_already_confirmed or confirmation_method_unavailable"
// @Failure 429 {object} models.APIResponse "confirmation_locked"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/confirmations/{id}/resend [post]
func (h *UserHandler) ResendConfirmation(c *gin.Context) {
	userHK, ok := h.confirmationsCaller(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	challenge, err := h.Confirmations.Resend(ctx, userHK, c.Param("id"), clientip.Key(c.ClientIP(), h.settings().RateLimits.IPv6PrefixLength))
	if err != nil {
		sendConfirmationError(c, userHK, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("CONFIRMATION_CODE_SENT"), challenge)
}

// sendConfirmationError answers a refusal of the confirmations. The
// withdrawal workflow (#70) answers with it too.
func sendConfirmationError(c *gin.Context, userHK string, err error) {
	switch {
	case errors.Is(err, confirm.ErrNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("CONFIRMATION_NOT_FOUND"), nil)
	case errors.Is(err, confirm.ErrInvalidCode):
		helpers.SendError(c, http.StatusForbidden, CodeConfirmationCodeInvalid, i18n.M("CONFIRMATION_CODE_INVALID"), nil)
	case errors.Is(err, confirm.ErrTooManyAttempts):
		helpers.SendError(c, http.StatusTooManyRequests, CodeConfirmationLocked, i18n.M("CONFIRMATION_LOCKED"), nil)
	case errors.Is(err, confirm.ErrExpired):
		helpers.SendError(c, http.StatusConflict, CodeConfirmationExpired, i18n.M("CONFIRMATION_EXPIRED"), nil)
	case errors.Is(err, confirm.ErrAlreadyConfirmed):
		helpers.SendError(c, http.StatusConflict, CodeConfirmationAlreadyConfirmed, i18n.M("CONFIRMATION_ALREADY_CONFIRMED"), nil)
	case errors.Is(err, confirm.ErrMethodUnavailable):
		helpers.SendError(c, http.StatusConflict, CodeConfirmationMethodUnavailable, i18n.M("CONFIRMATION_METHOD_UNAVAILABLE"), nil)
	case errors.Is(err, confirm.ErrNoMethod):
		helpers.SendError(c, http.StatusConflict, CodeConfirmationMethodRequired, i18n.M("CONFIRMATION_METHOD_REQUIRED"), nil)
	case errors.Is(err, rowseal.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, "row_unverified", i18n.M("CONFIRMATION_UNVERIFIED"), nil)
	default:
		log.Printf("Failed to handle a confirmation of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CONFIRM"), nil)
	}
}
