// internal/helpers/response.go
package helpers

import (
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/gin-gonic/gin"
)

// SendResponse is a helper function to send standardized JSON responses
func SendResponse(c *gin.Context, status int, message string, info interface{}) {
	c.Header("Content-Type", "application/json")
	c.JSON(status, models.APIResponse{
		Status:  status,
		Message: message,
		Info:    info,
	})
	if status != 200 {
		c.Abort()
	}
}
