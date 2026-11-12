package user

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Profile is the response of GET and PATCH /user/profile. The contacts are
// masked: the user proved control of them at sign-in, and a leaked response
// should not reveal them.
type Profile struct {
	UUID        string            `json:"uuid"`
	Status      string            `json:"status"`
	Contact     map[string]string `json:"contact"`
	FirstName   *string           `json:"first_name"`
	LastName    *string           `json:"last_name"`
	DisplayName *string           `json:"display_name"`
	// Locale is the language the user chose (#286), a tag of GET
	// /public/locales, or null for none: each request then negotiates it.
	// It applies to every signed-in request that names no locale itself.
	Locale *string `json:"locale" example:"fa"`
	// Theme is the colour theme for the frontends: light, dark, or system,
	// which follows the device.
	Theme     string     `json:"theme" enums:"light,dark,system" example:"system"`
	KYCStatus string     `json:"kyc_status"`
	KYCTier   string     `json:"kyc_tier"`
	CreatedAt *time.Time `json:"created_at"`
}

var (
	errProfileNotFound    = errors.New("profile not found")
	errAccountUnavailable = errors.New("account is not available")
)

// maskContact hides most of a contact, like the sign-in logs do: the first
// three characters stay visible, with the domain of an email address or the
// last three digits of a mobile number.
func maskContact(contactType, value string) string {
	const hidden = "*****"
	if contactType == "email" {
		at := strings.LastIndex(value, "@")
		if at <= 0 {
			return hidden
		}
		return value[:min(3, at)] + hidden + value[at:]
	}
	if len(value) <= 6 {
		return hidden
	}
	return value[:3] + hidden + value[len(value)-3:]
}

// callerID returns the user that the access token names. There is no user ID
// in the profile routes, so a user can only read and change their own
// profile.
func callerID(c *gin.Context) (uuid.UUID, bool) {
	claims, ok := auth.GetClaims(c)
	if !ok {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(claims.UUID)
	return id, err == nil
}

// sendProfileError answers a failed profile read.
func sendProfileError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errProfileNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("PROFILE_NOT_FOUND"), nil)
	case errors.Is(err, errAccountUnavailable):
		helpers.SendError(c, http.StatusForbidden, helpers.CodeAccountUnavailable, i18n.M("ACCOUNT_NOT_AVAILABLE_CONTACT_SUPPORT"), nil)
	default:
		log.Printf("Failed to load a profile: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LOAD_PROFILE"), nil)
	}
}

// GetProfile returns the profile of the signed-in user.
//
// @Summary Get the signed-in user's profile
// @Tags user
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=Profile}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/profile [get]
func (h *UserHandler) GetProfile(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	userID, ok := callerID(c)
	if !ok {
		helpers.SendError(c, http.StatusUnauthorized, helpers.CodeTokenInvalid, i18n.M("UNAUTHORIZED_INVALID_TOKEN"), nil)
		return
	}
	profile, err := h.Store.Profile(ctx, userID)
	if err != nil {
		sendProfileError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("PROFILE_RETRIEVED_SUCCESSFULLY"), profile)
}

// UpdateProfile changes the names and the preferences in the profile of the
// signed-in user and returns the updated profile. Only the fields in the
// request change.
//
// @Summary Change the names and the preferences in the profile
// @Description A saved locale applies at once to every later signed-in request that names no locale of its own, on every device and domain, and to the answer of this request.
// @Tags user
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body profileUpdate true "The fields to change"
// @Success 200 {object} models.APIResponse{info=Profile}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/profile [patch]
func (h *UserHandler) UpdateProfile(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	userID, ok := callerID(c)
	if !ok {
		helpers.SendError(c, http.StatusUnauthorized, helpers.CodeTokenInvalid, i18n.M("UNAUTHORIZED_INVALID_TOKEN"), nil)
		return
	}
	update, errs, err := decodeProfileUpdate(c)
	if err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_REQUEST_BODY"), nil)
		return
	}
	if errs == nil {
		errs = update.validate(h.Locales)
	}
	if errs != nil {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": errs})
		return
	}

	// The store locks the row from the read to the write, and commits the
	// change only with its audit event (#195).
	profile, err := h.Store.UpdateProfile(ctx, userID, func(p *Profile) { update.apply(p, h.Locales) }, audit.FromRequest(c))
	if errors.Is(err, errProfileNotFound) || errors.Is(err, errAccountUnavailable) {
		sendProfileError(c, err)
		return
	}
	if err != nil {
		log.Printf("Error updating a profile: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_UPDATE_PROFILE"), nil)
		return
	}
	if profile.Locale != nil {
		i18n.Prefer(c, *profile.Locale)
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("PROFILE_UPDATED_SUCCESSFULLY"), profile)
}
