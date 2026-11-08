package helpers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/dberr"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
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

// TestSendResponse_OnlyErrorsAbort checks that a 201, like a 200, lets the
// handlers after it run, and that every error response aborts them (#53).
func TestSendResponse_OnlyErrorsAbort(t *testing.T) {
	for status, aborts := range map[int]bool{
		http.StatusOK:                  false,
		http.StatusCreated:             false,
		http.StatusBadRequest:          true,
		http.StatusNotFound:            true,
		http.StatusInternalServerError: true,
	} {
		w, c := performSendResponse(status, "something happened", nil)

		assert.Equal(t, status, w.Code)
		assert.Equal(t, aborts, c.IsAborted(), "status %d", status)
		var resp models.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, status, resp.Status)
		assert.Equal(t, "something happened", resp.Message)
	}
}

func TestSendResponse_ErrorsCarryACodeAndTheRequestID(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Set("request_id", "req-42")
	SendResponse(c, http.StatusNotFound, "Currency not found", nil)

	var resp models.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "not_found", resp.Code, "the code of the status")
	assert.Equal(t, "req-42", resp.RequestID)

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	SendError(c, http.StatusConflict, "version_conflict", "Changed since", map[string]int{"version": 3})
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "version_conflict", resp.Code, "a specific code wins")
	assert.True(t, c.IsAborted())
}

func TestSendResponse_SuccessHasNoCode(t *testing.T) {
	w, _ := performSendResponse(http.StatusCreated, "created", nil)
	var raw map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	assert.NotContains(t, raw, "code")
	assert.NotContains(t, raw, "request_id")
}

func TestStatusCode_HasAFallback(t *testing.T) {
	assert.Equal(t, "rate_limited", StatusCode(http.StatusTooManyRequests))
	assert.Equal(t, "body_too_large", StatusCode(http.StatusRequestEntityTooLarge))
	assert.Equal(t, "error", StatusCode(http.StatusTeapot))
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

// TestSendResponse_AnswersABusyDatabaseWith503 sends a 500 for a request
// whose query PostgreSQL refused for a moment: the client gets 503
// database_busy with Retry-After, which it may retry (#230). A 500 of any
// other request stays a 500.
func TestSendResponse_AnswersABusyDatabaseWith503(t *testing.T) {
	send := func(busy error) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		ctx := dberr.Track(context.Background())
		dberr.Record(ctx, busy)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
		SendResponse(c, http.StatusInternalServerError, "Failed to update profile", map[string]string{"x": "y"})
		return w
	}

	w := send(&pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "1", w.Header().Get("Retry-After"))
	var resp models.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, dberr.Code, resp.Code)
	assert.Nil(t, resp.Info)

	w = send(&pgconn.PgError{Code: "23505"})
	assert.Equal(t, http.StatusInternalServerError, w.Code, "a unique violation is not a busy database")
	w = send(errors.New("plain error"))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
