package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// records decodes the JSON records written to out.
func records(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	var list []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var r map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &r), line)
		list = append(list, r)
	}
	return list
}

// TestRedaction_HidesSecretsAndContacts writes secrets, tokens, codes, and
// contact details in every place a record carries text: the message of a
// line of the standard log package, the message and attributes of an slog
// record, an error, and a group. None of them reaches the output (#50).
func TestRedaction_HidesSecretsAndContacts(t *testing.T) {
	out := &bytes.Buffer{}
	logger, err := New(Options{Output: out})
	require.NoError(t, err)
	t.Cleanup(Install(logger))

	const jwt = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiIxMjMifQ.c2lnbmF0dXJl"
	secrets := []string{
		"sara@example.com", "09121234567", "+989121234567", jwt, "hunter2", "Sup3rS3cret", "482913", "1234567890",
	}
	log.Printf("Failed to send the code to sara@example.com and 09121234567, password=hunter2")
	log.Printf("Dialing amqp://guest:Sup3rS3cret@rabbit:5672/ with Authorization: Bearer %s", jwt)
	logger.Info("sign-in for +989121234567",
		slog.String("otp", "482913"),
		slog.String("contact", "sara@example.com"),
		slog.Any("error", errors.New("token="+jwt)),
		slog.Group("user", slog.String("national_id", "1234567890"), slog.String("email", "sara@example.com")))

	text := out.String()
	for _, secret := range secrets {
		assert.NotContains(t, text, secret, "a secret reached the log")
	}
	for _, mark := range []string{"[redacted-email]", "[redacted-phone]", "[redacted-token]", "[redacted]"} {
		assert.Contains(t, text, mark)
	}
	list := records(t, out)
	require.Len(t, list, 3, "every line is one JSON record")
	assert.Equal(t, "ERROR", list[0]["level"], "a line that starts with Failed is an error")
	assert.Equal(t, "INFO", list[1]["level"])
	assert.Equal(t, "[redacted]", list[2]["otp"])
}

// TestBridge_GivesTheLinesOfTheStandardLogTheirLevels checks the levels of
// the prefixes that the code base uses.
func TestBridge_GivesTheLinesOfTheStandardLogTheirLevels(t *testing.T) {
	out := &bytes.Buffer{}
	logger, err := New(Options{Output: out, Level: "debug"})
	require.NoError(t, err)
	t.Cleanup(Install(logger))

	log.Println("Server started")
	log.Println("WARNING: keys.provider is not set")
	log.Println("SECURITY: users 1 fails its integrity check (mismatch)")
	log.Println("Error closing the connection")

	list := records(t, out)
	require.Len(t, list, 4)
	assert.Equal(t, []any{"INFO", "WARN", "WARN", "ERROR"}, []any{list[0]["level"], list[1]["level"], list[2]["level"], list[3]["level"]})
	assert.Equal(t, true, list[2]["security"], "a security event is marked")
	assert.NotEmpty(t, list[0]["time"])
}

// TestLevel_DropsTheRecordsBelowIt checks observability.logging.level.
func TestLevel_DropsTheRecordsBelowIt(t *testing.T) {
	out := &bytes.Buffer{}
	logger, err := New(Options{Output: out, Level: "warn", Format: "text"})
	require.NoError(t, err)
	logger.Info("hidden")
	logger.Warn("shown")
	assert.NotContains(t, out.String(), "hidden")
	assert.Contains(t, out.String(), "level=WARN msg=shown")

	_, err = New(Options{Level: "loud"})
	assert.Error(t, err)
	_, err = New(Options{Format: "xml"})
	assert.Error(t, err)
}

// TestRequests_LogsTheRouteNotThePath checks the request log: the pattern of
// the route, never a value of the path or the query, with the request ID
// and the user.
func TestRequests_LogsTheRouteNotThePath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	out := &bytes.Buffer{}
	logger, err := New(Options{Output: out})
	require.NoError(t, err)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("request_id", "req-1") })
	r.Use(Requests(logger, func(*gin.Context) string { return "user-7" }))
	r.GET("/contacts/:id", func(c *gin.Context) { c.Status(http.StatusInternalServerError) })

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/contacts/sara@example.com?token=abc", nil))

	list := records(t, out)
	require.Len(t, list, 1)
	rec := list[0]
	assert.Equal(t, "/contacts/:id", rec["route"])
	assert.Equal(t, "ERROR", rec["level"], "a server error")
	assert.EqualValues(t, 500, rec["status"])
	assert.Equal(t, "req-1", rec["request_id"])
	assert.Equal(t, "user-7", rec["user_id"])
	assert.NotContains(t, out.String(), "sara@example.com")
	assert.NotContains(t, out.String(), "abc")
}
