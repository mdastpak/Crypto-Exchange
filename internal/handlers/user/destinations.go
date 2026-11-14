package user

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/verification"

	"github.com/gin-gonic/gin"
)

// The error codes of the withdrawal destinations (#160).
const (
	codeDestinationInvalid      = "destination_invalid"
	codeDestinationExists       = "destination_exists"
	codeDestinationTaken        = "destination_taken"
	codeDestinationLimitReached = "destination_limit_reached"
	codeDestinationNotOwned     = "destination_not_owned"
	codeNationalIDRequired      = "national_id_required"
	codeOwnershipUnavailable    = "ownership_check_unavailable"
	codeDestinationsUnavailable = "destinations_unavailable"
)

// destinationTimeout bounds a request of the destinations, which may wait
// for the provider that checks the owner of a card, an IBAN, or an
// account.
const destinationTimeout = 30 * time.Second

// DestinationList is the withdrawal destinations of the signed-in user.
type DestinationList struct {
	Items []destinations.Destination `json:"items"`
}

// destinationsCaller returns the signed-in user, or answers and reports
// false.
func (h *UserHandler) destinationsCaller(c *gin.Context) (string, bool) {
	claims, ok := sessionCaller(c)
	if !ok {
		return "", false
	}
	if h.Destinations == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codeDestinationsUnavailable, i18n.M("DESTINATIONS_UNAVAILABLE"), nil)
		return "", false
	}
	return claims.UUID, true
}

// ListDestinations returns the withdrawal destinations of the signed-in
// user (#160).
//
// @Summary List my withdrawal destinations
// @Description The cards, IBANs, bank accounts, and crypto addresses that the signed-in user may withdraw to, the newest first, with their numbers masked, and when each becomes usable. A withdrawal names one of them by its id; it never takes a number or an address of its own.
// @Tags user-withdrawals
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=DestinationList}
// @Failure 401 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse "destinations_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/withdrawal-destinations [get]
func (h *UserHandler) ListDestinations(c *gin.Context) {
	userHK, ok := h.destinationsCaller(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), destinationTimeout)
	defer cancel()
	list, err := h.Destinations.List(ctx, userHK)
	if err != nil {
		log.Printf("Failed to list the withdrawal destinations of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LIST_DESTINATIONS"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("DESTINATIONS_RETRIEVED"), DestinationList{Items: list})
}

// AddDestination adds a withdrawal destination to the signed-in user's
// book (#160).
//
// @Summary Add a withdrawal destination
// @Description Adds a card, an IBAN (Sheba), a bank account, or a crypto address. Numbers may have spaces and dashes, and Persian or Arabic digits. A card, an IBAN, or an account must belong to the user's verified national ID, which the provider checks now: 409 national_id_required without one, 403 destination_not_owned when it is someone else's, 503 ownership_check_unavailable when the provider cannot answer. A crypto address must be valid for its network (bitcoin, ethereum, tron). The verification level must allow the withdrawals it is for (403 verification_required), and a recent change of a security setting holds new destinations (403 security_hold). It needs a fresh proof of the second factor (403 step_up_required). A new crypto address is usable after a waiting period, 24 hours by default; usable_from says when.
// @Tags user-withdrawals
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body destinations.Request true "The destination"
// @Success 201 {object} models.APIResponse{info=destinations.Destination}
// @Failure 400 {object} models.APIResponse "destination_invalid, with info.reason"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "destination_not_owned, verification_required, security_hold, or step_up_required"
// @Failure 409 {object} models.APIResponse "destination_exists, destination_taken, destination_limit_reached, or national_id_required"
// @Failure 503 {object} models.APIResponse "ownership_check_unavailable or destinations_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/withdrawal-destinations [post]
func (h *UserHandler) AddDestination(c *gin.Context) {
	userHK, ok := h.destinationsCaller(c)
	if !ok {
		return
	}
	var req destinations.Request
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), destinationTimeout)
	defer cancel()
	d, err := h.Destinations.Add(ctx, userHK, req, audit.FromRequest(c))
	if err != nil {
		sendDestinationError(c, userHK, err)
		return
	}
	helpers.SendResponse(c, http.StatusCreated, i18n.M("DESTINATION_ADDED"), d)
}

// RemoveDestination removes a withdrawal destination from the signed-in
// user's book (#160).
//
// @Summary Remove a withdrawal destination
// @Description Removes the destination: no withdrawal can use it afterwards. It needs a fresh proof of the second factor (403 step_up_required).
// @Tags user-withdrawals
// @Produce json
// @Security BearerAuth
// @Param id path string true "The id of the destination"
// @Success 200 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 404 {object} models.APIResponse "not_found"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/withdrawal-destinations/{id} [delete]
func (h *UserHandler) RemoveDestination(c *gin.Context) {
	userHK, ok := h.destinationsCaller(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), destinationTimeout)
	defer cancel()
	if err := h.Destinations.Remove(ctx, userHK, c.Param("id"), audit.FromRequest(c)); err != nil {
		sendDestinationError(c, userHK, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("DESTINATION_REMOVED"), nil)
}

// sendDestinationError answers a refusal of the destinations.
func sendDestinationError(c *gin.Context, userHK string, err error) {
	if verification.Send(c, err) || restrictions.Send(c, err) {
		return
	}
	switch {
	case errors.Is(err, destinations.ErrInvalid):
		reason := strings.TrimPrefix(err.Error(), destinations.ErrInvalid.Error()+": ")
		helpers.SendError(c, http.StatusBadRequest, codeDestinationInvalid, i18n.M("DESTINATION_INVALID"), gin.H{"reason": reason})
	case errors.Is(err, destinations.ErrNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("DESTINATION_NOT_FOUND"), nil)
	case errors.Is(err, destinations.ErrExists):
		helpers.SendError(c, http.StatusConflict, codeDestinationExists, i18n.M("DESTINATION_EXISTS"), nil)
	case errors.Is(err, destinations.ErrTaken):
		helpers.SendError(c, http.StatusConflict, codeDestinationTaken, i18n.M("DESTINATION_TAKEN"), nil)
	case errors.Is(err, destinations.ErrTooMany):
		helpers.SendError(c, http.StatusConflict, codeDestinationLimitReached, i18n.M("DESTINATION_LIMIT_REACHED"), nil)
	case errors.Is(err, destinations.ErrNationalIDRequired):
		helpers.SendError(c, http.StatusConflict, codeNationalIDRequired, i18n.M("NATIONAL_ID_REQUIRED"), nil)
	case errors.Is(err, destinations.ErrNotOwned):
		helpers.SendError(c, http.StatusForbidden, codeDestinationNotOwned, i18n.M("DESTINATION_NOT_OWNED"), nil)
	case errors.Is(err, destinations.ErrOwnershipUnavailable):
		log.Printf("The ownership of a withdrawal destination of user %s could not be checked: %v", userHK, err)
		helpers.SendError(c, http.StatusServiceUnavailable, codeOwnershipUnavailable, i18n.M("OWNERSHIP_CHECK_UNAVAILABLE"), nil)
	case errors.Is(err, destinations.ErrNoKeys):
		helpers.SendError(c, http.StatusServiceUnavailable, codeDestinationsUnavailable, i18n.M("DESTINATIONS_UNAVAILABLE"), nil)
	default:
		log.Printf("Failed to change the withdrawal destinations of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CHANGE_DESTINATIONS"), nil)
	}
}
