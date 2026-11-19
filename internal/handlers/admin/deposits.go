package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mdastpak/crypto-exchange/internal/deposits"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// The deposits on chain (#68).

const (
	codeDepositsUnavailable = "deposits_unavailable"
	codeDepositNotFound     = "deposit_not_found"
	codeDepositNotHeld      = "deposit_not_held"
)

func (h *AdminHandler) depositsConfigured(c *gin.Context) bool {
	if h.Deposits == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codeDepositsUnavailable, i18n.M("DEPOSITS_UNAVAILABLE"), nil)
		return false
	}
	return true
}

// @Summary List deposits
// @Description The newest deposits on chain (#68), at most 200, with the user's user_id and, for a held one, its hold_reason: below_minimum (FAILED), or deposits_closed, restricted, or aml (CONFIRMED, still pending in the user's balance). held=true lists only those on hold. A deposit that fails its seal answers 409 row_unverified.
// @Tags admin deposits
// @Produce json
// @Security BearerAuth
// @Param user_id query string false "The user" format(uuid)
// @Param status query string false "The status" Enums(DETECTED,CONFIRMED,CREDITED,FAILED)
// @Param held query bool false "Only the deposits on hold"
// @Success 200 {object} models.APIResponse{info=[]deposits.Deposit}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "row_unverified"
// @Failure 503 {object} models.APIResponse "deposits_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/deposits [get]
func (h *AdminHandler) ListDeposits(c *gin.Context) {
	if !h.depositsConfigured(c) {
		return
	}
	f := deposits.Filter{UserHK: c.Query("user_id"), Status: deposits.Status(c.Query("status"))}
	if f.UserHK != "" {
		if _, err := uuid.Parse(f.UserHK); err != nil {
			sendValidationErrors(c, fieldErrors{"user_id": "is not a user id"})
			return
		}
	}
	switch f.Status {
	case "", deposits.StatusDetected, deposits.StatusConfirmed, deposits.StatusCredited, deposits.StatusFailed:
	default:
		sendValidationErrors(c, fieldErrors{"status": "is not a status of a deposit"})
		return
	}
	switch c.Query("held") {
	case "", "false":
	case "true":
		f.Held = true
	default:
		sendValidationErrors(c, fieldErrors{"held": "must be true or false"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	list, err := h.Deposits.List(ctx, f)
	if err != nil {
		sendDepositError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("DEPOSITS_RETRIEVED"), list)
}

// @Summary Credit a held deposit
// @Description Credits a deposit on hold (#68), after review: one held at confirmation, whose amount moves from pending to available, or one below the minimum, which is first recorded as pending. A deposit dropped by a reorg cannot be credited. A sensitive route: it needs a step-up. Audited as admin.deposit_credit, with the hold.
// @Tags admin deposits
// @Produce json
// @Security BearerAuth
// @Param id path string true "The deposit" format(uuid)
// @Success 200 {object} models.APIResponse{info=deposits.Deposit}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 404 {object} models.APIResponse "deposit_not_found"
// @Failure 409 {object} models.APIResponse "deposit_not_held or row_unverified"
// @Failure 503 {object} models.APIResponse "deposits_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/deposits/{id}/credit [post]
func (h *AdminHandler) CreditDeposit(c *gin.Context) {
	if !h.depositsConfigured(c) {
		return
	}
	if _, err := uuid.Parse(c.Param("id")); err != nil {
		helpers.SendError(c, http.StatusNotFound, codeDepositNotFound, i18n.M("DEPOSIT_NOT_FOUND"), nil)
		return
	}
	origin, _, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	d, err := h.Deposits.Credit(ctx, c.Param("id"), origin)
	if err != nil {
		sendDepositError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("DEPOSIT_CREDITED"), d)
}

func sendDepositError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, deposits.ErrDepositNotFound):
		helpers.SendError(c, http.StatusNotFound, codeDepositNotFound, i18n.M("DEPOSIT_NOT_FOUND"), nil)
	case errors.Is(err, deposits.ErrNotHeld):
		helpers.SendError(c, http.StatusConflict, codeDepositNotHeld, i18n.M("DEPOSIT_NOT_HELD"), nil)
	case errors.Is(err, rowseal.ErrUnverified), errors.Is(err, ledger.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
	default:
		log.Printf("Failed to serve the deposits: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("INTERNAL_ERROR"), nil)
	}
}
