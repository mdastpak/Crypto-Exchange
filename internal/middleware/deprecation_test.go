package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestDeprecated_PointsToTheSuccessor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	since := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	router.GET("/public/currencies/:hk", Deprecated("/api/v1", since), func(c *gin.Context) {
		c.String(http.StatusOK, "served")
	})

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/public/currencies/a%20b", nil))
	assert.Equal(t, http.StatusOK, w.Code, "a deprecated route still answers")
	assert.Equal(t, "served", w.Body.String())
	assert.Equal(t, "@1793923200", w.Header().Get("Deprecation"))
	assert.Equal(t, `</api/v1/public/currencies/a%20b>; rel="successor-version"`, w.Header().Get("Link"))
}
