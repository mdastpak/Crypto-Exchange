package user

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/mdastpak/crypto-exchange/internal/antiphishing"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/gin-gonic/gin"
)

// The error codes of the anti-phishing code (#375).
const (
	CodeAntiPhishingCodeInvalid = "anti_phishing_code_invalid"
	CodeAntiPhishingCodeSame    = "anti_phishing_code_same"
	CodeAntiPhishingCodeNotSet  = "anti_phishing_code_not_set"
	codeAntiPhishingUnavailable = "anti_phishing_unavailable"
)

// AntiPhishingCodeRequest sets the anti-phishing code.
type AntiPhishingCodeRequest struct {
	// Code is 4 to 20 letters, digits, spaces, or hyphens, of any script.
	Code string `json:"code" binding:"required" example:"blue falcon 7"`
}

func (h *UserHandler) antiPhishingCaller(c *gin.Context) (string, bool) {
	claims, ok := sessionCaller(c)
	if !ok {
		return "", false
	}
	if h.AntiPhishing == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codeAntiPhishingUnavailable, i18n.M("ANTI_PHISHING_UNAVAILABLE"), nil)
		return "", false
	}
	return claims.UUID, true
}

// GetAntiPhishingCode returns the anti-phishing code of the signed-in user
// (#375).
//
// @Summary Get my anti-phishing code
// @Description Whether the signed-in user has an anti-phishing code, its first characters (the whole code is in every email, never in the API, so that a stolen session cannot copy it into a phishing email), when it was set, how many emails have shown it, and whether a change is suggested: change_reason "age" once it is anti_phishing.rotation.max_age_days old, "uses" once max_uses emails have shown it.
// @Tags user-security
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=antiphishing.Status}
// @Failure 401 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse "anti_phishing_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/anti-phishing-code [get]
func (h *UserHandler) GetAntiPhishingCode(c *gin.Context) {
	userHK, ok := h.antiPhishingCaller(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	st, err := h.AntiPhishing.Status(ctx, userHK)
	if err != nil {
		sendAntiPhishingError(c, userHK, err, i18n.M("FAILED_TO_READ_SECURITY_SETTINGS"))
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("ANTI_PHISHING_CODE_RETRIEVED"), st)
}

// SetAntiPhishingCode sets the anti-phishing code of the signed-in user
// (#375).
//
// @Summary Set my anti-phishing code
// @Description Sets or changes the code that every email from the exchange to the user's verified contacts shows: an email without it is not from the exchange. It is 4 to 20 letters, digits, spaces, or hyphens (400 anti_phishing_code_invalid), and not the current code (409 anti_phishing_code_same). A new code starts with no uses. The change needs a fresh proof of the second factor (403 step_up_required). Audited, without the code.
// @Tags user-security
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body AntiPhishingCodeRequest true "The code"
// @Success 200 {object} models.APIResponse{info=antiphishing.Status}
// @Failure 400 {object} models.APIResponse "anti_phishing_code_invalid"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 409 {object} models.APIResponse "anti_phishing_code_same"
// @Failure 503 {object} models.APIResponse "anti_phishing_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/anti-phishing-code [put]
func (h *UserHandler) SetAntiPhishingCode(c *gin.Context) {
	userHK, ok := h.antiPhishingCaller(c)
	if !ok {
		return
	}
	var req AntiPhishingCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	if err := h.AntiPhishing.Set(ctx, userHK, req.Code, audit.FromRequest(c)); err != nil {
		sendAntiPhishingError(c, userHK, err, i18n.M("FAILED_TO_SAVE_ANTI_PHISHING_CODE"))
		return
	}
	st, err := h.AntiPhishing.Status(ctx, userHK)
	if err != nil {
		sendAntiPhishingError(c, userHK, err, i18n.M("FAILED_TO_READ_SECURITY_SETTINGS"))
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("ANTI_PHISHING_CODE_SET"), st)
}

// RemoveAntiPhishingCode removes the anti-phishing code of the signed-in
// user (#375).
//
// @Summary Remove my anti-phishing code
// @Description Removes the anti-phishing code: emails show none from then on. 404 anti_phishing_code_not_set without one. The removal needs a fresh proof of the second factor (403 step_up_required). Audited.
// @Tags user-security
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 404 {object} models.APIResponse "anti_phishing_code_not_set"
// @Failure 503 {object} models.APIResponse "anti_phishing_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/anti-phishing-code [delete]
func (h *UserHandler) RemoveAntiPhishingCode(c *gin.Context) {
	userHK, ok := h.antiPhishingCaller(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	if err := h.AntiPhishing.Remove(ctx, userHK, audit.FromRequest(c)); err != nil {
		sendAntiPhishingError(c, userHK, err, i18n.M("FAILED_TO_SAVE_ANTI_PHISHING_CODE"))
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("ANTI_PHISHING_CODE_REMOVED"), nil)
}

// sendAntiPhishingError answers a failed request on the anti-phishing
// code; failed is the message of an unexpected failure.
func sendAntiPhishingError(c *gin.Context, userHK string, err error, failed i18n.Message) {
	switch {
	case errors.Is(err, antiphishing.ErrInvalid):
		helpers.SendError(c, http.StatusBadRequest, CodeAntiPhishingCodeInvalid, i18n.M("ANTI_PHISHING_CODE_INVALID"), nil)
	case errors.Is(err, antiphishing.ErrSame):
		helpers.SendError(c, http.StatusConflict, CodeAntiPhishingCodeSame, i18n.M("ANTI_PHISHING_CODE_SAME"), nil)
	case errors.Is(err, antiphishing.ErrNotSet):
		helpers.SendError(c, http.StatusNotFound, CodeAntiPhishingCodeNotSet, i18n.M("ANTI_PHISHING_CODE_NOT_SET"), nil)
	case errors.Is(err, antiphishing.ErrNoKeys):
		helpers.SendError(c, http.StatusServiceUnavailable, codeAntiPhishingUnavailable, i18n.M("ANTI_PHISHING_UNAVAILABLE"), nil)
	default:
		log.Printf("Failed to handle the anti-phishing code of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, failed, nil)
	}
}
