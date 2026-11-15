package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/delisting"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// The delistings of currencies and the overrides of their capabilities
// (#216).

// DelistingList is the delistings of a currency, newest first.
type DelistingList struct {
	Items []delisting.Delisting `json:"items"`
}

// CancelDelistingRequest cancels a delisting.
type CancelDelistingRequest struct {
	// Version is the version of the delisting that the cancel is based on.
	Version *int64 `json:"version,omitempty" example:"1"`
}

// CapabilityOverrideRequest sets a capability of a currency.
type CapabilityOverrideRequest struct {
	Allowed *bool `json:"allowed"`
	// Note says why, at most 1000 characters.
	Note string `json:"note,omitempty"`
}

// currencyParam reads the currency hk of the path, in its canonical form.
func currencyParam(c *gin.Context) (string, bool) {
	id, err := uuid.Parse(c.Param("hk"))
	if err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_CURRENCY_ID"), nil)
		return "", false
	}
	return id.String(), true
}

// afterLifecycleChange follows a committed change of a lifecycle.
func (h *AdminHandler) afterLifecycleChange(ctx context.Context, change delisting.Change) {
	if change.Public {
		h.afterPublicChange(ctx, change.CurrencyHK)
	}
}

func decodeStrict(c *gin.Context, into any) bool {
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return false
	}
	return true
}

