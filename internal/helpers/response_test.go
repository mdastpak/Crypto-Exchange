package helpers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func performSendResponse(status int, message string, info interface{}) (*httptest.ResponseRecorder, *gin.Context) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	SendResponse(c, status, message, info)
	return w, c
}

func TestSendResponse_SuccessStatusDoesNotAbort(t *testing.T) {
	w, c := performSendResponse(http.StatusOK, "all good", map[string]string{"foo": "bar"})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, c.IsAborted(), "a 200 response should not abort the gin context")
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	var resp models.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "all good", resp.Message)
	assert.NotNil(t, resp.Info)
}

func TestSendResponse_NonSuccessStatusAborts(t *testing.T) {
	tests := []int{
		http.StatusBadRequest,
		http.StatusNotFound,
		http.StatusInternalServerError,
		http.StatusCreated,
	}

	for _, status := range tests {
		w, c := performSendResponse(status, "something happened", nil)

		assert.Equal(t, status, w.Code)
		if status == http.StatusOK {
			assert.False(t, c.IsAborted())
		} else {
			assert.True(t, c.IsAborted(), "status %d should abort the gin context", status)
		}

		var resp models.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, status, resp.Status)
		assert.Equal(t, "something happened", resp.Message)
	}
}

func TestSendResponse_NilInfoOmittedFromJSON(t *testing.T) {
	w, _ := performSendResponse(http.StatusBadRequest, "bad input", nil)

	var raw map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	_, hasInfo := raw["info"]
	assert.False(t, hasInfo, "info field should be omitted when nil due to omitempty")
}

func TestSendResponse_WithInfoIncludedInJSON(t *testing.T) {
	w, _ := performSendResponse(http.StatusOK, "ok", []string{"a", "b"})

	var raw map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	require.Contains(t, raw, "info")
	assert.Equal(t, []interface{}{"a", "b"}, raw["info"])
}
