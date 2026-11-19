package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/withdrawals"

	"github.com/gin-gonic/gin"
)

// The payouts of withdrawals (#70). With the manual provider, an
// administrator pays each transfer, by bank or on chain, and records it
// here with its tracking number, or its failure.

const (
	codePayoutNotFound = "payout_not_found"
	codePayoutNotOpen  = "payout_not_open"
	// codePayoutAutomatic refuses to record by hand a payout that its
	// provider pays and reports, such as the TRON signer (#68).
	codePayoutAutomatic = "payout_automatic"
)

// payoutCompletion records a payout as paid.
type payoutCompletion struct {
	// TrackingID is the bank's tracking number, or the hash of the
	// transaction on chain, 1 to 200 characters.
	TrackingID string `json:"tracking_id" example:"140311150012345678"`
}

// payoutFailure records a payout as failed.
type payoutFailure struct {
	// Reason is why it failed, 1 to 200 characters.
	Reason string `json:"reason" example:"The destination bank refused the transfer."`
}

func (h *AdminHandler) payoutsConfigured(c *gin.Context) bool {
	if h.Payouts == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codeWithdrawalsUnavailable, i18n.M("WITHDRAWALS_UNAVAILABLE"), nil)
		return false
	}
	return true
}

// @Summary List payouts
// @Description The payouts of withdrawals of a status, by when they are due: submitted by default, the transfers that the manual provider waits for an administrator to pay. Each has the user, the currency, the rail, the amount, and the destination masked. A payout that fails its seal answers 409 row_unverified.
// @Tags admin withdrawals
// @Produce json
// @Security BearerAuth
// @Param status query string false "submitted by default" Enums(planned,submitted,completed,failed,released)
// @Param limit query int false "At most 100" minimum(1) maximum(100)
// @Success 200 {object} models.APIResponse{info=[]withdrawals.Payout}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "row_unverified"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/payouts [get]
func (h *AdminHandler) ListPayouts(c *gin.Context) {
	if !h.payoutsConfigured(c) {
		return
	}
	status := withdrawals.PayoutStatus(c.Query("status"))
	switch status {
	case "", withdrawals.PayoutPlanned, withdrawals.PayoutSubmitted, withdrawals.PayoutCompleted, withdrawals.PayoutFailed,
		withdrawals.PayoutReleased:
	default:
		sendValidationErrors(c, fieldErrors{"status": "is not a status of a payout"})
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
	list, err := h.Payouts.List(ctx, status, limit)
	if err != nil {
		sendPayoutError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("PAYOUTS_RETRIEVED"), list)
}

// @Summary Show a payout to pay
// @Description A payout with its destination in the clear: the card number, the IBAN, the account number, or the address and its memo, for the administrator who pays it by hand. The reading is audited as withdrawal.payout_view.
// @Tags admin withdrawals
// @Produce json
// @Security BearerAuth
// @Param id path string true "The payout" format(uuid)
// @Success 200 {object} models.APIResponse{info=withdrawals.PayoutDetail}
// @Failure 401 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "payout_not_found"
// @Failure 409 {object} models.APIResponse "row_unverified"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/payouts/{id} [get]
func (h *AdminHandler) GetPayout(c *gin.Context) {
	if !h.payoutsConfigured(c) {
		return
	}
	origin, _, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	detail, err := h.Payouts.Detail(ctx, c.Param("id"), origin)
	if err != nil {
		sendPayoutError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("PAYOUT_RETRIEVED"), detail)
}

// @Summary Record a payout as paid
// @Description Records a submitted payout as paid, with the bank's tracking number or the hash on chain. Its amount and its share of the fee leave the user's balance in the ledger, in the same transaction. The withdrawal completes when every payout is paid. A sensitive route: it needs a step-up. Audited as withdrawal.payout_complete.
// @Tags admin withdrawals
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "The payout" format(uuid)
// @Param body body payoutCompletion true "The tracking number"
// @Success 200 {object} models.APIResponse{info=withdrawals.Payout}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 404 {object} models.APIResponse "payout_not_found"
// @Failure 409 {object} models.APIResponse "payout_not_open, payout_automatic, or row_unverified"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/payouts/{id}/complete [post]
func (h *AdminHandler) CompletePayout(c *gin.Context) {
	if !h.payoutsConfigured(c) {
		return
	}
	var req payoutCompletion
	if !decodeStrict(c, &req) {
		return
	}
	origin, _, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	pay, err := h.Payouts.Complete(ctx, c.Param("id"), req.TrackingID, origin)
	if err != nil {
		sendPayoutError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("PAYOUT_COMPLETED"), pay)
}

// @Summary Record a payout as failed
// @Description Records a planned or submitted payout as failed. Its amount is planned again on the next rail that reaches the destination; when none can take it, its amount and its share of the fee are released to the user. The payouts paid stay paid. A sensitive route: it needs a step-up. Audited as withdrawal.payout_fail, with the reason.
// @Tags admin withdrawals
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "The payout" format(uuid)
// @Param body body payoutFailure true "The reason"
// @Success 200 {object} models.APIResponse{info=withdrawals.Payout}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 404 {object} models.APIResponse "payout_not_found"
// @Failure 409 {object} models.APIResponse "payout_not_open, payout_automatic, or row_unverified"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/payouts/{id}/fail [post]
func (h *AdminHandler) FailPayout(c *gin.Context) {
	if !h.payoutsConfigured(c) {
		return
	}
	var req payoutFailure
	if !decodeStrict(c, &req) {
		return
	}
	origin, _, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	pay, err := h.Payouts.Fail(ctx, c.Param("id"), req.Reason, origin)
	if err != nil {
		sendPayoutError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("PAYOUT_FAILED"), pay)
}

func sendPayoutError(c *gin.Context, err error) {
	var invalid withdrawals.Invalid
	switch {
	case errors.As(err, &invalid):
		sendValidationErrors(c, fieldErrors(invalid))
	case errors.Is(err, withdrawals.ErrPayoutNotFound):
		helpers.SendError(c, http.StatusNotFound, codePayoutNotFound, i18n.M("PAYOUT_NOT_FOUND"), nil)
	case errors.Is(err, withdrawals.ErrPayoutNotOpen):
		helpers.SendError(c, http.StatusConflict, codePayoutNotOpen, i18n.M("PAYOUT_NOT_OPEN"), nil)
	case errors.Is(err, withdrawals.ErrPayoutAutomatic):
		helpers.SendError(c, http.StatusConflict, codePayoutAutomatic, i18n.M("PAYOUT_AUTOMATIC"), nil)
	case errors.Is(err, rowseal.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
	case errors.Is(err, destinations.ErrNoKeys):
		helpers.SendError(c, http.StatusServiceUnavailable, codeWithdrawalsUnavailable, i18n.M("WITHDRAWALS_UNAVAILABLE"), nil)
	default:
		log.Printf("Failed to handle a payout for an administrator: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("PAYOUT_FAILED_TO_RECORD"), nil)
	}
}
