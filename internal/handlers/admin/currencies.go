package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"encrypted-db/config"
	"encrypted-db/internal/adminauth"
	"encrypted-db/internal/db"
	"encrypted-db/internal/helpers"
	"encrypted-db/internal/models"
	"encrypted-db/internal/notify"
	"encrypted-db/internal/otp"
	"encrypted-db/internal/rabbitmq"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type AdminHandler struct {
	PostgresDB      *db.PostgresService
	RedisClient     *db.RedisService
	RabbitMQService *rabbitmq.RabbitMQService
	// Events publishes currency events. It is nil without RabbitMQ.
	Events eventPublisher

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

// Currency event actions. They describe the public view: what clients that
// are not administrators can see.
const (
	currencyEventCreate = "create"
	currencyEventUpdate = "update"
	currencyEventDelete = "delete"
)

type CurrencyEvent struct {
	Entity     string          `json:"entity"`
	Action     string          `json:"action"`
	HK         string          `json:"hk"`
	Info       json.RawMessage `json:"info,omitempty"`
	ServerTime string          `json:"server_time"`
}

func (h *AdminHandler) publishCurrencyEvent(ctx context.Context, action, hk string, info interface{}) {
	infoJSON, err := json.Marshal(info)
	if err != nil {
		log.Printf("Error marshaling currency event info: %v", err)
		return
	}

	event := CurrencyEvent{
		Entity:     "currencies",
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

// syncPublicView brings the cache, which public and user responses read, and
// the events, which WebSocket clients receive, in line with the visibility
// of currency after a change. Only approved currencies are public:
//
//   - a currency that becomes public is cached and announced as created,
//   - a public currency that changes is cached and announced as updated,
//   - a currency that stops being public, by a status change or deletion,
//     leaves the cache and is announced as deleted,
//   - changes to a currency that stays hidden are not announced at all.
func (h *AdminHandler) syncPublicView(ctx context.Context, wasPublic bool, currency models.Currency) {
	isPublic := currency.IsPublic()
	if isPublic {
		if err := h.AddOrUpdateCurrencyInCache(ctx, currency); err != nil {
			log.Printf("Error caching currency %s: %v", currency.HK, err)
		}
	} else if err := h.DeleteCurrencyFromCache(ctx, currency.HK); err != nil {
		log.Printf("Error removing currency %s from the cache: %v", currency.HK, err)
	}

	switch {
	case isPublic && !wasPublic:
		h.publishCurrencyEvent(ctx, currencyEventCreate, currency.HK, currency.Info)
	case isPublic && wasPublic:
		h.publishCurrencyEvent(ctx, currencyEventUpdate, currency.HK, currency.Info)
	case !isPublic && wasPublic:
		h.publishCurrencyEvent(ctx, currencyEventDelete, currency.HK, map[string]string{"status": currency.Status})
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

	var isDeleted bool
	var previousStatus string
	err := h.PostgresDB.DB.QueryRowContext(ctx,
		"SELECT (deleted_at IS NOT NULL), status FROM currencies WHERE hk = $1", hk,
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

	_, err = h.PostgresDB.DB.ExecContext(ctx,
		"UPDATE currencies SET deleted_at=NOW(), status='deleted' WHERE hk=$1", hk,
	)
	if err != nil {
		log.Printf("Error deleting currency in database: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to delete currency due to an unexpected error.", nil)
		return
	}

	h.syncPublicView(ctx, previousStatus == models.CurrencyStatusApproved,
		models.Currency{HK: hk, Status: models.CurrencyStatusDeleted})

	helpers.SendResponse(c, http.StatusOK, "Currency deleted successfully.", nil)
}

func (h *AdminHandler) CleanupBaseDefinitions(ctx context.Context) error {
	pattern := "base_definitions:currency:*"

	iter := h.RedisClient.Client.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		if err := h.RedisClient.Client.Del(ctx, iter.Val()).Err(); err != nil {
			log.Printf("Failed to delete key %s from Redis: %v", iter.Val(), err)
			return err
		}
	}

	if err := iter.Err(); err != nil {
		log.Printf("Error during Redis key iteration: %v", err)
		return err
	}

	log.Println("All base definitions for currencies have been removed from Redis.")
	return nil
}

func (h *AdminHandler) AddOrUpdateCurrencyInCache(ctx context.Context, currency models.Currency) error {
	data, err := json.Marshal(currency)
	if err != nil {
		log.Printf("Error marshalling currency data for cache: %v", err)
		return err
	}

	cacheKey := fmt.Sprintf("base_definitions:currency:%s", currency.HK)

	if err := h.RedisClient.Client.Set(ctx, cacheKey, data, 24*time.Hour).Err(); err != nil {
		log.Printf("Error updating currency in cache: %v", err)
		return err
	}

	log.Printf("Currency with HK %s has been added/updated in cache.", currency.HK)
	return nil
}

func (h *AdminHandler) DeleteCurrencyFromCache(ctx context.Context, hk string) error {
	cacheKey := fmt.Sprintf("base_definitions:currency:%s", hk)

	if err := h.RedisClient.Client.Del(ctx, cacheKey).Err(); err != nil {
		log.Printf("Error deleting currency from cache: %v", err)
		return err
	}

	log.Printf("Currency with HK %s has been deleted from cache.", hk)
	return nil
}

func (h *AdminHandler) LoadAndCacheCurrencies(ctx context.Context) error {
	// Only public currencies are cached, as syncPublicView keeps it.
	rows, err := h.PostgresDB.DB.QueryContext(ctx, "SELECT id, hk, status, info FROM currencies WHERE status='approved' AND deleted_at IS NULL")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var currency models.Currency
		var infoData []byte

		if err := rows.Scan(&currency.ID, &currency.HK, &currency.Status, &infoData); err != nil {
			return err
		}

		if err := json.Unmarshal(infoData, &currency.Info); err != nil {
			return err
		}

		if err := h.AddOrUpdateCurrencyInCache(ctx, currency); err != nil {
			return err
		}
	}

	log.Println("All active currencies have been loaded and cached successfully.")
	return nil
}
