package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/adminauth"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/outbox"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type AdminHandler struct {
	PostgresDB      *db.PostgresService
	RedisClient     *db.RedisService
	RabbitMQService *rabbitmq.RabbitMQService
	// Outbox is the relay that publishes the currency events, which changes
	// write to the outbox. Without it, they wait there for the relay of
	// another instance.
	Outbox nudger
	// Currencies holds the list of public currencies, which changes drop.
	Currencies *currencies.Catalog

	// Admin sign-in. Accounts is nil, and sign-in unavailable, until the
	// server configures admin.totp_key.
	Accounts *adminauth.Service
	// OTP issues the emailed sign-in codes, in its own namespace.
	OTP *otp.Store
	// OTPSender delivers the emailed codes.
	OTPSender notify.OTPSender
}

func NewHandler(is *models.InfraServices) *AdminHandler {
	h := &AdminHandler{
		PostgresDB:      is.Postgres,
		RedisClient:     is.Redis,
		RabbitMQService: is.RabbitMQ,
		OTPSender:       notify.DiscardSender{},
		Currencies:      currencies.ForServices(is),
	}
	if is.Redis != nil {
		settings := otp.SettingsFromConfig()
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

// publicEvent returns the event of a change to currency in the public view,
// which only approved currencies are in, or nil when the view does not
// change:
//
//   - a currency that becomes public is announced as created,
//   - a public currency that changes is announced as updated,
//   - a currency that stops being public, by a status change or deletion,
//     is announced as deleted,
//   - changes to a currency that stays hidden are not announced at all.
func publicEvent(wasPublic bool, currency models.Currency) ([]byte, error) {
	isPublic := currency.IsPublic()
	var action string
	var info interface{} = currency.Info
	switch {
	case isPublic && !wasPublic:
		action = models.CurrencyEventCreate
	case isPublic && wasPublic:
		action = models.CurrencyEventUpdate
	case !isPublic && wasPublic:
		action = models.CurrencyEventDelete
		info = models.CurrencyRemoval{Status: currency.Status}
	default:
		return nil, nil
	}
	infoJSON, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	// Version 7 UUIDs grow with time, so the IDs also sort the events.
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	return json.Marshal(models.CurrencyEvent{
		ID:         id.String(),
		Entity:     models.CurrencyEventEntity,
		Action:     action,
		HK:         currency.HK,
		Info:       infoJSON,
		ServerTime: time.Now().UTC().Format(time.RFC3339),
	})
}

// enqueuePublicEvent writes the event of a change, if it has one, to the
// outbox in tx, so that it is published if and only if the change commits.
// It reports whether the change touches the public view.
func enqueuePublicEvent(ctx context.Context, tx *sql.Tx, wasPublic bool, currency models.Currency) (bool, error) {
	event, err := publicEvent(wasPublic, currency)
	if err != nil || event == nil {
		return false, err
	}
	return true, outbox.Enqueue(ctx, tx, models.CurrencyEventTopic, event)
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
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid input: Please check the provided data format.", nil)
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

	infoJSON, err := json.Marshal(info)
	if err != nil {
		log.Printf("Error marshalling CurrencyInfo to JSON: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Internal error: Failed to process currency information.", nil)
		return
	}

	// The currency and its event are written together.
	tx, err := h.PostgresDB.DB.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("Error starting a currency create: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to create currency due to an unexpected error.", nil)
		return
	}
	defer tx.Rollback()

	var generatedHK string
	var version int64
	err = tx.QueryRowContext(ctx,
		"INSERT INTO currencies (status, info) VALUES ($1, $2::jsonb) RETURNING hk, version",
		status, infoJSON,
	).Scan(&generatedHK, &version)
	var public bool
	if err == nil {
		public, err = enqueuePublicEvent(ctx, tx, false, models.Currency{HK: generatedHK, Status: status, Info: info})
	}
	if err == nil && !h.recordChange(ctx, c, tx, h.currencyEvent(c, audit.ActionCurrencyCreate, generatedHK,
		&audit.Changes{After: currencyFields(status, info)})) {
		return
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		sendCurrencyWriteError(c, err, "create")
		return
	}
	if public {
		h.afterPublicChange(ctx, generatedHK)
	}

	helpers.SendResponse(c, http.StatusCreated, "Currency created successfully.", gin.H{"hk": generatedHK, "status": status, "version": version})
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
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid UUID format for HK", nil)
		return
	}

	req, fieldErrs, err := decodeCurrencyRequest(c)
	if err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid input: Please check the provided data format.", nil)
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

	// The row is locked from the read to the write, so that concurrent
	// partial updates of one currency are applied one after the other
	// instead of overwriting each other's fields. An update that sends the
	// version it is based on is refused if another one came first.
	tx, err := h.PostgresDB.DB.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("Error starting a currency update: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to update currency due to an unexpected error.", nil)
		return
	}
	defer tx.Rollback()

	var currentStatus string
	var infoData []byte
	var currentVersion int64
	err = tx.QueryRowContext(ctx,
		"SELECT status, info, version FROM currencies WHERE hk = $1 AND deleted_at IS NULL FOR UPDATE", hk,
	).Scan(&currentStatus, &infoData, &currentVersion)
	if err != nil {
		if err == sql.ErrNoRows {
			helpers.SendResponse(c, http.StatusNotFound, "Currency not found", nil)
			return
		}
		log.Printf("Error retrieving currency data: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to retrieve currency data.", nil)
		return
	}

	if req.Version != nil && *req.Version != currentVersion {
		sendVersionConflict(c, currentVersion)
		return
	}

	var currentInfo models.CurrencyInfo
	if err := json.Unmarshal(infoData, &currentInfo); err != nil {
		log.Printf("Error unmarshalling CurrencyInfo: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to process currency information.", nil)
		return
	}

	wasPublic := currentStatus == models.CurrencyStatusApproved

	// Only the fields in the request change. The result is validated as a
	// whole, so an update cannot leave a currency invalid.
	before := currencyFields(currentStatus, currentInfo)
	currentStatus, currentInfo = req.apply(currentStatus, currentInfo)
	if errs := validateCurrency(currentStatus, currentInfo); errs != nil {
		sendValidationErrors(c, errs)
		return
	}

	infoJSON, err := json.Marshal(currentInfo)
	if err != nil {
		log.Printf("Error marshalling CurrencyInfo to JSON: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Internal error: Failed to process currency information.", nil)
		return
	}

	var newVersion int64
	err = tx.QueryRowContext(ctx,
		"UPDATE currencies SET status=$1, info=$2::jsonb, version=version+1, updated_at=NOW() WHERE hk=$3 AND deleted_at IS NULL RETURNING version",
		currentStatus, infoJSON, hk,
	).Scan(&newVersion)
	var public bool
	if err == nil {
		public, err = enqueuePublicEvent(ctx, tx, wasPublic, models.Currency{HK: hk, Status: currentStatus, Info: currentInfo})
	}
	if err == nil && !h.recordChange(ctx, c, tx, h.currencyEvent(c, audit.ActionCurrencyUpdate, hk,
		changedFields(before, currencyFields(currentStatus, currentInfo)))) {
		return
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		sendCurrencyWriteError(c, err, "update")
		return
	}
	if public {
		h.afterPublicChange(ctx, hk)
	}

	helpers.SendResponse(c, http.StatusOK, "Currency updated successfully.", gin.H{"version": newVersion})
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
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid UUID format for HK", nil)
		return
	}
	expected, ok := versionQuery(c)
	if !ok {
		sendValidationErrors(c, fieldErrors{"version": "must be a number of at least 1"})
		return
	}

	// The row is locked from the read to the write, so that the previous
	// status, which decides the event, is the one this delete replaces, and
	// concurrent deletes of one currency succeed only once.
	tx, err := h.PostgresDB.DB.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("Error starting a currency delete: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to delete currency due to an unexpected error.", nil)
		return
	}
	defer tx.Rollback()

	var isDeleted bool
	var previousStatus string
	var currentVersion int64
	err = tx.QueryRowContext(ctx,
		"SELECT (deleted_at IS NOT NULL), status, version FROM currencies WHERE hk = $1 FOR UPDATE", hk,
	).Scan(&isDeleted, &previousStatus, &currentVersion)
	if err != nil {
		if err == sql.ErrNoRows {
			helpers.SendResponse(c, http.StatusNotFound, "Currency not found", nil)
		} else {
			log.Printf("Error checking currency in database: %v", err)
			helpers.SendResponse(c, http.StatusInternalServerError, "Failed to delete currency due to an unexpected error.", nil)
		}
		return
	}

	if isDeleted {
		helpers.SendResponse(c, http.StatusNotFound, "Currency not found or already deleted", nil)
		return
	}
	if expected != nil && *expected != currentVersion {
		sendVersionConflict(c, currentVersion)
		return
	}

	var newVersion int64
	err = tx.QueryRowContext(ctx,
		"UPDATE currencies SET deleted_at=NOW(), status='deleted', version=version+1 WHERE hk=$1 RETURNING version", hk,
	).Scan(&newVersion)
	var public bool
	if err == nil {
		public, err = enqueuePublicEvent(ctx, tx, previousStatus == models.CurrencyStatusApproved,
			models.Currency{HK: hk, Status: models.CurrencyStatusDeleted})
	}
	if err == nil && !h.recordChange(ctx, c, tx, h.currencyEvent(c, audit.ActionCurrencyDelete, hk, &audit.Changes{
		Before: map[string]interface{}{"status": previousStatus},
		After:  map[string]interface{}{"status": models.CurrencyStatusDeleted},
	})) {
		return
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		log.Printf("Error deleting currency in database: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to delete currency due to an unexpected error.", nil)
		return
	}
	if public {
		h.afterPublicChange(ctx, hk)
	}

	helpers.SendResponse(c, http.StatusOK, "Currency deleted successfully.", gin.H{"version": newVersion})
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
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid UUID format for HK", nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var currency models.Currency
	var info []byte
	var created, updated, deleted sql.NullTime
	err = h.PostgresDB.DB.QueryRowContext(ctx,
		"SELECT id, hk, status, info, version, created_at, updated_at, deleted_at FROM currencies WHERE hk = $1", id.String(),
	).Scan(&currency.ID, &currency.HK, &currency.Status, &info, &currency.Version, &created, &updated, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		helpers.SendResponse(c, http.StatusNotFound, "Currency not found", nil)
		return
	}
	if err == nil {
		err = json.Unmarshal(info, &currency.Info)
	}
	if err != nil {
		log.Printf("Error reading currency %s: %v", id, err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to retrieve currency data.", nil)
		return
	}
	currency.CreatedAt, currency.UpdatedAt = timestamp(created), timestamp(updated)
	if deleted.Valid {
		at := timestamp(deleted)
		currency.DeletedAt = &at
	}
	helpers.SendResponse(c, http.StatusOK, "Currency retrieved successfully.", currency)
}

