package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/otc/routed"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// RoutedOrders lists the routed orders of the OTC trades and resolves
// those left to a review (#513): *routed.Review.
type RoutedOrders interface {
	List(ctx context.Context, f routed.Filter) ([]routed.Item, error)
	Resolve(ctx context.Context, id, action, note string, origin audit.Event) (routed.Resolution, error)
}

var _ RoutedOrders = (*routed.Review)(nil)

// ResolveRoutedOrderRequest is the body of a resolution.
type ResolveRoutedOrderRequest struct {
	// Action is handled, after the order was taken care of by hand, or
	// retry, to route its remainder again.
	Action string `json:"action" binding:"required" enums:"handled,retry"`
	// Note says what was done or checked, such as the hedge placed by hand.
	Note string `json:"note" binding:"required" maxLength:"1000"`
}

const maxRoutedNote = 1000

var venueName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,29}$`)

func (h *AdminHandler) routedUnavailable(c *gin.Context) bool {
	if h.RoutedOrders == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, "routing_unavailable", i18n.M("OTC_ROUTING_UNAVAILABLE"), nil)
		return true
	}
	return false
}

// ListRoutedOrders lists the routed orders of the OTC trades.
//
// @Summary List the routed orders of the OTC trades
// @Description The orders that cover the venue legs of the OTC trades at the venues (#387), the newest first, each with its transitions, so that an administrator sees every order left to a review (status=manual_review) and why, without the logs (#513). An order changed outside the backend is listed with sealed false and the id of its integrity finding: no node drives it, and it cannot be resolved until it is re-sealed after a review. The unsealed columns, the lease and the deadlines, are shown apart. Filter by status, venue, and trade; limit is at most 200.
// @Tags admin otc
// @Produce json
// @Security BearerAuth
// @Param status query string false "The status" Enums(planned,placed,partially_filled,filled,failed,cancelled,manual_review,resolved)
// @Param venue query string false "The venue"
// @Param trade_id query string false "The trade" format(uuid)
// @Param limit query int false "At most this many orders, 50 by default, at most 200"
// @Success 200 {object} models.APIResponse{info=[]routed.Item}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/otc/routed-orders [get]
func (h *AdminHandler) ListRoutedOrders(c *gin.Context) {
	if h.routedUnavailable(c) {
		return
	}
	f := routed.Filter{Status: c.Query("status"), Venue: c.Query("venue"), TradeID: c.Query("trade_id")}
	invalid := fieldErrors{}
	if f.Status != "" && !slices.Contains(routed.Statuses, f.Status) {
		invalid["status"] = "must be a status of a routed order"
	}
	if f.Venue != "" && !venueName.MatchString(f.Venue) {
		invalid["venue"] = "must be the name of a venue"
	}
	if f.TradeID != "" {
		if _, err := uuid.Parse(f.TradeID); err != nil {
			invalid["trade_id"] = "must be a UUID"
		}
	}
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > routed.MaxListLimit {
			invalid["limit"] = "must be from 1 to 200"
		}
		f.Limit = n
	}
	if len(invalid) > 0 {
		sendValidationErrors(c, invalid)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	items, err := h.RoutedOrders.List(ctx, f)
	if err != nil {
		log.Printf("Failed to list the routed orders: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_ROUTED_ORDERS"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("ROUTED_ORDERS_RETRIEVED"), items)
}

// ResolveRoutedOrder resolves a routed order left to a review.
//
// @Summary Resolve a routed order left to a review
// @Description Ends a routed order in manual_review as resolved (#513). handled records that a person took care of it, such as a hedge placed by hand; nothing is placed. retry routes its remainder again, at any venue, as orders of new attempts, up to 10 per leg; only an order left for no_route or attempts_exhausted can be retried, as its venue order is over, and the others answer 409 not_retryable, so that an order that may still be open at the venue is checked there first. The note is required. The order's seal is checked: one changed outside the backend answers 409 row_unverified until it is re-sealed. A lockdown of otc_routing refuses a retry. Audited as admin.otc_routed_order_resolve. The route needs a step-up.
// @Tags admin otc
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "The order's id" format(uuid)
// @Param body body ResolveRoutedOrderRequest true "The resolution, and what was done"
// @Success 200 {object} models.APIResponse{info=routed.Resolution}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/otc/routed-orders/{id}/resolve [post]
func (h *AdminHandler) ResolveRoutedOrder(c *gin.Context) {
	if h.routedUnavailable(c) {
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		sendValidationErrors(c, fieldErrors{"id": "must be a UUID"})
		return
	}
	var req ResolveRoutedOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	invalid := fieldErrors{}
	if req.Action != routed.ResolveHandled && req.Action != routed.ResolveRetry {
		invalid["action"] = "must be handled or retry"
	}
	req.Note = strings.TrimSpace(req.Note)
	if req.Note == "" || utf8.RuneCountInString(req.Note) > maxRoutedNote || strings.ContainsRune(req.Note, 0) {
		invalid["note"] = "is required, at most 1000 characters"
	}
	if len(invalid) > 0 {
		sendValidationErrors(c, invalid)
		return
	}
	origin, _, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	res, err := h.RoutedOrders.Resolve(ctx, id, req.Action, req.Note, origin)
	var locked *platform.Locked
	switch {
	case errors.Is(err, routed.ErrNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("ROUTED_ORDER_NOT_FOUND"), nil)
	case errors.Is(err, routed.ErrNotInReview):
		helpers.SendError(c, http.StatusConflict, "not_in_review", i18n.M("ROUTED_ORDER_NOT_IN_REVIEW"), nil)
	case errors.Is(err, routed.ErrNotRetryable):
		helpers.SendError(c, http.StatusConflict, "not_retryable", i18n.M("ROUTED_ORDER_NOT_RETRYABLE"), nil)
	case errors.Is(err, routed.ErrNoRetryRoute):
		helpers.SendError(c, http.StatusConflict, "no_route", i18n.M("ROUTED_ORDER_NO_ROUTE"), nil)
	case errors.Is(err, routed.ErrRetriesExhausted):
		helpers.SendError(c, http.StatusConflict, "attempts_exhausted", i18n.M("ROUTED_ORDER_ATTEMPTS_EXHAUSTED"), nil)
	case errors.Is(err, routed.ErrRetryUnavailable):
		helpers.SendError(c, http.StatusServiceUnavailable, "routing_unavailable", i18n.M("OTC_ROUTING_UNAVAILABLE"), nil)
	case errors.Is(err, rowseal.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
	case errors.As(err, &locked):
		platform.Send(c, locked)
	case err != nil:
		log.Printf("Failed to resolve the routed order %s: %v", id, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_RESOLVE_ROUTED_ORDER"), nil)
	default:
		helpers.SendResponse(c, http.StatusOK, i18n.M("ROUTED_ORDER_RESOLVED"), res)
	}
}
