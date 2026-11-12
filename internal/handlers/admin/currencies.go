package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/delisting"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/rebrand"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// AdminHandler serves the admin routes. Every dependency is an interface
// (deps.go, #48); a nil one makes its routes unavailable.
type AdminHandler struct {
	// Config is the configuration of the process (#48). NewHandler sets it;
	// nil reads as the zero configuration.
	Config *config.Configuration
	// The stores of the routes.
	CurrencyStore CurrencyStore
	Restrictions  RestrictionStore
	Controls      ControlStore
	Delistings    DelistingStore
	Rebrands      RebrandStore
	Audit         AuditLog
	Findings      FindingStore
	// Outbox is the relay that publishes the currency events, which changes
	// write to the outbox. Without it, they wait there for the relay of
	// another instance.
	Outbox nudger
	// Currencies holds the list of public currencies, which changes drop.
	Currencies CatalogCache

	// Admin sign-in. Accounts is nil, and sign-in unavailable, without the
	// data keys.
	Accounts Accounts
	// OTP issues the emailed sign-in codes, in its own namespace.
	OTP CodeStore
	// OTPSender delivers the emailed codes.
	OTPSender notify.OTPSender

	// Platform is this instance's copy of the platform controls (#235),
	// which a change refreshes at once. LiftApproval are the features whose
	// active lockdown needs a second administrator to lift.
	Platform     ControlRefresher
	LiftApproval []platform.Feature
}

// NewHandler returns the handler with the stores of the services in is. A
// service that is not there leaves its dependencies nil, never a typed nil.
func NewHandler(is *models.InfraServices) *AdminHandler {
	h := &AdminHandler{
		Config:    is.Settings(),
		OTPSender: notify.DiscardSender{},
	}
	if is.Postgres != nil {
		db := is.Postgres.DB
		h.CurrencyStore = currencies.Store{DB: db}
		h.Restrictions = restrictions.Store{DB: db}
		h.Controls = platform.Store{DB: db}
		h.Delistings = delisting.Store{DB: db}
		h.Rebrands = rebrand.Store{DB: db}
		h.Audit = audit.Log{DB: db}
		h.Findings = rowseal.Findings{DB: db}
	}
	if catalog := currencies.ForServices(is); catalog != nil {
		h.Currencies = catalog
	}
	if is.Redis != nil {
		settings := otp.SettingsOf(is.Settings())
		settings.Namespace = otpNamespace
		h.OTP = otp.NewStore(is.Redis.Client, settings)
	}
	return h
}

// nudger makes the outbox relay look for messages. *outbox.Relay
// implements it.
type nudger interface {
	Nudge()
}

// syncTimeout bounds the steps that follow a committed currency change.
const syncTimeout = 10 * time.Second

// afterPublicChange runs once a change to the public view has committed. It
// drops the cached list, so that the next read loads it from PostgreSQL, and
// nudges the outbox relay, which publishes the event and drops the list
// again: a drop that fails here is retried there. The change is committed,
// so these steps run even when the client stops waiting, within
// syncTimeout.
func (h *AdminHandler) afterPublicChange(ctx context.Context, hk string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncTimeout)
	defer cancel()
	if h.Currencies != nil {
		if err := h.Currencies.Invalidate(ctx); err != nil {
			log.Printf("Error dropping the cached currency list after a change to %s; the outbox relay retries: %v", hk, err)
		}
	}
	if h.Outbox != nil {
		h.Outbox.Nudge()
	}
}

// @Summary Create a currency
// @Tags admin currencies
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body currencyRequest true "The currency"
// @Success 201 {object} models.APIResponse{info=models.CurrencyCreatedInfo}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies [post]
func (h *AdminHandler) CreateCurrency(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	req, fieldErrs, err := decodeCurrencyRequest(c)
	if err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	if fieldErrs != nil {
		sendValidationErrors(c, fieldErrs)
		return
	}
	if req.Version != nil {
		sendValidationErrors(c, fieldErrors{"version": "is set by the server"})
		return
	}
	status, info := req.apply(defaultCurrencyStatus, models.CurrencyInfo{})
	if errs := validateCurrency(status, info); errs != nil {
		sendValidationErrors(c, errs)
		return
	}

	// The currency, its event, and its record are written together.
	w, err := h.CurrencyStore.Create(ctx, status, info, h.currencyOrigin(c))
	if err != nil {
		sendCurrencyWriteError(c, err, "create")
		return
	}
	if w.Public {
		h.afterPublicChange(ctx, w.HK)
	}
	helpers.SendResponse(c, http.StatusCreated, i18n.M("CURRENCY_CREATED_SUCCESSFULLY"), gin.H{"hk": w.HK, "status": status, "version": w.Version})
}

