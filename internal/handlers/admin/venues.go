package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/venues"
	"github.com/mdastpak/crypto-exchange/internal/venues/state"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// VenueStates reads the state of the external venues and clears the holds
// of their reconciliation (#385): *state.Store.
type VenueStates interface {
	Overview(ctx context.Context, mappings map[string]venues.Mapping) ([]state.VenueState, error)
	ClearHold(ctx context.Context, id string, origin audit.Event, reason string) (state.Hold, error)
}

var _ VenueStates = (*state.Store)(nil)

// ClearVenueHoldRequest is the body of a clear of a hold.
type ClearVenueHoldRequest struct {
	// Reason says why the difference is understood, such as the deposit
	// that the venue credited late.
	Reason string `json:"reason" binding:"required" maxLength:"1000"`
}

// maxVenueHoldReason bounds the reason of a clear, as the schema does.
const maxVenueHoldReason = 1000

// ListVenues shows the state of the enabled venues.
//
// @Summary Show the state of the venues
// @Description The external exchanges that cover the OTC trades (#385), as the snapshots of cmd/market-worker last saw them: the time and freshness of each snapshot and its last failure, each balance against what the books say the venue holds with its open reservations and whether it is routable, the networks, the count of the markets and of those trading, and the open reconciliation holds. Read from the database, so every node answers the same.
// @Tags admin venues
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=[]state.VenueState}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/venues [get]
func (h *AdminHandler) ListVenues(c *gin.Context) {
	if h.Venues == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, "venues_unavailable", i18n.M("VENUES_UNAVAILABLE"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	states, err := h.Venues.Overview(ctx, state.MappingsOf(h.settings().Venues))
	if errors.Is(err, rowseal.ErrUnverified) {
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
		return
	}
	if err != nil {
		log.Printf("Failed to read the state of the venues: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_VENUES"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("VENUES_RETRIEVED_SUCCESSFULLY"), states)
}

// ClearVenueHold clears a reconciliation hold after a review.
//
// @Summary Clear a reconciliation hold of a venue
// @Description After an operator reviewed the difference between what a venue reported holding of a currency and what the books say (#385), clears the hold that made the currency not routable there. The reason is required. If the difference stays, the next reconciliations hold the currency again. Audited as admin.venue_hold_clear. A hold already cleared answers 409 already_cleared; one changed outside the backend, 409 row_unverified. The route needs a step-up.
// @Tags admin venues
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "The hold's id" format(uuid)
// @Param body body ClearVenueHoldRequest true "Why the difference is understood"
// @Success 200 {object} models.APIResponse{info=state.Hold}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/venues/holds/{id}/clear [post]
func (h *AdminHandler) ClearVenueHold(c *gin.Context) {
	if h.Venues == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, "venues_unavailable", i18n.M("VENUES_UNAVAILABLE"), nil)
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		sendValidationErrors(c, fieldErrors{"id": "must be a UUID"})
		return
	}
	var req ClearVenueHoldRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" || utf8.RuneCountInString(req.Reason) > maxVenueHoldReason || strings.ContainsRune(req.Reason, 0) {
		sendValidationErrors(c, fieldErrors{"reason": "is required, at most 1000 characters"})
		return
	}
	origin, _, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	hold, err := h.Venues.ClearHold(ctx, id, origin, req.Reason)
	switch {
	case errors.Is(err, state.ErrNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("VENUE_HOLD_NOT_FOUND"), nil)
	case errors.Is(err, state.ErrAlreadyCleared):
		helpers.SendError(c, http.StatusConflict, "already_cleared", i18n.M("VENUE_HOLD_ALREADY_CLEARED"), nil)
	case errors.Is(err, rowseal.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
	case err != nil:
		log.Printf("Failed to clear the venue hold %s: %v", id, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CLEAR_VENUE_HOLD"), nil)
	default:
		helpers.SendResponse(c, http.StatusOK, i18n.M("VENUE_HOLD_CLEARED"), hold)
	}
}
