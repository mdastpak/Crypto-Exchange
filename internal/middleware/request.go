package middleware

import (
	"bytes"
	"io"
	"net/http"
	"regexp"

	"encrypted-db/internal/helpers"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// RequestIDHeader carries the ID of a request in requests and responses.
const RequestIDHeader = "X-Request-ID"

// validRequestID matches the request IDs that clients may choose: up to 64
// letters, digits, and the punctuation of UUIDs and trace IDs. Anything else,
// such as spaces, quotes, or line breaks, could forge entries in the logs.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// RequestID gives every request an ID, stored as "request_id" in the context
// and returned in the X-Request-ID header. A valid ID from the client is kept,
// so that a request can be traced across services; any other is replaced by
// a new UUIDv7.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(RequestIDHeader)
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		c.Set("request_id", id)
		c.Header(RequestIDHeader, id)
		c.Next()
	}
}

func newRequestID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

// BodyLimit rejects request bodies larger than max bytes with 413. The body
// is read before the handler runs, so a handler never sees part of an
// oversized body, also when the client does not announce its length.
func BodyLimit(max int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body == nil || c.Request.Body == http.NoBody {
			c.Next()
			return
		}
		if c.Request.ContentLength > max {
			rejectLargeBody(c)
			return
		}
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, max+1))
		if err != nil {
			helpers.SendResponse(c, http.StatusBadRequest, "Invalid request body", nil)
			c.Abort()
			return
		}
		if int64(len(body)) > max {
			rejectLargeBody(c)
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Next()
	}
}

func rejectLargeBody(c *gin.Context) {
	// The rest of the body is not read, so the connection cannot be reused.
	c.Header("Connection", "close")
	helpers.SendResponse(c, http.StatusRequestEntityTooLarge, "Request body too large", nil)
	c.Abort()
}
