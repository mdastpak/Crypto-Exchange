package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecoveryMiddleware_RevealsNothingAboutThePanic(t *testing.T) {
	const secret = "postgres://admin:hunter2@db.internal:5432/exchange"
	panics := map[string]interface{}{
		"string": "connect to " + secret,
		"error":  errors.New("connect to " + secret),
	}
	for _, mode := range []string{"debug", "release"} {
		for name, value := range panics {
			t.Run(mode+" "+name, func(t *testing.T) {
				saved := config.Config.Server.Mode
				t.Cleanup(func() { config.Config.Server.Mode = saved })
				config.Config.Server.Mode = mode
				gin.SetMode(gin.TestMode)

				// Gin writes the panic and its stack trace to the server log.
				var serverLog bytes.Buffer
				previous := gin.DefaultErrorWriter
				gin.DefaultErrorWriter = &serverLog
				t.Cleanup(func() { gin.DefaultErrorWriter = previous })

				router := gin.New()
				router.Use(RecoveryMiddleware())
				router.GET("/boom", func(c *gin.Context) { panic(value) })

				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/boom", nil))

				assert.Equal(t, http.StatusInternalServerError, w.Code)
				var body map[string]interface{}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
				assert.Equal(t, map[string]interface{}{
					"status":  float64(http.StatusInternalServerError),
					"message": "Internal server error",
				}, body, "the response holds only the standard envelope")
				assert.NotContains(t, w.Body.String(), "hunter2")
				assert.NotContains(t, w.Body.String(), "goroutine")
				assert.Contains(t, serverLog.String(), "panic recovered", "the panic must still reach the server log")
			})
		}
	}
}
