package user

import (
	"context"
	"encoding/json"
	"encrypted-db/internal/helpers"
	"encrypted-db/internal/models"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

// UpdateProfile is a placeholder endpoint for future profile-update functionality.
// It returns 501 Not Implemented rather than leaving the route unregistered or
// wired to a nil handler (which would panic the server on first request).
func (h *UserHandler) UpdateProfile(c *gin.Context) {
	helpers.SendResponse(c, http.StatusNotImplemented, "Profile update is not implemented yet.", nil)
}

func (h *UserHandler) GetActiveCurrencies(c *gin.Context) {
	// Define the pattern to match all active currency keys
	pattern := "base_definitions:currency:*"

	// Use Redis SCAN command to retrieve all matching keys
	currencies := make([]models.Currency, 0)
	iter := h.RedisClient.Client.Scan(context.Background(), 0, pattern, 0).Iterator()
	for iter.Next(context.Background()) {
		// Get the value of each currency key from Redis
		data, err := h.RedisClient.Client.Get(context.Background(), iter.Val()).Result()
		if err != nil {
			log.Printf("Error retrieving currency from cache for key %s: %v\n", iter.Val(), err)
			helpers.SendResponse(c, http.StatusInternalServerError, "Failed to retrieve currencies from cache.", nil)
			return
		}

		// Unmarshal the JSON data into a Currency struct
		var currency models.Currency
		if err := json.Unmarshal([]byte(data), &currency); err != nil {
			log.Printf("Error unmarshalling currency data for key %s: %v\n", iter.Val(), err)
			helpers.SendResponse(c, http.StatusInternalServerError, "Failed to process currency information.", nil)
			return
		}

		// The cache should only hold public currencies; filtering here too
		// keeps an outdated entry from leaking.
		if !currency.IsPublic() {
			continue
		}
		currencies = append(currencies, currency)
	}

	// Check for any error during key iteration
	if err := iter.Err(); err != nil {
		log.Printf("Error during Redis key iteration: %v\n", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to retrieve currencies from cache.", nil)
		return
	}

	// Send the list of active currencies as a response
	helpers.SendResponse(c, http.StatusOK, "Active currencies retrieved successfully.", currencies)
}
