package user

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/idcheck"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/gin-gonic/gin"
)

// The codes of the identity routes (#158).
const (
	codeIdentityVerified        = "identity_already_verified"
	codeIdentityPending         = "identity_check_pending"
	codeIdentityTooManyAttempts = "identity_attempts_exceeded"
	codeNationalIDTaken         = "national_id_taken"
)

// IdentityRequest is the national ID and the date of birth of the user.
type IdentityRequest struct {
	// NationalID is the ten digits of the Iranian national ID.
	NationalID string `json:"national_id" binding:"required" example:"0012345679"`
	// BirthDate is the date of birth in the Gregorian calendar, YYYY-MM-DD.
	BirthDate string `json:"birth_date" binding:"required" example:"1991-08-16" format:"date"`
}

// identityCaller returns the signed-in user, or answers and reports false.
func (h *UserHandler) identityCaller(c *gin.Context) (string, bool) {
	claims, ok := sessionCaller(c)
	if !ok {
		return "", false
	}
	if h.Identity == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codeIdentityUnavailable, i18n.M("IDENTITY_UNAVAILABLE"), nil)
		return "", false
	}
	return claims.UUID, true
}

// GetIdentity returns the state of the user's identity check (#158).
//
// @Summary My identity check
// @Description The state of the signed-in user's identity: none, pending while the provider cannot answer and the server asks again, verified, or rejected with its reason (mismatched or taken). national_id is masked. attempts_left is how many more checks may fail today. A verified national ID and a card or IBAN of the user's withdrawal destinations whose owner the provider matched raise the user to verification level 1.
// @Tags user-identity
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=idcheck.Status}
// @Failure 401 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "row_unverified"
// @Failure 503 {object} models.APIResponse "identity_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/identity [get]
func (h *UserHandler) GetIdentity(c *gin.Context) {
	userHK, ok := h.identityCaller(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	st, err := h.Identity.Status(ctx, userHK)
	if err != nil {
		sendIdentityError(c, userHK, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("IDENTITY_RETRIEVED"), st)
}

// SubmitIdentity checks the user's national ID and date of birth (#158).
//
// @Summary Verify my identity
// @Description Checks the national ID and the date of birth of the signed-in user at the national registry, through the identity provider, and, when the account has a verified mobile number, that the number is registered to the national ID. 200 with the identity: verified, or rejected with the reason mismatched, which is final for that check; 202 while the provider cannot answer: the check is pending, the server asks again, and nothing is approved on a failure. One person has one account: a national ID verified for another account answers 409 national_id_taken. A verified identity cannot be changed here (409 identity_already_verified); one check is pending at a time (409 identity_check_pending); five failed checks in a day stop the checks until the next (429 identity_attempts_exceeded). Audited as user.identity_check, never with the values.
// @Tags user-identity
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body IdentityRequest true "The national ID and the date of birth"
// @Success 200 {object} models.APIResponse{info=idcheck.Status}
// @Success 202 {object} models.APIResponse{info=idcheck.Status}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "national_id_taken, identity_already_verified, identity_check_pending, or row_unverified"
// @Failure 429 {object} models.APIResponse "identity_attempts_exceeded"
// @Failure 503 {object} models.APIResponse "identity_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/identity [post]
func (h *UserHandler) SubmitIdentity(c *gin.Context) {
	userHK, ok := h.identityCaller(c)
	if !ok {
		return
	}
	var req IdentityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendValidation(c, map[string]string{"body": "must have national_id and birth_date"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	st, err := h.Identity.Submit(ctx, userHK, strings.TrimSpace(req.NationalID), strings.TrimSpace(req.BirthDate), audit.FromRequest(c))
	if err != nil {
		sendIdentityError(c, userHK, err)
		return
	}
	status := http.StatusOK
	if st.State == idcheck.StatePending {
		status = http.StatusAccepted
	}
	helpers.SendResponse(c, status, i18n.M("IDENTITY_RETRIEVED"), st)
}

// sendIdentityError answers an error of the identity checks. The errors
// never hold a national ID or a date.
func sendIdentityError(c *gin.Context, userHK string, err error) {
	var invalid idcheck.Invalid
	switch {
	case errors.As(err, &invalid):
		sendValidation(c, invalid)
	case errors.Is(err, idcheck.ErrNationalIDTaken):
		helpers.SendError(c, http.StatusConflict, codeNationalIDTaken, i18n.M("NATIONAL_ID_TAKEN"), nil)
	case errors.Is(err, idcheck.ErrAlreadyVerified):
		helpers.SendError(c, http.StatusConflict, codeIdentityVerified, i18n.M("IDENTITY_ALREADY_VERIFIED"), nil)
	case errors.Is(err, idcheck.ErrPending):
		helpers.SendError(c, http.StatusConflict, codeIdentityPending, i18n.M("IDENTITY_CHECK_PENDING"), nil)
	case errors.Is(err, idcheck.ErrTooManyAttempts):
		helpers.SendError(c, http.StatusTooManyRequests, codeIdentityTooManyAttempts, i18n.M("IDENTITY_ATTEMPTS_EXCEEDED"), nil)
	case errors.Is(err, idcheck.ErrUnavailable), errors.Is(err, idcheck.ErrNoKeys):
		helpers.SendError(c, http.StatusServiceUnavailable, codeIdentityUnavailable, i18n.M("IDENTITY_UNAVAILABLE"), nil)
	case errors.Is(err, rowseal.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, "row_unverified", i18n.M("IDENTITY_UNVERIFIED"), nil)
	default:
		log.Printf("Failed to check the identity of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("INTERNAL_SERVER_ERROR"), nil)
	}
}
