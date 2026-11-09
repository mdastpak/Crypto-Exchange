package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/dberr"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requestIDOf(t *testing.T, sent string) (string, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(RequestID())
	var seen string
	engine.GET("/", func(c *gin.Context) { seen = c.GetString("request_id") })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if sent != "" {
		req.Header.Set(RequestIDHeader, sent)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return seen, w.Header().Get(RequestIDHeader)
}

func TestRequestID_KeepsAValidClientID(t *testing.T) {
	for _, id := range []string{"abc-123", "0192f5e4-7c1b-7a3e-9a51-3c2f6f1b7d10", "trace:span.1_2", strings.Repeat("a", 64)} {
		seen, returned := requestIDOf(t, id)
		assert.Equal(t, id, seen)
		assert.Equal(t, id, returned)
	}
}

func TestRequestID_ReplacesMissingAndInvalidIDs(t *testing.T) {
	for _, id := range []string{"", "two words", `quote"d`, "a=b", strings.Repeat("a", 65), "id\twith\ttabs"} {
		seen, returned := requestIDOf(t, id)
		assert.NotEqual(t, id, seen, "%q must be replaced", id)
		assert.Equal(t, seen, returned)
		parsed, err := uuid.Parse(seen)
		require.NoError(t, err, "a generated ID is a UUID")
		assert.Equal(t, uuid.Version(7), parsed.Version())
	}
}

func bodyLimited(t *testing.T, max int64, body io.Reader, contentLength int64) (*httptest.ResponseRecorder, string, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(BodyLimit(max))
	var received string
	called := false
	engine.POST("/", func(c *gin.Context) {
		called = true
		data, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		received = string(data)
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.ContentLength = contentLength
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w, received, called
}

func TestBodyLimit_PassesBodiesUpToTheLimit(t *testing.T) {
	w, received, called := bodyLimited(t, 8, strings.NewReader("12345678"), 8)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, called)
	assert.Equal(t, "12345678", received, "the handler reads the whole body")

	w, _, called = bodyLimited(t, 8, nil, 0)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, called, "a request without a body passes")
}

func TestBodyLimit_RejectsLargerBodiesWith413(t *testing.T) {
	cases := map[string]int64{
		"announced length": 9,
		// The client does not announce the length, as with chunked
		// encoding, so the limit is found by reading.
		"unknown length": -1,
	}
	for name, contentLength := range cases {
		w, _, called := bodyLimited(t, 8, strings.NewReader("123456789"), contentLength)
		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code, name)
		assert.False(t, called, "%s: the handler must not run", name)
		assert.Contains(t, w.Body.String(), "Request body too large", name)
	}
}

// TestRequestID_TracksDatabaseRefusals checks that every request carries
// the note of internal/dberr, so that a handler's 500 after a busy
// database becomes 503 (#230).
func TestRequestID_TracksDatabaseRefusals(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(RequestID())
	engine.GET("/", func(c *gin.Context) {
		dberr.Record(c.Request.Context(), &pgconn.PgError{Code: "40P01"})
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("INTERNAL_SERVER_ERROR"), nil)
	})
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), dberr.Code)
}
