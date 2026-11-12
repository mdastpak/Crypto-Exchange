package user

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/passkey"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/gin-gonic/gin"
)

const (
	codePasskeyInvalid     = "passkey_invalid"
	codePasskeyUnavailable = "passkey_unavailable"
	codePasskeyNotFound    = "passkey_not_found"     //nolint:gosec // G101: the name of a code, not a credential.
	codePasskeyLimit       = "passkey_limit_reached" //nolint:gosec // G101: the name of a code, not a credential.
	codePasskeyExists      = "passkey_exists"
)

// AddPasskeyRequest finishes the registration of a passkey.
type AddPasskeyRequest struct {
	// Name is what the user calls the passkey, such as the device.
	Name string `json:"name" example:"iPhone"`
	// Credential is the answer of navigator.credentials.create to the
	// options of POST /user/passkey-registration.
	Credential json.RawMessage `json:"credential" swaggertype:"object"`
}

// PasskeyAdded is the answer to a new passkey.
type PasskeyAdded struct {
	Passkey passkey.Passkey `json:"passkey"`
	// SecurityHoldUntil is when the security hold on withdrawals ends.
	SecurityHoldUntil time.Time `json:"security_hold_until"`
}

// PasskeyList lists the passkeys of the user.
type PasskeyList struct {
	Passkeys []passkey.Passkey `json:"passkeys"`
	// Max is how many passkeys an account can have.
	Max int `json:"max" example:"10"`
}

// sessionScope names a session to the passkeys: a proof that began in one
// session finishes in no other.
func sessionScope(sessionID string) string { return "session:" + sessionID }

// passkeyCaller returns the caller of a passkey route and its audit event.
// It answers, and reports false, when the route cannot run.
func (h *UserHandler) passkeyCaller(c *gin.Context) (userHK, sessionID string, event audit.Event, ok bool) {
	claims, ok := sessionCaller(c)
	if !ok {
		return "", "", audit.Event{}, false
	}
	if h.Passkeys == nil || !h.Passkeys.Enabled() || h.Passwords == nil || h.Store == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codePasskeyUnavailable, i18n.M("PASSKEYS_UNAVAILABLE"), nil)
		return "", "", audit.Event{}, false
	}
	event = audit.FromRequest(c)
	event.ActorType, event.ActorID = audit.ActorUser, claims.UUID
	event.TargetType, event.TargetID = audit.TargetUser, claims.UUID
	return claims.UUID, claims.SessionID, event, true
}

