package helpers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/dberr"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func performSendResponse(status int, message i18n.Message, info interface{}) (*httptest.ResponseRecorder, *gin.Context) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	SendResponse(c, status, message, info)
	return w, c
}

func TestSendResponse_SuccessStatusDoesNotAbort(t *testing.T) {
	w, c := performSendResponse(http.StatusOK, i18n.M("PROFILE_RETRIEVED_SUCCESSFULLY"), map[string]string{"foo": "bar"})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, c.IsAborted(), "a 200 response should not abort the gin context")
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	var resp models.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "Profile retrieved successfully.", resp.Message)
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
		w, c := performSendResponse(status, i18n.M("NOT_FOUND"), nil)

		assert.Equal(t, status, w.Code)
		assert.Equal(t, aborts, c.IsAborted(), "status %d", status)
		var resp models.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, status, resp.Status)
		assert.Equal(t, "Not found", resp.Message)
	}
}

func TestSendResponse_ErrorsCarryACodeAndTheRequestID(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Set("request_id", "req-42")
	SendResponse(c, http.StatusNotFound, i18n.M("CURRENCY_NOT_FOUND"), nil)

	var resp models.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "not_found", resp.Code, "the code of the status")
	assert.Equal(t, "req-42", resp.RequestID)

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	SendError(c, http.StatusConflict, "version_conflict", i18n.M("CURRENCY_VERSION_CONFLICT"), map[string]int{"version": 3})
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "version_conflict", resp.Code, "a specific code wins")
	assert.True(t, c.IsAborted())
}

func TestSendResponse_SuccessHasNoCode(t *testing.T) {
	w, _ := performSendResponse(http.StatusCreated, i18n.M("CURRENCY_CREATED_SUCCESSFULLY"), nil)
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
	w, _ := performSendResponse(http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)

	var raw map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	_, hasInfo := raw["info"]
	assert.False(t, hasInfo, "info field should be omitted when nil due to omitempty")
}

func TestSendResponse_WithInfoIncludedInJSON(t *testing.T) {
	w, _ := performSendResponse(http.StatusOK, i18n.M("FAVORITES_UPDATED"), []string{"a", "b"})

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
		SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_UPDATE_PROFILE"), map[string]string{"x": "y"})
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

// TestSendResponse_AnswersAnUnavailableDependencyWith503 sends a 500 for a
// request that a dependency it needs could not serve, such as RabbitMQ for a
// sign-in code (#232): the client gets 503 with the dependency's code,
// message, and Retry-After. It wins over a busy database, whose answer would
// say less.
func TestSendResponse_AnswersAnUnavailableDependencyWith503(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ctx := dberr.Track(context.Background())
	dberr.MarkBusy(ctx)
	dberr.MarkUnavailable(ctx, dberr.DeliveryUnavailable)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
	SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_SEND_OTP"), map[string]string{"x": "y"})

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "5", w.Header().Get("Retry-After"))
	var resp models.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, dberr.CodeDeliveryUnavailable, resp.Code)
	assert.Equal(t, dberr.DeliveryUnavailable.Message.String(), resp.Message)
	assert.Nil(t, resp.Info)

	// Only a 500 is turned into the answer.
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
	SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	_, ok := dberr.UnavailableOf(context.Background())
	assert.False(t, ok, "an untracked request has no note")
}

// TestSendResponse_TranslatesIntoTheLocaleOfTheRequest checks the
// translation of the envelope (#283, #290): the message by its key, with
// its parameters, and the headers that tell caches and clients which
// language they got.
func TestSendResponse_TranslatesIntoTheLocaleOfTheRequest(t *testing.T) {
	registry := i18n.MustNew("en", "en", "fa", "ar")
	send := func(lang string, fn func(c *gin.Context)) *httptest.ResponseRecorder {
		engine := gin.New()
		engine.Use(i18n.Middleware(registry))
		engine.GET("/", fn)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/?lang="+lang, nil))
		return w
	}
	message := func(w *httptest.ResponseRecorder) models.APIResponse {
		var body models.APIResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		return body
	}

	w := send("fa", func(c *gin.Context) { SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_REQUEST_BODY"), nil) })
	body := message(w)
	assert.Equal(t, "بدنه درخواست نامعتبر است.", body.Message)
	assert.Equal(t, "invalid_request", body.Code, "the code never changes")
	assert.Equal(t, "fa", w.Header().Get("Content-Language"))
	assert.Contains(t, w.Header().Values("Vary"), i18n.VaryHeaders)

	w = send("fa", func(c *gin.Context) {
		SendError(c, http.StatusForbidden, "currency_operation_disabled",
			i18n.M("CURRENCY_OPERATION_UNAVAILABLE", i18n.A("operation", i18n.Key("CAPABILITY_SELL")), i18n.A("currency", "ETH")), nil)
	})
	assert.Equal(t, "فروش ارز \u2068ETH\u2069 در حال حاضر در دسترس نیست.", message(w).Message)

	// A locale without a catalog answers in English, and says so; an
	// error answer varies with the locale.
	w = send("ar", func(c *gin.Context) { SendResponse(c, http.StatusNotFound, i18n.M("CURRENCY_NOT_FOUND"), nil) })
	assert.Equal(t, "Currency not found", message(w).Message)
	assert.Equal(t, "en", w.Header().Get("Content-Language"))
	assert.Contains(t, w.Header().Values("Vary"), i18n.VaryHeaders)

	// A success in English does not vary, so caches may share it.
	w = send("ar", func(c *gin.Context) { SendResponse(c, http.StatusOK, i18n.M("PROFILE_RETRIEVED_SUCCESSFULLY"), nil) })
	assert.Equal(t, "en", w.Header().Get("Content-Language"))
	assert.Empty(t, w.Header().Values("Vary"))
	w = send("fa", func(c *gin.Context) { SendResponse(c, http.StatusOK, i18n.M("PROFILE_RETRIEVED_SUCCESSFULLY"), nil) })
	assert.Equal(t, "پروفایل دریافت شد.", message(w).Message)
	assert.Contains(t, w.Header().Values("Vary"), i18n.VaryHeaders)

	w = send("en", func(c *gin.Context) { SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_REQUEST_BODY"), nil) })
	assert.Equal(t, "Invalid request body", message(w).Message)
	assert.Equal(t, "en", w.Header().Get("Content-Language"))
}

// TestSendResponse_WithoutTheMiddlewareStaysEnglish keeps the envelope of
// the routes and tests that run without the locale middleware unchanged.
func TestSendResponse_WithoutTheMiddlewareStaysEnglish(t *testing.T) {
	w, _ := performSendResponse(http.StatusBadRequest, i18n.M("INVALID_REQUEST_BODY"), nil)
	assert.Contains(t, w.Body.String(), "Invalid request body")
	assert.Empty(t, w.Header().Get("Content-Language"))
}
