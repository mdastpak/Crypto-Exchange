package openapitest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDocument is a small document in the shape that swag generates.
const testDocument = `{
  "swagger": "2.0",
  "paths": {
    "/items/{id}": {
      "get": {"responses": {
        "200": {"schema": {"allOf": [{"$ref": "#/definitions/Envelope"}, {"type": "object", "properties": {"info": {"$ref": "#/definitions/Item"}}}]}},
        "404": {"schema": {"$ref": "#/definitions/Envelope"}}
      }}
    },
    "/items/special": {
      "get": {"responses": {"200": {"schema": {"type": "object", "properties": {"special": {"type": "boolean"}}}}}}
    },
    "/health": {
      "get": {"responses": {
        "200": {"schema": {"$ref": "#/definitions/Health"}},
        "default": {"schema": {"$ref": "#/definitions/Envelope"}}
      }}
    },
    "/ws": {"get": {"responses": {"101": {"description": "Switching Protocols"}}}}
  },
  "definitions": {
    "Envelope": {"type": "object", "properties": {
      "status": {"type": "integer"}, "message": {"type": "string"}, "code": {"type": "string"}, "info": {}
    }},
    "Item": {"type": "object", "properties": {
      "id": {"type": "integer"}, "name": {"type": "string"}, "tags": {"type": "array", "items": {"type": "string"}},
      "kind": {"type": "string", "enum": ["coin", "token"]}, "price": {"type": "number"}
    }},
    "Health": {"type": "object", "properties": {
      "status": {"type": "string"},
      "details": {"type": "object", "additionalProperties": {"type": "object", "properties": {"status": {"type": "string"}}}},
      "extra": {"type": "object", "additionalProperties": true},
      "closed": {"type": "object", "additionalProperties": false}
    }}
  }
}`

func load(t *testing.T) *Document {
	t.Helper()
	file := filepath.Join(t.TempDir(), "swagger.json")
	require.NoError(t, os.WriteFile(file, []byte(testDocument), 0o600))
	d, err := Load(file)
	require.NoError(t, err)
	return d
}

func check(d *Document, method, path string, status int, body string) (bool, []string) {
	return d.Check(method, path, status, "application/json", []byte(body))
}

func TestCheck_AcceptsWhatTheDocumentDescribes(t *testing.T) {
	d := load(t)
	for name, tc := range map[string]struct {
		path   string
		status int
		body   string
	}{
		"envelope with a typed info": {"/items/7", 200, `{"status": 200, "message": "ok", "info": {"id": 7, "name": "BTC", "tags": ["a"], "kind": "coin", "price": 1.5}}`},
		"null for any type":          {"/items/7", 200, `{"status": 200, "message": "ok", "info": {"id": 7, "name": null, "tags": null}}`},
		"plain envelope, any info":   {"/items/7", 404, `{"status": 404, "message": "no", "code": "not_found", "info": {"anything": [1, 2]}}`},
		"a map of objects":           {"/health", 200, `{"status": "ok", "details": {"redis": {"status": "ok"}, "postgres": {"status": "ok"}}}`},
		"a map of anything":          {"/health", 200, `{"extra": {"a": 1, "b": [true], "c": {"d": null}}}`},
		"the default response":       {"/health", 503, `{"status": 503, "message": "down", "code": "unavailable"}`},
		"a literal path first":       {"/items/special", 200, `{"special": true}`},
	} {
		t.Run(name, func(t *testing.T) {
			documented, problems := check(d, "GET", tc.path, tc.status, tc.body)
			assert.True(t, documented)
			assert.Empty(t, problems)
		})
	}

	documented, problems := d.Check("GET", "/ws", 101, "", nil)
	assert.True(t, documented)
	assert.Empty(t, problems, "a response without a schema has no body to check")
}

func TestCheck_ReportsWhatDiffers(t *testing.T) {
	d := load(t)
	for name, tc := range map[string]struct {
		path   string
		status int
		body   string
		want   string
	}{
		"an undocumented status":     {"/items/7", 500, `{}`, "status 500 is not documented"},
		"an undocumented property":   {"/items/7", 200, `{"status": 200, "message": "ok", "extra": 1}`, "body.extra: is not in the documented schema"},
		"a property of the info":     {"/items/7", 200, `{"info": {"id": 1, "secret": "x"}}`, "body.info.secret: is not in the documented schema"},
		"a string for an integer":    {"/items/7", 200, `{"info": {"id": "7"}}`, "body.info.id: is 7, not an integer"},
		"a fraction for an integer":  {"/items/7", 200, `{"info": {"id": 7.5}}`, "not an integer"},
		"an object for an array":     {"/items/7", 200, `{"info": {"tags": {}}}`, "body.info.tags: is map[string]interface {}, not an array"},
		"an item of the wrong type":  {"/items/7", 200, `{"info": {"tags": [1]}}`, "body.info.tags[0]: is json.Number, not a string"},
		"a value outside the enum":   {"/items/7", 200, `{"info": {"kind": "stock"}}`, "stock is not one of [coin token]"},
		"a map value of the wrong":   {"/health", 200, `{"details": {"redis": {"status": 1}}}`, "body.details.redis.status: is json.Number, not a string"},
		"a body that is not JSON":    {"/items/7", 200, `not json`, "the body is not JSON"},
		"a property of a closed map": {"/health", 200, `{"closed": {"a": 1}}`, "body.closed.a: is not in the documented schema"},
	} {
		t.Run(name, func(t *testing.T) {
			documented, problems := check(d, "GET", tc.path, tc.status, tc.body)
			assert.True(t, documented)
			require.NotEmpty(t, problems)
			assert.Contains(t, strings.Join(problems, "\n"), tc.want)
		})
	}

	_, problems := d.Check("GET", "/items/7", 200, "text/plain", []byte("ok"))
	assert.Contains(t, strings.Join(problems, "\n"), "not JSON")
}

func TestCheck_SkipsWhatTheDocumentDoesNotHave(t *testing.T) {
	d := load(t)
	for _, tc := range []struct{ method, path string }{{"GET", "/nothing"}, {"POST", "/items/7"}, {"GET", "/items/7/more"}} {
		documented, problems := check(d, tc.method, tc.path, 200, `{}`)
		assert.False(t, documented, "%s %s", tc.method, tc.path)
		assert.Empty(t, problems)
	}
}

func TestLoad_ReadsTheGeneratedDocument(t *testing.T) {
	d, err := Load(filepath.Join("..", "..", "..", "docs", "swagger.json"))
	require.NoError(t, err)
	documented, problems := d.Check("GET", "/system/live", 200, "application/json", []byte(`{"status": "ok"}`))
	assert.True(t, documented)
	assert.Empty(t, problems)
}
