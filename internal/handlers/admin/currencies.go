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
	"github.com/lib/pq"
)

type AdminHandler struct {
	PostgresDB      *db.PostgresService
	RedisClient     *db.RedisService
	RabbitMQService *rabbitmq.RabbitMQService

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
	if is.Redis != nil {
		settings := otp.SettingsFromConfig()
		settings.Namespace = otpNamespace
		h.OTP = otp.NewStore(is.Redis.Client, settings)
	}
	return h
}

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

	if err := h.RabbitMQService.Publish(ctx, config.Config.RabbitMQ.Exchanges.PriceUpdates, string(eventJSON)); err != nil {
		log.Printf("Failed to publish currency %s: %v", action, err)
	}
}

func (h *AdminHandler) CreateCurrency(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var currency models.Currency
	if err := c.ShouldBindJSON(&currency); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid input: Please check the provided data format.", nil)
		return
	}

	infoJSON, err := json.Marshal(currency.Info)
	if err != nil {
		log.Printf("Error marshalling CurrencyInfo to JSON: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Internal error: Failed to process currency information.", nil)
		return
	}

	var generatedHK string
	err = h.PostgresDB.DB.QueryRowContext(ctx,
		"INSERT INTO currencies (status, info) VALUES ($1, $2::jsonb) RETURNING hk",
		currency.Status, infoJSON,
	).Scan(&generatedHK)
	if err != nil {
		log.Printf("Error creating currency in database: %v", err)
		if pgError, ok := err.(*pq.Error); ok {
			switch pgError.Code.Name() {
			case "unique_violation":
				helpers.SendResponse(c, http.StatusBadRequest, "Currency with this symbol or contract already exists.", nil)
				return
			case "check_violation":
				helpers.SendResponse(c, http.StatusBadRequest, "Invalid currency structure: Missing required fields.", nil)
				return
			}
		}
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to create currency due to an unexpected error.", nil)
		return
	}

	currency.HK = generatedHK

	if err := h.AddOrUpdateCurrencyInCache(ctx, currency); err != nil {
		log.Printf("Error caching currency after database update: %v", err)
	}

	h.publishCurrencyEvent(ctx, "create", generatedHK, currency.Info)

	helpers.SendResponse(c, http.StatusCreated, "Currency created successfully.", nil)
}

func (h *AdminHandler) UpdateCurrency(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	hk := c.Param("hk")
	if _, err := uuid.Parse(hk); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid UUID format for HK", nil)
		return
	}

	var updatedData models.Currency
	if err := c.ShouldBindJSON(&updatedData); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid input: Please check the provided data format.", nil)
		return
	}

	var currentStatus string
	var infoData []byte
	err := h.PostgresDB.DB.QueryRowContext(ctx,
		"SELECT status, info FROM currencies WHERE hk = $1 AND deleted_at IS NULL", hk,
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

	if updatedData.Status != "" {
		currentStatus = updatedData.Status
	}
	if updatedData.Info.EnName != "" {
		currentInfo.EnName = updatedData.Info.EnName
	}
	if updatedData.Info.FaName != "" {
		currentInfo.FaName = updatedData.Info.FaName
	}
	if updatedData.Info.Symbol != "" {
		currentInfo.Symbol = updatedData.Info.Symbol
	}
	if updatedData.Info.Contract != "" {
		currentInfo.Contract = updatedData.Info.Contract
	}
	if updatedData.Info.IfFiat {
		currentInfo.IfFiat = updatedData.Info.IfFiat
	}
	if updatedData.Info.IsToken {
		currentInfo.IsToken = updatedData.Info.IsToken
	}

	infoJSON, err := json.Marshal(currentInfo)
	if err != nil {
		log.Printf("Error marshalling CurrencyInfo to JSON: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Internal error: Failed to process currency information.", nil)
		return
	}

	_, err = h.PostgresDB.DB.ExecContext(ctx,
		"UPDATE currencies SET status=$1, info=$2::jsonb, updated_at=NOW() WHERE hk=$3 AND deleted_at IS NULL",
		currentStatus, infoJSON, hk,
	)
	if err != nil {
		log.Printf("Error updating currency in database: %v", err)
		if pgError, ok := err.(*pq.Error); ok {
			switch pgError.Code.Name() {
			case "unique_violation":
				helpers.SendResponse(c, http.StatusBadRequest, "Currency with this symbol or contract already exists.", nil)
				return
			case "check_violation":
				helpers.SendResponse(c, http.StatusBadRequest, "Invalid currency structure: Missing required fields.", nil)
				return
			}
		}
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to update currency due to an unexpected error.", nil)
		return
	}

	updatedData.HK = hk
	updatedData.Info = currentInfo
	updatedData.Status = currentStatus

	if err := h.AddOrUpdateCurrencyInCache(ctx, updatedData); err != nil {
		log.Printf("Error caching currency after database update: %v", err)
	}

	h.publishCurrencyEvent(ctx, "update", hk, currentInfo)

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
	err := h.PostgresDB.DB.QueryRowContext(ctx,
		"SELECT (deleted_at IS NOT NULL) FROM currencies WHERE hk = $1", hk,
	).Scan(&isDeleted)
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

	if err := h.DeleteCurrencyFromCache(ctx, hk); err != nil {
		log.Printf("Error deleting currency from cache after database deletion: %v", err)
	}

	h.publishCurrencyEvent(ctx, "delete", hk, map[string]string{"status": "deleted"})

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
