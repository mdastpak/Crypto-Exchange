package user

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/verification"

	"github.com/gin-gonic/gin"
)

// GetVerification returns the verification level of the signed-in user,
// what it allows, and their withdrawal limits (#157), so that a client can
// show what each level unlocks and what remains of the daily limits.
//
// @Summary Get my verification level
// @Description The verification level of the signed-in user, from 0 to 2, and the KYC tier and status it comes from. Only an approved tier counts. actions lists every action (deposit_fiat, deposit_crypto, trade_otc, trade_market, withdraw_fiat, withdraw_crypto), whether the level allows it, and the level it needs. withdrawal_limits lists the limits of the currencies that the level may withdraw: per transaction, and in any 24 hours in all the user's accounts, the fee included, with what the user has requested and what remains. An action that the level does not allow answers 403 verification_required; a withdrawal above a limit answers 403 withdrawal_limit_exceeded.
// @Tags user
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=verification.Summary}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/verification [get]
func (h *UserHandler) GetVerification(c *gin.Context) {
	userID, ok := callerID(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	if h.Verification == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("FAILED_TO_READ_VERIFICATION"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	summary, err := h.Verification.Summary(ctx, userID.String())
	if errors.Is(err, verification.ErrUserNotFound) {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	if err != nil {
		log.Printf("Failed to read the verification of a user: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_VERIFICATION"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("VERIFICATION_RETRIEVED_SUCCESSFULLY"), summary)
}
