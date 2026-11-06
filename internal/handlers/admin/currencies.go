package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/adminauth"
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
	err = tx.QueryRowContext(ctx,
		"INSERT INTO currencies (status, info) VALUES ($1, $2::jsonb) RETURNING hk",
		status, infoJSON,
	).Scan(&generatedHK)
	var public bool
	if err == nil {
		public, err = enqueuePublicEvent(ctx, tx, false, models.Currency{HK: generatedHK, Status: status, Info: info})
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

	helpers.SendResponse(c, http.StatusCreated, "Currency created successfully.", gin.H{"hk": generatedHK, "status": status})
}

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

	// The row is locked from the read to the write, so that concurrent
	// partial updates of one currency are applied one after the other
	// instead of overwriting each other's fields.
	tx, err := h.PostgresDB.DB.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("Error starting a currency update: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to update currency due to an unexpected error.", nil)
		return
	}
	defer tx.Rollback()

	var currentStatus string
	var infoData []byte
	err = tx.QueryRowContext(ctx,
		"SELECT status, info FROM currencies WHERE hk = $1 AND deleted_at IS NULL FOR UPDATE", hk,
	).Scan(&currentStatus, &infoData)
	if err != nil {
		if err == sql.ErrNoRows {
			helpers.SendResponse(c, http.StatusNotFound, "Currency not found", nil)
			return
		}
		log.Printf("Error retrieving currency data: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to retrieve currency data.", nil)
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

	_, err = tx.ExecContext(ctx,
		"UPDATE currencies SET status=$1, info=$2::jsonb, updated_at=NOW() WHERE hk=$3 AND deleted_at IS NULL",
		currentStatus, infoJSON, hk,
	)
	var public bool
	if err == nil {
		public, err = enqueuePublicEvent(ctx, tx, wasPublic, models.Currency{HK: hk, Status: currentStatus, Info: currentInfo})
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

	helpers.SendResponse(c, http.StatusOK, "Currency updated successfully.", nil)
}

func (h *AdminHandler) DeleteCurrency(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	hk := c.Param("hk")
	if _, err := uuid.Parse(hk); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid UUID format for HK", nil)
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
	err = tx.QueryRowContext(ctx,
		"SELECT (deleted_at IS NOT NULL), status FROM currencies WHERE hk = $1 FOR UPDATE", hk,
	).Scan(&isDeleted, &previousStatus)
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

	_, err = tx.ExecContext(ctx,
		"UPDATE currencies SET deleted_at=NOW(), status='deleted' WHERE hk=$1", hk,
	)
	var public bool
	if err == nil {
		public, err = enqueuePublicEvent(ctx, tx, previousStatus == models.CurrencyStatusApproved,
			models.Currency{HK: hk, Status: models.CurrencyStatusDeleted})
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

	helpers.SendResponse(c, http.StatusOK, "Currency deleted successfully.", nil)
}