// @Summary Start adding a passkey
// @Description Answers the options for navigator.credentials.create: a passkey of the device, such as Face ID, a fingerprint, Windows Hello, or a security key. The device must verify its holder. The answer of the device goes to POST /user/passkeys within five minutes. A passkey comes after the account password, 409 second_factor_not_set without one, and needs a fresh proof of the second factor: 403 step_up_required.
// @Tags user-security
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=passkey.Options} "The options, under publicKey"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 409 {object} models.APIResponse "second_factor_not_set or passkey_limit_reached"
// @Failure 503 {object} models.APIResponse "passkey_unavailable: passkeys are not configured"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/passkey-registration [post]
func (h *UserHandler) BeginPasskey(c *gin.Context) {
	userHK, _, _, ok := h.passkeyCaller(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	// A passkey comes after the password: an account that has one always
	// has a password and its recovery codes to fall back on.
	if has, err := h.Passwords.Has(ctx, userHK); errors.Is(err, rowseal.ErrUnverified) {
		helpers.SendError(c, http.StatusForbidden, helpers.CodeAccountUnavailable, i18n.M("ACCOUNT_CANNOT_SIGN_IN"), nil)
		return
	} else if err != nil {
		log.Printf("Failed to read the password of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("SECOND_FACTOR_UNAVAILABLE"), nil)
		return
	} else if !has {
		helpers.SendError(c, http.StatusConflict, codeSecondFactorNotSet, i18n.M("SECOND_FACTOR_NOT_SET"), nil)
		return
	}
	// The prompt shows the account under a contact of the user.
	account := h.Store.AccountName(ctx, userHK)
	options, err := h.Passkeys.BeginRegistration(ctx, userHK, account)
	switch {
	case errors.Is(err, passkey.ErrTooMany):
		helpers.SendError(c, http.StatusConflict, codePasskeyLimit, i18n.M("PASSKEY_LIMIT_REACHED"), gin.H{"max": passkey.MaxPerUser})
		return
	case err != nil:
		log.Printf("Failed to begin a passkey of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("SECOND_FACTOR_UNAVAILABLE"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("PASSKEY_OPTIONS"), options)
}

// @Summary Add a passkey
// @Description Checks the answer of the device to the options of POST /user/passkey-registration and adds the passkey. From then on the user can prove the second step with it instead of the account password: at a sign-in, at a step-up, and to reset a forgotten password. It is a change of the second factor, so the security hold starts. An account has at most ten passkeys.
// @Tags user-security
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body AddPasskeyRequest true "A name, and the answer of the device"
// @Success 201 {object} models.APIResponse{info=PasskeyAdded}
// @Failure 400 {object} models.APIResponse "validation_failed, or passkey_invalid: the answer does not match the registration"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 409 {object} models.APIResponse "passkey_exists or passkey_limit_reached"
// @Failure 503 {object} models.APIResponse "passkey_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/passkeys [post]
func (h *UserHandler) AddPasskey(c *gin.Context) {
	userHK, _, event, ok := h.passkeyCaller(c)
	if !ok {
		return
	}
	var req AddPasskeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	errs := map[string]string{}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || utf8.RuneCountInString(req.Name) > passkey.MaxNameLength || strings.ContainsFunc(req.Name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		errs["name"] = "must be 1 to 64 characters"
	}
	if len(req.Credential) == 0 {
		errs["credential"] = "is required"
	}
	if len(errs) > 0 {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": errs})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	event.Action = audit.ActionUserPasskeyAdd
	added, holdUntil, err := h.Passkeys.FinishRegistration(ctx, userHK, req.Name, req.Credential, event)
	switch {
	case errors.Is(err, passkey.ErrInvalid), errors.Is(err, passkey.ErrNoCeremony):
		h.noteSecurity(ctx, event, audit.Failure, codePasskeyInvalid)
		helpers.SendError(c, http.StatusBadRequest, codePasskeyInvalid, i18n.M("PASSKEY_INVALID"), nil)
		return
	case errors.Is(err, passkey.ErrExists):
		helpers.SendError(c, http.StatusConflict, codePasskeyExists, i18n.M("PASSKEY_EXISTS"), nil)
		return
	case errors.Is(err, passkey.ErrTooMany):
		helpers.SendError(c, http.StatusConflict, codePasskeyLimit, i18n.M("PASSKEY_LIMIT_REACHED"), gin.H{"max": passkey.MaxPerUser})
		return
	case err != nil:
		log.Printf("Failed to add a passkey of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("SECOND_FACTOR_UNAVAILABLE"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusCreated, i18n.M("PASSKEY_ADDED"), PasskeyAdded{Passkey: added, SecurityHoldUntil: holdUntil})
}

// @Summary List the passkeys of the signed-in user
// @Description The passkeys of the user, oldest first, without their keys. A passkey whose row was changed outside the backend is left out, and proves nothing.
// @Tags user-security
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=PasskeyList}
// @Failure 401 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/passkeys [get]
func (h *UserHandler) ListPasskeys(c *gin.Context) {
	claims, ok := sessionCaller(c)
	if !ok {
		return
	}
	list := PasskeyList{Passkeys: []passkey.Passkey{}, Max: passkey.MaxPerUser}
	if h.Passkeys != nil && h.Passkeys.Enabled() {
		ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
		defer cancel()
		passkeys, err := h.Passkeys.List(ctx, claims.UUID)
		if err != nil {
			log.Printf("Failed to list the passkeys of user %s: %v", claims.UUID, err)
			helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_SECURITY_SETTINGS"), nil)
			return
		}
		list.Passkeys = append(list.Passkeys, passkeys...)
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("PASSKEYS_LISTED"), list)
}

// @Summary Remove a passkey
// @Description Removes a passkey of the user. The account password stays the second step. It needs a fresh proof of the second factor, and starts the security hold.
// @Tags user-security
// @Produce json
// @Security BearerAuth
// @Param id path string true "The ID of the passkey"
// @Success 200 {object} models.APIResponse{info=TOTPChanged}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 404 {object} models.APIResponse "passkey_not_found"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/passkeys/{id} [delete]
func (h *UserHandler) RemovePasskey(c *gin.Context) {
	userHK, _, event, ok := h.passkeyCaller(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	holdUntil, err := h.Passkeys.Remove(ctx, userHK, c.Param("id"), event)
	switch {
	case errors.Is(err, passkey.ErrNoPasskey):
		helpers.SendError(c, http.StatusNotFound, codePasskeyNotFound, i18n.M("PASSKEY_NOT_FOUND"), nil)
		return
	case err != nil:
		log.Printf("Failed to remove a passkey of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("SECOND_FACTOR_UNAVAILABLE"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("PASSKEY_REMOVED"), TOTPChanged{SecurityHoldUntil: holdUntil})
}

// @Summary Get the options of a passkey proof for a step-up
// @Description Answers the options for navigator.credentials.get, bound to the session. The answer of the device goes, as passkey, to POST /user/step-up within five minutes, once.
// @Tags user-security
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=passkey.Options} "The options, under publicKey"
// @Failure 401 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "passkey_not_found: the user has no passkey"
// @Failure 503 {object} models.APIResponse "passkey_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/step-up/passkey [post]
func (h *UserHandler) StepUpPasskeyOptions(c *gin.Context) {
	userHK, sessionID, _, ok := h.passkeyCaller(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	options, err := h.Passkeys.BeginAssertion(ctx, userHK, sessionScope(sessionID))
	switch {
	case errors.Is(err, passkey.ErrNoPasskey):
		helpers.SendError(c, http.StatusConflict, codePasskeyNotFound, i18n.M("PASSKEY_NOT_FOUND"), nil)
		return
	case err != nil:
		log.Printf("Failed to begin the passkey proof of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("SECOND_FACTOR_UNAVAILABLE"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("PASSKEY_OPTIONS"), options)
}
