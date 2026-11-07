package main

import (
	"bufio"
	"bytes"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/testutil/openapitest"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// openAPIDocument is the generated OpenAPI document, loaded once.
var openAPIDocument = sync.OnceValues(func() (*openapitest.Document, error) {
	return openapitest.Load(filepath.Join("..", "..", "docs", "swagger.json"))
})

// contractWriter keeps a copy of the response body for the contract check.
type contractWriter struct {
	gin.ResponseWriter
	body     bytes.Buffer
	hijacked bool
}

func (w *contractWriter) Write(b []byte) (int, error) {
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w *contractWriter) WriteString(s string) (int, error) {
	w.body.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

func (w *contractWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	return w.ResponseWriter.Hijack()
}

// contractCheck checks every response of a test router against the OpenAPI
// document (#53) and reports what differs as an error of the test, so that
// every end-to-end test also tests the document. A deprecated alias is
// checked against the operation of its versioned path. A response without
// a documented operation, such as a 404 for an unknown path, is skipped,
// and so is an upgraded WebSocket connection.
func contractCheck(t *testing.T) gin.HandlerFunc {
	t.Helper()
	document, err := openAPIDocument()
	require.NoError(t, err)
	return func(c *gin.Context) {
		w := &contractWriter{ResponseWriter: c.Writer}
		c.Writer = w
		c.Next()
		if w.hijacked {
			return
		}
		method, path := c.Request.Method, c.Request.URL.Path
		contentType := w.Header().Get("Content-Type")
		documented, problems := document.Check(method, path, w.Status(), contentType, w.body.Bytes())
		if !documented && !strings.HasPrefix(path, helpers.APIPrefix+"/") {
			_, problems = document.Check(method, helpers.APIPrefix+path, w.Status(), contentType, w.body.Bytes())
		}
		for _, problem := range problems {
			t.Errorf("the response differs from the OpenAPI document: %s\nbody: %s", problem, w.body.String())
		}
	}
}

// routeForTest registers the routes of the server on router, as main does,
// behind the contract check.
func routeForTest(t *testing.T, router *gin.Engine, handlers *InfraHandlers) {
	t.Helper()
	router.Use(contractCheck(t))
	require.NoError(t, RouteHandler(router, handlers))
}
