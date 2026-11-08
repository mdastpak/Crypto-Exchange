// internal/helpers/response.go
package helpers

import (
	"net/http"
	"strconv"

	"github.com/mdastpak/crypto-exchange/internal/dberr"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/gin-gonic/gin"
)

// Error codes that more than one package answers with (#53). docs/api.md
// lists every code; a code, once published, does not change meaning.
const (
	CodeValidationFailed   = "validation_failed"
	CodeAccountUnavailable = "account_unavailable"
	CodeTokenInvalid       = "token_invalid"
)

// statusCodes are the codes of the error responses that have no more
// specific one.
var statusCodes = map[int]string{
	http.StatusBadRequest:            "invalid_request",
	http.StatusUnauthorized:          "unauthorized",
	http.StatusForbidden:             "forbidden",
	http.StatusNotFound:              "not_found",
	http.StatusMethodNotAllowed:      "method_not_allowed",
	http.StatusConflict:              "conflict",
	http.StatusRequestEntityTooLarge: "body_too_large",
	http.StatusTooManyRequests:       "rate_limited",
	http.StatusInternalServerError:   "internal_error",
	http.StatusNotImplemented:        "not_implemented",
	http.StatusServiceUnavailable:    "unavailable",
}

// StatusCode returns the code of an error response of status that has no
// more specific one.
func StatusCode(status int) string {
	if code, ok := statusCodes[status]; ok {
		return code
	}
	return "error"
}

// SendResponse sends the standard envelope. An error response, of status
// 400 or more, gets the code of its status and the request ID, and aborts
// the handlers after it; SendError gives it a more specific code.
func SendResponse(c *gin.Context, status int, message string, info interface{}) {
	send(c, status, "", message, info)
}

// SendError sends an error response with a code that tells clients what
// went wrong, where the code of the status does not.
func SendError(c *gin.Context, status int, code, message string, info interface{}) {
	send(c, status, code, message, info)
}

func send(c *gin.Context, status int, code, message string, info interface{}) {
	// A 500 of a request whose query PostgreSQL refused for a moment, such
	// as a lock wait past lock_timeout, is not a bug: the client may retry
	// (#230).
	if status == http.StatusInternalServerError && c.Request != nil {
		if answer, ok := dberr.UnavailableOf(c.Request.Context()); ok {
			// A dependency that the request needs was away, such as
			// RabbitMQ for a sign-in code (#232).
			status, code, info, message = http.StatusServiceUnavailable, answer.Code, nil, answer.Message
			c.Header("Retry-After", strconv.Itoa(answer.RetryAfterSeconds))
		} else if dberr.Busy(c.Request.Context()) {
			status, code, info = http.StatusServiceUnavailable, dberr.Code, nil
			message = "The service is busy. Please try again."
			c.Header("Retry-After", strconv.Itoa(dberr.RetryAfterSeconds))
		}
	}
	// Only a code that a caller chose names one kind of error; the code of
	// a status covers messages of different meanings, so it translates
	// none (#283).
	message = localize(c, status, code, message)
	response := models.APIResponse{Status: status, Message: message, Info: info}
	if status >= http.StatusBadRequest {
		if code == "" {
			code = StatusCode(status)
		}
		response.Code = code
		// The request ID middleware sets it on every request.
		response.RequestID = c.GetString("request_id")
	}
	c.Header("Content-Type", "application/json")
	c.JSON(status, response)
	if status >= http.StatusBadRequest {
		c.Abort()
	}
}

// localize returns message in the locale of the request (#283) and sets
// Content-Language to the language that the message is in. A message
// without a translation stays in English, and says so. An error answer
// varies with the headers that the locale is negotiated from, so that a
// cache does not serve one visitor's language to another; a translated
// answer of any status does too.
func localize(c *gin.Context, status int, code, message string) string {
	n, ok := i18n.FromGin(c)
	if !ok {
		return message
	}
	translated, ok := n.Translate(code, message)
	language := i18n.English
	if ok {
		message, language = translated, n.Locale.Code
	}
	c.Header("Content-Language", language)
	if ok || status >= http.StatusBadRequest {
		c.Writer.Header().Add("Vary", i18n.VaryHeaders)
	}
	return message
}