// @Summary Update a currency
// @Description Changes only the fields sent. With version, the update is refused with 409 version_conflict when the currency has changed since.
// @Tags admin currencies
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The currency's UUID" format(uuid)
// @Param body body currencyRequest true "The fields to change, and optionally the version the change is based on"
// @Success 200 {object} models.APIResponse{info=models.VersionInfo}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies/{hk} [put]
func (h *AdminHandler) UpdateCurrency(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	hk := c.Param("hk")
	if _, err := uuid.Parse(hk); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_CURRENCY_ID"), nil)
		return
	}

	req, fieldErrs, err := decodeCurrencyRequest(c)
	if err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	if fieldErrs != nil {
		sendValidationErrors(c, fieldErrs)
		return
	}
	if req.Version != nil && *req.Version < 1 {
		sendValidationErrors(c, fieldErrors{"version": "must be at least 1"})
		return
	}

	// Only the fields in the request change. The result is validated as a
	// whole, so an update cannot leave a currency invalid. The store locks
	// the row from the read to the write, so that concurrent partial updates
	// are applied one after the other, and refuses an update based on a
	// version that another one replaced.
	edit := func(status string, info models.CurrencyInfo) (string, models.CurrencyInfo, error) {
		status, info = req.apply(status, info)
		if errs := validateCurrency(status, info); errs != nil {
			return status, info, errs
		}
		return status, info, nil
	}
	w, err := h.CurrencyStore.Update(ctx, hk, req.Version, edit, h.currencyOrigin(c))
	var invalid fieldErrors
	var conflict *currencies.VersionConflict
	switch {
	case errors.As(err, &invalid):
		sendValidationErrors(c, invalid)
		return
	case errors.Is(err, currencies.ErrNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("CURRENCY_NOT_FOUND"), nil)
		return
	case errors.As(err, &conflict):
		sendVersionConflict(c, conflict.Current)
		return
	case err != nil:
		sendCurrencyWriteError(c, err, "update")
		return
	}
	if w.Public {
		h.afterPublicChange(ctx, hk)
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("CURRENCY_UPDATED_SUCCESSFULLY"), gin.H{"version": w.Version})
}

// @Summary Delete a currency
// @Description Soft-deletes the currency. With version, the delete is refused with 409 version_conflict when the currency has changed since.
// @Tags admin currencies
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The currency's UUID" format(uuid)
// @Param version query int false "The version the delete is based on" minimum(1)
// @Success 200 {object} models.APIResponse{info=models.VersionInfo}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies/{hk} [delete]
func (h *AdminHandler) DeleteCurrency(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	hk := c.Param("hk")
	if _, err := uuid.Parse(hk); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_CURRENCY_ID"), nil)
		return
	}
	expected, ok := versionQuery(c)
	if !ok {
		sendValidationErrors(c, fieldErrors{"version": "must be a number of at least 1"})
		return
	}

	w, err := h.CurrencyStore.Delete(ctx, hk, expected, h.currencyOrigin(c))
	var conflict *currencies.VersionConflict
	switch {
	case errors.Is(err, currencies.ErrNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("CURRENCY_NOT_FOUND"), nil)
		return
	case errors.Is(err, currencies.ErrAlreadyDeleted):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("CURRENCY_NOT_FOUND_OR_ALREADY_DELETED"), nil)
		return
	case errors.As(err, &conflict):
		sendVersionConflict(c, conflict.Current)
		return
	case err != nil:
		sendUnrecordedOr(c, err, "delete", i18n.M("CURRENCY_DELETE_FAILED"))
		return
	}
	if w.Public {
		h.afterPublicChange(ctx, hk)
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("CURRENCY_DELETED_SUCCESSFULLY"), gin.H{"version": w.Version})
}

