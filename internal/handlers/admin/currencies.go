package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/adminauth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type AdminHandler struct {
	PostgresDB      *db.PostgresService
	RedisClient     *db.RedisService
	RabbitMQService *rabbitmq.RabbitMQService
	// Events publishes currency events. It is nil without RabbitMQ.
	Events eventPublisher
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
	if is.RabbitMQ != nil {
		h.Events = is.RabbitMQ
	}
	if is.Redis != nil {
		settings := otp.SettingsFromConfig()
		settings.Namespace = otpNamespace
		h.OTP = otp.NewStore(is.Redis.Client, settings)
	}
	return h
}

// eventPublisher publishes currency events. *rabbitmq.RabbitMQService
// implements it.
type eventPublisher interface {
	Publish(ctx context.Context, exchange, message string) error
}

func (h *AdminHandler) publishCurrencyEvent(ctx context.Context, action, hk string, info interface{}) {
	infoJSON, err := json.Marshal(info)
	if err != nil {
		log.Printf("Error marshaling currency event info: %v", err)
		return
	}

	event := models.CurrencyEvent{
		Entity:     models.CurrencyEventEntity,
		Action:     action,
		HK:         hk,
		Info:       infoJSON,
		ServerTime: time.Now().UTC().Format(time.RFC3339),
	}

	eventJSON, err := json.Marshal(event)
	if err != nil {
		log.Printf("Error marshaling currency event: %v", err)
		return
	}

	if h.Events == nil {
		return
	}
	if err := h.Events.Publish(ctx, config.Config.RabbitMQ.Exchanges.PriceUpdates, string(eventJSON)); err != nil {
		log.Printf("Failed to publish currency %s: %v", action, err)
	}
}

// syncTimeout bounds the steps that follow a committed currency change.
const syncTimeout = 10 * time.Second

// syncPublicView brings the list of public currencies, which public
// responses read, and the events, which WebSocket clients receive, in line
// with the visibility of currency after a committed change. Only approved
// currencies are public:
//
//   - a currency that becomes public is announced as created,
//   - a public currency that changes is announced as updated,
//   - a currency that stops being public, by a status change or deletion,
//     is announced as deleted,
//   - changes to a currency that stays hidden are not announced at all.
//
// The cached list is dropped when the currency was or is public, and the
// next read loads it from PostgreSQL. The change is committed, so these
// steps run even when the client stops waiting, within syncTimeout.
func (h *AdminHandler) syncPublicView(ctx context.Context, wasPublic bool, currency models.Currency) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncTimeout)
	defer cancel()
	isPublic := currency.IsPublic()
	if (isPublic || wasPublic) && h.Currencies != nil {
		if err := h.Currencies.Invalidate(ctx); err != nil {
			log.Printf("Error dropping the cached currency list after a change to %s, which stays out of date for at most redis.currency_cache_ttl_seconds: %v",
				currency.HK, err)
		}
	}

	switch {
	case isPublic && !wasPublic:
		h.publishCurrencyEvent(ctx, models.CurrencyEventCreate, currency.HK, currency.Info)
	case isPublic && wasPublic:
		h.publishCurrencyEvent(ctx, models.CurrencyEventUpdate, currency.HK, currency.Info)
	case !isPublic && wasPublic:
		h.publishCurrencyEvent(ctx, models.CurrencyEventDelete, currency.HK, models.CurrencyRemoval{Status: currency.Status})
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

	var generatedHK string
	err = h.PostgresDB.DB.QueryRowContext(ctx,
		"INSERT INTO currencies (status, info) VALUES ($1, $2::jsonb) RETURNING hk",
		status, infoJSON,
	).Scan(&generatedHK)
	if err != nil {
		sendCurrencyWriteError(c, err, "create")
		return
	}

	h.syncPublicView(ctx, false, models.Currency{HK: generatedHK, Status: status, Info: info})

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
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		sendCurrencyWriteError(c, err, "update")
		return
	}

	h.syncPublicView(ctx, wasPublic, models.Currency{HK: hk, Status: currentStatus, Info: currentInfo})

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
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		log.Printf("Error deleting currency in database: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to delete currency due to an unexpected error.", nil)
		return
	}

	h.syncPublicView(ctx, previousStatus == models.CurrencyStatusApproved,
		models.Currency{HK: hk, Status: models.CurrencyStatusDeleted})

	helpers.SendResponse(c, http.StatusOK, "Currency deleted successfully.", nil)
}
