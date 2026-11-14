package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/withdrawals"

	"github.com/gin-gonic/gin"
)

// The withdrawals that administrators review (#70): those at or above
// withdrawals.review_above of their currency wait for an approval or a
// rejection before they are paid.

const (
	codeWithdrawalsUnavailable = "withdrawals_unavailable"
	codeWithdrawalNotFound     = "withdrawal_not_found"
	codeWithdrawalNotInReview  = "withdrawal_not_in_review"
)

// withdrawalReview is the decision on a withdrawal held for review.
type withdrawalReview struct {
	// Reason, for a rejection, is what the user is told, 1 to 500
	// characters.
	Reason string `json:"reason,omitempty" example:"The destination is under investigation."`
}

func (h *AdminHandler) withdrawalsConfigured(c *gin.Context) bool {
	if h.Withdrawals == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codeWithdrawalsUnavailable, i18n.M("WITHDRAWALS_UNAVAILABLE"), nil)
		return false
	}
	return true
}

// @Summary List withdrawals for review
// @Description The withdrawals that wait for an administrator (status review), oldest first; or, with status, those of that status, newest first. At most limit (100). Each has the user's user_hk. A withdrawal that fails its seal answers 409 row_unverified, as the other administrative lists do.
// @Tags admin withdrawals
// @Produce json
// @Security BearerAuth
// @Param status query string false "review by default" Enums(review,queued,processing,partially_paid,completed,cancelled,rejected,failed)
// @Param limit query int false "At most 100" minimum(1) maximum(100)
// @Success 200 {object} models.APIResponse{info=[]withdrawals.Withdrawal}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "row_unverified"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/withdrawals [get]
func (h *AdminHandler) ListWithdrawals(c *gin.Context) {
	if !h.withdrawalsConfigured(c) {
		return
	}
	status := withdrawals.Status(c.Query("status"))
	switch status {
	case "", withdrawals.StatusReview, withdrawals.StatusQueued, withdrawals.StatusProcessing, withdrawals.StatusPartiallyPaid,
		withdrawals.StatusCompleted, withdrawals.StatusCancelled, withdrawals.StatusRejected, withdrawals.StatusFailed:
	default:
		sendValidationErrors(c, fieldErrors{"status": "is not a status of a withdrawal"})
		return
	}
	limit := withdrawals.MaxList
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > withdrawals.MaxList {
			sendValidationErrors(c, fieldErrors{"limit": "must be from 1 to 100"})
			return
		}
		limit = n
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	list, err := h.Withdrawals.Review(ctx, status, limit)
	if err != nil {
		sendWithdrawalError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("WITHDRAWALS_RETRIEVED"), list)
}

// @Summary Approve a withdrawal
// @Description Lets a withdrawal held for review go to its payouts (status queued). A sensitive route: it needs a step-up. Audited as withdrawal.approve.
// @Tags admin withdrawals
// @Produce json
// @Security BearerAuth
// @Param id path string true "The withdrawal" format(uuid)
// @Success 200 {object} models.APIResponse{info=withdrawals.Withdrawal}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 404 {object} models.APIResponse "withdrawal_not_found"
// @Failure 409 {object} models.APIResponse "withdrawal_not_in_review or row_unverified"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/withdrawals/{id}/approve [post]
func (h *AdminHandler) ApproveWithdrawal(c *gin.Context) {
	if !h.withdrawalsConfigured(c) {
		return
	}
	origin, adminHK, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	w, err := h.Withdrawals.Approve(ctx, c.Param("id"), adminHK, origin)
	if err != nil {
		sendWithdrawalError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("WITHDRAWAL_APPROVED"), w)
}

// @Summary Reject a withdrawal
// @Description Refuses a withdrawal held for review (status rejected), and releases its amount and fee to the user's available balance. The user sees reason. A sensitive route: it needs a step-up. Audited as withdrawal.reject, with the reason.
// @Tags admin withdrawals
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "The withdrawal" format(uuid)
// @Param body body withdrawalReview true "The reason"
// @Success 200 {object} models.APIResponse{info=withdrawals.Withdrawal}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 404 {object} models.APIResponse "withdrawal_not_found"
// @Failure 409 {object} models.APIResponse "withdrawal_not_in_review or row_unverified"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/withdrawals/{id}/reject [post]
func (h *AdminHandler) RejectWithdrawal(c *gin.Context) {
	if !h.withdrawalsConfigured(c) {
		return
	}
	var req withdrawalReview
	if !decodeStrict(c, &req) {
		return
	}
	origin, adminHK, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	w, err := h.Withdrawals.Reject(ctx, c.Param("id"), adminHK, req.Reason, origin)
	if err != nil {
		sendWithdrawalError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("WITHDRAWAL_REJECTED"), w)
}

func sendWithdrawalError(c *gin.Context, err error) {
	var invalid withdrawals.Invalid
	switch {
	case errors.As(err, &invalid):
		sendValidationErrors(c, fieldErrors(invalid))
	case errors.Is(err, withdrawals.ErrNotFound):
		helpers.SendError(c, http.StatusNotFound, codeWithdrawalNotFound, i18n.M("WITHDRAWAL_NOT_FOUND"), nil)
	case errors.Is(err, withdrawals.ErrNotInReview):
		helpers.SendError(c, http.StatusConflict, codeWithdrawalNotInReview, i18n.M("WITHDRAWAL_NOT_IN_REVIEW"), nil)
	case errors.Is(err, rowseal.ErrUnverified), errors.Is(err, ledger.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
	default:
		log.Printf("Failed to handle a withdrawal for an administrator: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("WITHDRAWAL_FAILED"), nil)
	}
}
