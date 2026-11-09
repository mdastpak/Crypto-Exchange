package middleware

import (
	"net/http"

	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/gin-gonic/gin"
)

// RecoveryMiddleware turns a panic into a 500 response in the standard
// envelope. Gin logs the panic value and the stack trace on the server; the
// response never contains them, in any mode, because they reveal internal
// details to anyone who can trigger the panic.
func RecoveryMiddleware() gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered interface{}) {
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("INTERNAL_SERVER_ERROR"), nil)
	})
}
