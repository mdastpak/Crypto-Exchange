package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/helpers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ginParam matches a path parameter of a Gin route, such as :hk.
var ginParam = regexp.MustCompile(`:([A-Za-z0-9_]+)`)

// documentedOperations returns the operations of the OpenAPI document that
// swag generates into docs/, as "METHOD path".
func documentedOperations(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "swagger.json"))
	require.NoError(t, err)
	var document struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(data, &document))
	var operations []string
	for path, methods := range document.Paths {
		for method := range methods {
			operations = append(operations, strings.ToUpper(method)+" "+path)
		}
	}
	sort.Strings(operations)
	return operations
}

// TestOpenAPI_DocumentsEveryRoute compares the routes of the server with the
// operations of the OpenAPI document (#54): each route is documented, and
// each documented operation exists. The deprecated unversioned aliases are
// described once, in the document's description, and the Swagger UI is not
// an operation.
func TestOpenAPI_DocumentsEveryRoute(t *testing.T) {
	env := newE2EEnv(t)
	var routes []string
	for _, route := range newTestRouter(t, env).Routes() {
		path := route.Path
		if strings.HasPrefix(path, "/swagger/") {
			continue
		}
		if !strings.HasPrefix(path, helpers.APIPrefix+"/") && !unversioned(path) {
			continue // an alias
		}
		routes = append(routes, route.Method+" "+ginParam.ReplaceAllString(path, "{$1}"))
	}
	sort.Strings(routes)
	assert.Equal(t, routes, documentedOperations(t),
		"regenerate the document with: go run github.com/swaggo/swag/cmd/swag@v1.16.6 init -g cmd/server/main.go -o docs --parseInternal")
}