// @Summary Restore a deleted currency
// @Description Undoes a delete: the currency keeps its hk, symbol, and contract, and comes back as pending, so that it is public again only once an administrator approves it. With version, the restore is refused with 409 version_conflict when the currency has changed since. A currency that is not deleted answers 409 not_deleted.
// @Tags admin currencies
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The currency's UUID" format(uuid)
// @Param version query int false "The version the restore is based on" minimum(1)
// @Success 200 {object} models.APIResponse{info=models.VersionInfo}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies/{hk}/restore [post]
func (h *AdminHandler) RestoreCurrency(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	hk := c.Param("hk")
	if _, err := uuid.Parse(hk); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_CURRENCY_ID"), nil)
		return
	}
	expected, ok := versionQuery(c)
	if !ok {
		sendValidationErrors(c, fieldErrors{"version": "must be a number of at least 1"})
		return
	}

	// A pending currency is not public, so the restore publishes no event and
	// leaves the cached public list as it is.
	w, err := h.CurrencyStore.Restore(ctx, hk, expected, h.currencyOrigin(c))
	var conflict *currencies.VersionConflict
	switch {
	case errors.Is(err, currencies.ErrNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("CURRENCY_NOT_FOUND"), nil)
		return
	case errors.As(err, &conflict):
		sendVersionConflict(c, conflict.Current)
		return
	case errors.Is(err, currencies.ErrNotDeleted):
		helpers.SendError(c, http.StatusConflict, "not_deleted", i18n.M("CURRENCY_NOT_DELETED"), gin.H{"code": "not_deleted"})
		return
	case err != nil:
		sendUnrecordedOr(c, err, "restore", i18n.M("CURRENCY_RESTORE_FAILED"))
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("CURRENCY_RESTORED_SUCCESSFULLY"), gin.H{"version": w.Version})
}

// GetCurrency returns one currency, whatever its status, with its version:
// what an update or a delete sends to be refused if the currency changed in
// the meantime.
//
// @Summary Get a currency with its version
// @Description Whatever its status, deleted ones included.
// @Tags admin currencies
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The currency's UUID" format(uuid)
// @Success 200 {object} models.APIResponse{info=models.Currency}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies/{hk} [get]
func (h *AdminHandler) GetCurrency(c *gin.Context) {
	id, err := uuid.Parse(c.Param("hk"))
	if err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_CURRENCY_ID"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	currency, err := h.CurrencyStore.Get(ctx, id.String())
	if errors.Is(err, currencies.ErrNotFound) {
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("CURRENCY_NOT_FOUND"), nil)
		return
	}
	if err != nil {
		log.Printf("Error reading currency %s: %v", id, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_RETRIEVE_CURRENCY_DATA"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("CURRENCY_RETRIEVED_SUCCESSFULLY"), currency)
}

// currencyOrigin is the origin of the audit event of a change to a currency
// by the administrator of the request. Without an administrator, the event,
// and so the change, is refused.
func (h *AdminHandler) currencyOrigin(c *gin.Context) audit.Event {
	e := audit.FromRequest(c)
	e.ActorType = audit.ActorAdmin
	if claims, ok := auth.GetClaims(c); ok {
		e.ActorID = claims.UUID
	}
	return e
}

// sendUnrecordedOr answers a change that failed: 500 with
// FAILED_TO_RECORD_THE_CHANGE when its audit event could not be written,
// which rolled it back (no change without its record), and otherwise 500
// with failure. The failure is the server's, never a problem of the
// request.
func sendUnrecordedOr(c *gin.Context, err error, action string, failure i18n.Message) {
	if errors.Is(err, currencies.ErrUnrecorded) {
		log.Printf("Refused the currency %s, which could not be recorded: %v", action, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_RECORD_THE_CHANGE"), nil)
		return
	}
	log.Printf("Failed to %s a currency: %v", action, err)
	helpers.SendResponse(c, http.StatusInternalServerError, failure, nil)
}

// zeroConfig is the configuration of a handler without one.
var zeroConfig config.Configuration

// settings returns the configuration of the handler, or the zero
// configuration without one.
func (h *AdminHandler) settings() *config.Configuration {
	if h.Config == nil {
		return &zeroConfig
	}
	return h.Config
}