// timestamp formats a database time in RFC 3339, in UTC, or as "" when it is
// NULL.
func timestamp(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}

// recordChange writes the audit event of a change in its transaction, and
// reports whether it did. When it cannot, it answers 500, and the caller
// returns, so that the transaction rolls back: no change without its event.
// The failure is the server's, never a problem of the request, whatever
// constraint the event broke.
func (h *AdminHandler) recordChange(ctx context.Context, c *gin.Context, tx *sql.Tx, e audit.Event) bool {
	if err := audit.Record(ctx, tx, e); err != nil {
		log.Printf("Refused the change %s of %s, which could not be recorded: %v", e.Action, e.TargetID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to record the change.", nil)
		return false
	}
	return true
}

// currencyEvent is the audit event of a change to a currency by the
// administrator of the request. Without an administrator, the event, and so
// the change, is refused.
func (h *AdminHandler) currencyEvent(c *gin.Context, action, hk string, changes *audit.Changes) audit.Event {
	e := audit.FromRequest(c)
	e.ActorType, e.Action, e.Outcome = audit.ActorAdmin, action, audit.Success
	if claims, ok := auth.GetClaims(c); ok {
		e.ActorID = claims.UUID
	}
	e.TargetType, e.TargetID = audit.TargetCurrency, hk
	e.Changes = changes
	return e
}

// currencyFields are the fields of a currency as the audit log records them.
func currencyFields(status string, info models.CurrencyInfo) map[string]interface{} {
	return map[string]interface{}{
		"status": status, "en_name": info.EnName, "fa_name": info.FaName, "symbol": info.Symbol,
		"if_fiat": info.IfFiat, "is_token": info.IsToken, "contract": info.Contract,
	}
}

// changedFields keeps the fields whose values differ between before and
// after.
func changedFields(before, after map[string]interface{}) *audit.Changes {
	changes := &audit.Changes{Before: map[string]interface{}{}, After: map[string]interface{}{}}
	for name, value := range after {
		if before[name] != value {
			changes.Before[name], changes.After[name] = before[name], value
		}
	}
	return changes
}
