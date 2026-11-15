package user

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rebrand"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"

	"github.com/gin-gonic/gin"
)

// conversionRequest converts a holding of a source of a migration.
type conversionRequest struct {
	// SourceHK is the source currency to convert, whose whole available
	// balance converts.
	SourceHK string `json:"source_hk" format:"uuid"`
	// SubAccountID converts the holding of a sub-account of the user; empty
	// is the main account.
	SubAccountID *string `json:"sub_account_id,omitempty" format:"uuid"`
}

// ConvertMigrationHolding converts a holding of the source of an optional
// migration (#215).
//
// @Summary Convert a holding of a migrating currency
// @Description While an optional migration (a rebrand, redenomination, or merger that offers its conversion) is in its converting phase, converts the whole available balance of one of its sources into the target, at the source's ratio, rounded down to the precision of the target; the rest under it is dust, which the exchange keeps. Locked funds and pending deposits stay. The source must allow convert_from and the target convert_into, and no restriction of the user may block the conversion of either. A forced migration converts every holder by itself and answers migration_not_converting here.
// @Tags user-currencies
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "The migration's id" format(uuid)
// @Param body body conversionRequest true "The source"
// @Success 201 {object} models.APIResponse{info=rebrand.Converted}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "account_frozen, operation_restricted, asset_restricted, or currency_operation_disabled"
// @Failure 404 {object} models.APIResponse "migration_not_found or not_a_source"
// @Failure 409 {object} models.APIResponse "migration_not_converting, nothing_to_convert, or insufficient_balance"
// @Failure 503 {object} models.APIResponse "conversions_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/currency-migrations/{id}/convert [post]
func (h *UserHandler) ConvertMigrationHolding(c *gin.Context) {
	claims, ok := sessionCaller(c)
	if !ok {
		return
	}
	if h.Conversions == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, "conversions_unavailable", i18n.M("CONVERSIONS_UNAVAILABLE"), nil)
		return
	}
	var req conversionRequest
	if !decodeStrict(c, &req) {
		return
	}
	invalid := map[string]string{}
	migration, err := shared.UUIDFromString(c.Param("id"))
	if err != nil {
		invalid["id"] = "a UUID"
	}
	source, err := shared.UUIDFromString(req.SourceHK)
	if err != nil {
		invalid["source_hk"] = "a UUID"
	}
	a := ledger.Account{}
	if a.UserHK, err = shared.UUIDFromString(claims.UUID); err != nil {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	if req.SubAccountID != nil {
		sub, err := shared.UUIDFromString(*req.SubAccountID)
		if err != nil {
			invalid["sub_account_id"] = "a UUID"
		}
		a.SubAccountID = &sub
	}
	if len(invalid) > 0 {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": invalid})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	out, err := h.Conversions.ConvertOwn(ctx, migration.String(), source.String(), a)
	switch {
	case err == nil:
		helpers.SendResponse(c, http.StatusCreated, i18n.M("MIGRATION_HOLDING_CONVERTED"), out)
	case errors.Is(err, rebrand.ErrNotFound):
		helpers.SendError(c, http.StatusNotFound, "migration_not_found", i18n.M("MIGRATION_NOT_FOUND"), nil)
	case errors.Is(err, rebrand.ErrNotASource):
		helpers.SendError(c, http.StatusNotFound, "not_a_source", i18n.M("MIGRATION_NOT_A_SOURCE"), nil)
	case errors.Is(err, rebrand.ErrNotConverting):
		helpers.SendError(c, http.StatusConflict, "migration_not_converting", i18n.M("MIGRATION_NOT_CONVERTING"), nil)
	case errors.Is(err, rebrand.ErrNothingToConvert):
		helpers.SendError(c, http.StatusConflict, "nothing_to_convert", i18n.M("MIGRATION_NOTHING_TO_CONVERT"), nil)
	case errors.Is(err, ledger.ErrInsufficientBalance), errors.Is(err, ledger.ErrInsufficientLocked):
		helpers.SendError(c, http.StatusConflict, "insufficient_balance", i18n.M("INSUFFICIENT_BALANCE"), nil)
	case restrictions.Send(c, err), currencies.SendRefusal(c, err):
	default:
		log.Printf("Failed to convert a holding of user %s in migration %s: %v", claims.UUID, migration, err)
		helpers.SendError(c, http.StatusInternalServerError, "conversion_failed", i18n.M("MIGRATION_CONVERSION_FAILED"), nil)
	}
}
