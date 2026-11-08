package public

import (
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/platform"

	"github.com/gin-gonic/gin"
)

// @Summary Show the status of the exchange
// @Description Whether the exchange is open or under maintenance, the features that are locked now, and the maintenances and lockdowns that are announced, with their messages in Persian and English, for the apps to show a banner or a countdown (#235). It answers during a maintenance too, from the memory of the instance, so it never needs the database. Over the WebSocket, every connection receives the same status as a platform.status event when it changes.
// @Tags system
// @Produce json
// @Success 200 {object} models.APIResponse{info=platform.Status}
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/status [get]
func (h *PublicHandler) PlatformStatus(c *gin.Context) {
	status := platform.StatusOf(nil, time.Now())
	if h.Platform != nil && h.Platform.Watcher != nil {
		status = h.Platform.Watcher.Status()
	}
	helpers.SendResponse(c, http.StatusOK, "Status retrieved successfully.", status)
}