// @Summary Schedule the delisting of a currency
// @Description Schedules the steps that take an approved currency to delisted. A delisting that stopped before delisted, such as an emergency halt, stays scheduled, so that it can be cancelled to relist the currency; a new delisting supersedes it and takes the currency on from its state, with steps after it. A standard delisting starts with delist_announced; an emergency one, for a security or regulatory reason, at trading_halted or later. The steps follow the order of the lifecycle (delist_announced, deposits_disabled, sell_only, trading_halted, withdrawal_only, frozen, delisted), each at a later time, and any of them may be left out. Each step is applied at its time by the scheduler of the instances, exactly once, and changes the capabilities of the currency; a step that is due is applied at once. The settlement says what happens at frozen to the balances that are left: convert into another approved currency at a price, or hold for a claim. A conversion records its price once when the currency is frozen (the fixed price, or the reference price of the price engine: its mid for last_trade, its TWAP over the longest window for twap) and converts every available balance at it in the ledger, rounded down to the precision of the settlement currency; locked and pending funds convert once available. A delisting that reaches delisted passes through frozen, and its delisted step waits until no balance of the currency is left, so a claim holds the currency at frozen. Audited as currency.delisting_schedule, and each step as currency.lifecycle_change.
// @Tags admin currencies
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The currency's UUID" format(uuid)
// @Param body body delisting.Plan true "The delisting"
// @Success 201 {object} models.APIResponse{info=delisting.Delisting}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "not_listed or delisting_in_progress"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies/{hk}/delistings [post]
func (h *AdminHandler) ScheduleDelisting(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	hk, ok := currencyParam(c)
	if !ok {
		return
	}
	var plan delisting.Plan
	if !decodeStrict(c, &plan) {
		return
	}
	plan.Note, plan.AnnouncementURL = strings.TrimSpace(plan.Note), strings.TrimSpace(plan.AnnouncementURL)
	if errs := plan.Validate(time.Now()); len(errs) > 0 {
		sendValidationErrors(c, errs)
		return
	}
	origin, _, ok := restrictionOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	d, change, err := h.Delistings.Schedule(ctx, hk, plan, origin)
	switch {
	case errors.Is(err, delisting.ErrCurrencyNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("CURRENCY_NOT_FOUND"), nil)
	case errors.Is(err, delisting.ErrNotListed):
		helpers.SendError(c, http.StatusConflict, "not_listed", i18n.M("ONLY_APPROVED_CAN_BE_DELISTED"), nil)
	case errors.Is(err, delisting.ErrInProgress):
		helpers.SendError(c, http.StatusConflict, "delisting_in_progress", i18n.M("CURRENCY_ALREADY_DELISTING"), nil)
	case errors.Is(err, delisting.ErrStepsBehind):
		sendValidationErrors(c, fieldErrors{"steps": "must start after the lifecycle state that the currency is at"})
	case errors.Is(err, delisting.ErrSkipsFrozen):
		sendValidationErrors(c, fieldErrors{"steps": "must pass through frozen, where the balances that are left are settled, before delisted"})
	case errors.Is(err, delisting.ErrSettlementCurrency):
		sendValidationErrors(c, fieldErrors{"settlement_currency_hk": "must be another approved currency"})
	case err != nil:
		log.Printf("Failed to schedule the delisting of %s: %v", hk, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_SCHEDULE_DELISTING"), nil)
	default:
		h.afterLifecycleChange(ctx, change)
		helpers.SendResponse(c, http.StatusCreated, i18n.M("DELISTING_SCHEDULED"), d)
	}
}

// @Summary List the delistings of a currency
// @Description The delistings of a currency, newest first, at most 100, with their steps and when each was applied.
// @Tags admin currencies
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The currency's UUID" format(uuid)
// @Success 200 {object} models.APIResponse{info=DelistingList}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies/{hk}/delistings [get]
func (h *AdminHandler) ListDelistings(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	hk, ok := currencyParam(c)
	if !ok {
		return
	}
	list, err := h.Delistings.List(ctx, hk)
	if err != nil {
		log.Printf("Failed to list the delistings of %s: %v", hk, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LIST_DELISTINGS"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("DELISTINGS_RETRIEVED_SUCCESSFULLY"), DelistingList{Items: list})
}

// @Summary Cancel the delisting of a currency
// @Description Cancels a scheduled delisting before it reaches frozen: its steps that are left are not applied, and the currency returns to active with every capability of that state. At frozen and after, balances are settled, and a cancel answers 409 delisting_irreversible. With version, a cancel based on another version of the delisting answers 409 version_conflict. Audited as currency.delisting_cancel.
// @Tags admin currencies
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The currency's UUID" format(uuid)
// @Param id path string true "The delisting's id" format(uuid)
// @Param body body CancelDelistingRequest false "The version"
// @Success 200 {object} models.APIResponse{info=delisting.Delisting}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies/{hk}/delistings/{id}/cancel [post]
func (h *AdminHandler) CancelDelisting(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	hk, ok := currencyParam(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_DELISTING_ID"), nil)
		return
	}
	var req CancelDelistingRequest
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
	d, change, err := h.Delistings.Cancel(ctx, hk, id.String(), req.Version, origin)
	var conflict *delisting.VersionConflict
	switch {
	case errors.Is(err, delisting.ErrNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("DELISTING_NOT_FOUND"), nil)
	case errors.As(err, &conflict):
		helpers.SendError(c, http.StatusConflict, "version_conflict", i18n.M("DELISTING_VERSION_CONFLICT"),
			gin.H{"version": conflict.Current})
	case errors.Is(err, delisting.ErrNotScheduled):
		helpers.SendError(c, http.StatusConflict, "delisting_not_scheduled", i18n.M("DELISTING_ENDED"), nil)
	case errors.Is(err, delisting.ErrIrreversible):
		helpers.SendError(c, http.StatusConflict, "delisting_irreversible", i18n.M("DELISTING_PAST_CANCEL"), nil)
	case err != nil:
		log.Printf("Failed to cancel the delisting %s: %v", id, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CANCEL_DELISTING"), nil)
	default:
		h.afterLifecycleChange(ctx, change)
		helpers.SendResponse(c, http.StatusOK, i18n.M("DELISTING_CANCELLED"), d)
	}
}

// @Summary Override a capability of a currency
// @Description Sets one capability of a currency (deposit, withdraw, buy, sell, convert_into, convert_from, or listed) to allowed or refused, whatever its lifecycle state says, until the override is removed: for example to pause the deposits of one currency for an incident, or to keep withdrawals open at frozen. Audited as currency.capability_override.
// @Tags admin currencies
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The currency's UUID" format(uuid)
// @Param capability path string true "The capability" Enums(deposit,withdraw,buy,sell,convert_into,convert_from,listed)
// @Param body body CapabilityOverrideRequest true "The override"
// @Success 200 {object} models.APIResponse{info=models.CurrencyLifecycle}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies/{hk}/capabilities/{capability} [put]
func (h *AdminHandler) OverrideCapability(c *gin.Context) {
	var req CapabilityOverrideRequest
	if !decodeStrict(c, &req) {
		return
	}
	req.Note = strings.TrimSpace(req.Note)
	errs := fieldErrors{}
	if req.Allowed == nil {
		errs["allowed"] = "is required"
	}
	if len([]rune(req.Note)) > maxNote || strings.ContainsRune(req.Note, 0) {
		errs["note"] = "must be at most 1000 characters"
	}
	if len(errs) > 0 {
		sendValidationErrors(c, errs)
		return
	}
	h.changeCapability(c, req.Allowed, req.Note)
}

// @Summary Remove the override of a capability
// @Description The capability follows the lifecycle state of the currency again. Audited as currency.capability_override.
// @Tags admin currencies
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The currency's UUID" format(uuid)
// @Param capability path string true "The capability" Enums(deposit,withdraw,buy,sell,convert_into,convert_from,listed)
// @Success 200 {object} models.APIResponse{info=models.CurrencyLifecycle}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies/{hk}/capabilities/{capability} [delete]
func (h *AdminHandler) RemoveCapabilityOverride(c *gin.Context) {
	h.changeCapability(c, nil, "")
}

func (h *AdminHandler) changeCapability(c *gin.Context, allowed *bool, note string) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	hk, ok := currencyParam(c)
	if !ok {
		return
	}
	k := currencies.Capability(c.Param("capability"))
	if !slices.Contains(currencies.Capabilities, k) {
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("UNKNOWN_CAPABILITY"), nil)
		return
	}
	origin, _, ok := restrictionOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	var l *models.CurrencyLifecycle
	l, change, err := h.Delistings.Override(ctx, hk, k, allowed, note, origin)
	switch {
	case errors.Is(err, delisting.ErrCurrencyNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("CURRENCY_NOT_FOUND"), nil)
	case err != nil:
		log.Printf("Failed to override the %s capability of %s: %v", k, hk, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CHANGE_CAPABILITY"), nil)
	default:
		h.afterLifecycleChange(ctx, change)
		helpers.SendResponse(c, http.StatusOK, i18n.M("CAPABILITY_CHANGED"), l)
	}
}

// @Summary Show the settlement of a delisting
// @Description The settlement of the balances of the currency: before frozen, a preview of the holders and their remaining balances (available, locked, and pending), and, at a fixed price, the estimate of what they would be credited; from frozen on, the recorded price with its source and time, and from the ledger the conversions made, what they converted, what was credited, and the dust kept.
// @Tags admin currencies
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The currency's UUID" format(uuid)
// @Param id path string true "The delisting's id" format(uuid)
// @Success 200 {object} models.APIResponse{info=delisting.SettlementReport}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies/{hk}/delistings/{id}/settlement [get]
func (h *AdminHandler) GetDelistingSettlement(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	hk, ok := currencyParam(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_DELISTING_ID"), nil)
		return
	}
	report, err := h.Delistings.Report(ctx, hk, id.String())
	switch {
	case errors.Is(err, delisting.ErrNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("DELISTING_NOT_FOUND"), nil)
	case err != nil:
		log.Printf("Failed to read the settlement of the delisting %s: %v", id, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_DELISTING_SETTLEMENT"), nil)
	default:
		helpers.SendResponse(c, http.StatusOK, i18n.M("DELISTING_SETTLEMENT_RETRIEVED"), report)
	}
}

// @Summary Show the notices of a delisting
// @Description The notices that the delisting sent its users (#216): one for the last stage that each run of the scheduler applied (step:<state>), a reminder of the freeze on each of delisting.notices.reminder_days (reminder:<days>d), and one for its cancel. Each stage and the cancel go to every user by email, and to the users who hold the currency also by SMS when a provider has a template for it; a reminder goes to the holders only. Each notice shows when it was due, when its recipients were written, how many there are and how many held the currency, and its recipients by outcome: sent, no_contact (no verified contact that a channel reaches), stale (the delisting was cancelled, or the account deleted, before it went), failed (after the last attempt), and pending.
// @Tags admin currencies
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The currency's UUID" format(uuid)
// @Param id path string true "The delisting's id" format(uuid)
// @Success 200 {object} models.APIResponse{info=[]delisting.NoticeReport}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies/{hk}/delistings/{id}/notices [get]
func (h *AdminHandler) GetDelistingNotices(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	hk, ok := currencyParam(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_DELISTING_ID"), nil)
		return
	}
	notices, err := h.Delistings.Notices(ctx, hk, id.String())
	switch {
	case errors.Is(err, delisting.ErrNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("DELISTING_NOT_FOUND"), nil)
	case err != nil:
		log.Printf("Failed to read the notices of the delisting %s: %v", id, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_DELISTING_NOTICES"), nil)
	default:
		helpers.SendResponse(c, http.StatusOK, i18n.M("DELISTING_NOTICES_RETRIEVED"), notices)
	}
}
