package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/rebrand"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// The migrations of currencies into others (#215).

// MigrationList is the migrations of a currency, newest first.
type MigrationList struct {
	Items []rebrand.Migration `json:"items"`
}

// CancelMigrationRequest cancels a migration.
type CancelMigrationRequest struct {
	// Version is the version of the migration that the cancel is based on.
	Version *int64 `json:"version,omitempty" example:"1"`
}

// afterCurrencyChanges follows committed changes of currencies.
func (h *AdminHandler) afterCurrencyChanges(ctx context.Context, changes []currencies.Change) {
	if i := slices.IndexFunc(changes, func(c currencies.Change) bool { return c.Public }); i >= 0 {
		h.afterPublicChange(ctx, changes[i].CurrencyHK)
	}
}

// @Summary Schedule a migration into a currency
// @Description Schedules a rebrand, a redenomination, or a merger that moves the holders of one or more approved source currencies to this approved target, each source at its ratio: the units of the target that one unit of the source becomes, such as 0.01 for 100 LEND = 1 AAVE. The phases follow the order announced, deposits_paused, trading_halted, converting, completed; announced and completed are required. A forced migration pauses the deposits and withdrawals of its sources at deposits_paused, halts their trading and conversions at trading_halted, and retires them from the lists at completed; an optional one only offers the conversion, and its sources stay. Each phase applies at its time, exactly once, on any instance. From announced on, the sources show the migration. Conversions round down to the precision of the target and record the dust; they are carried out by the ledger. Audited as currency.migration_schedule, and each phase as currency.migration_phase.
// @Tags admin currencies
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The target's UUID" format(uuid)
// @Param body body rebrand.Plan true "The migration"
// @Success 201 {object} models.APIResponse{info=rebrand.Migration}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "not_listed, source_in_migration, or target_in_migration"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies/{hk}/migrations [post]
func (h *AdminHandler) ScheduleMigration(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	hk, ok := currencyParam(c)
	if !ok {
		return
	}
	var plan rebrand.Plan
	if !decodeStrict(c, &plan) {
		return
	}
	plan.Note, plan.AnnouncementURL = strings.TrimSpace(plan.Note), strings.TrimSpace(plan.AnnouncementURL)
	if errs := plan.Validate(hk, time.Now()); len(errs) > 0 {
		sendValidationErrors(c, errs)
		return
	}
	origin, _, ok := restrictionOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	m, err := h.Rebrands.Schedule(ctx, hk, plan, origin)
	switch {
	case errors.Is(err, rebrand.ErrTargetNotListed):
		helpers.SendError(c, http.StatusConflict, "not_listed", i18n.M("MIGRATION_TARGET_NOT_APPROVED"), nil)
	case errors.Is(err, rebrand.ErrSourceNotListed):
		helpers.SendError(c, http.StatusConflict, "not_listed", i18n.M("MIGRATION_SOURCE_NOT_APPROVED"), nil)
	case errors.Is(err, rebrand.ErrSourceInMigration):
		helpers.SendError(c, http.StatusConflict, "source_in_migration", i18n.M("MIGRATION_SOURCE_MIGRATING"), nil)
	case errors.Is(err, rebrand.ErrTargetInMigration):
		helpers.SendError(c, http.StatusConflict, "target_in_migration", i18n.M("MIGRATION_TARGET_MIGRATING"), nil)
	case err != nil:
		log.Printf("Failed to schedule a migration into %s: %v", hk, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_SCHEDULE_MIGRATION"), nil)
	default:
		helpers.SendResponse(c, http.StatusCreated, i18n.M("MIGRATION_SCHEDULED"), m)
	}
}

// @Summary List the migrations of a currency
// @Description The migrations that the currency is the target or a source of, newest first, at most 100, with their sources, phases, and when each applied.
// @Tags admin currencies
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The currency's UUID" format(uuid)
// @Success 200 {object} models.APIResponse{info=MigrationList}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies/{hk}/migrations [get]
func (h *AdminHandler) ListMigrations(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	hk, ok := currencyParam(c)
	if !ok {
		return
	}
	list, err := h.Rebrands.List(ctx, hk)
	if err != nil {
		log.Printf("Failed to list the migrations of %s: %v", hk, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LIST_MIGRATIONS"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("MIGRATIONS_RETRIEVED_SUCCESSFULLY"), MigrationList{Items: list})
}

// @Summary Cancel a migration
// @Description Cancels a migration into this currency before it converts: the phases that are left do not apply, and its sources get back what its phases paused. From converting on, a cancel answers 409 migration_irreversible. With version, a cancel based on another version answers 409 version_conflict. Audited as currency.migration_cancel.
// @Tags admin currencies
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The target's UUID" format(uuid)
// @Param id path string true "The migration's id" format(uuid)
// @Param body body CancelMigrationRequest false "The version"
// @Success 200 {object} models.APIResponse{info=rebrand.Migration}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies/{hk}/migrations/{id}/cancel [post]
func (h *AdminHandler) CancelMigration(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	hk, ok := currencyParam(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_MIGRATION_ID"), nil)
		return
	}
	var req CancelMigrationRequest
	if c.Request.ContentLength != 0 && !decodeStrict(c, &req) {
		return
	}
	if req.Version != nil && *req.Version < 1 {
		sendValidationErrors(c, fieldErrors{"version": "must be at least 1"})
		return
	}
	origin, _, ok := restrictionOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	m, changes, err := h.Rebrands.Cancel(ctx, hk, id.String(), req.Version, origin)
	var conflict *rebrand.VersionConflict
	switch {
	case errors.Is(err, rebrand.ErrNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("MIGRATION_NOT_FOUND"), nil)
	case errors.As(err, &conflict):
		helpers.SendError(c, http.StatusConflict, "version_conflict", i18n.M("MIGRATION_VERSION_CONFLICT"),
			gin.H{"version": conflict.Current})
	case errors.Is(err, rebrand.ErrNotOpen):
		helpers.SendError(c, http.StatusConflict, "migration_not_open", i18n.M("MIGRATION_ENDED"), nil)
	case errors.Is(err, rebrand.ErrIrreversible):
		helpers.SendError(c, http.StatusConflict, "migration_irreversible", i18n.M("MIGRATION_PAST_CANCEL"), nil)
	case err != nil:
		log.Printf("Failed to cancel the migration %s: %v", id, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CANCEL_MIGRATION"), nil)
	default:
		h.afterCurrencyChanges(ctx, changes)
		helpers.SendResponse(c, http.StatusOK, i18n.M("MIGRATION_CANCELLED"), m)
	}
}
