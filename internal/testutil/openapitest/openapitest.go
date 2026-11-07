// Package openapitest checks HTTP responses against the OpenAPI document that
// swag generates into docs/ (#53). A response must have a status that its
// operation documents, or a default response, and its JSON body must match
// the documented schema, without properties that the schema does not have,
// so that the document cannot drift from what the server sends.
//
// It reads the subset of Swagger 2.0 that swag generates: $ref, allOf,
// properties, additionalProperties, items, enum, and the JSON types.
package openapitest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Schema is a schema of the document.
type Schema struct {
	Ref                  string             `json:"$ref"`
	Type                 string             `json:"type"`
	Properties           map[string]*Schema `json:"properties"`
	AdditionalProperties *Schema            `json:"additionalProperties"`
	Items                *Schema            `json:"items"`
	AllOf                []*Schema          `json:"allOf"`
	Enum                 []interface{}      `json:"enum"`
}

type response struct {
	Schema *Schema `json:"schema"`
}

type operation struct {
	Responses map[string]response `json:"responses"`
}

type path struct {
	template   string
	pattern    *regexp.Regexp
	operations map[string]operation
}

// Document is a loaded OpenAPI document.
type Document struct {
	paths       []path
	definitions map[string]*Schema
}

// Load reads the Swagger 2.0 document at file.
func Load(file string) (*Document, error) {
	data, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		return nil, err
	}
	var raw struct {
		Paths       map[string]map[string]operation `json:"paths"`
		Definitions map[string]*Schema              `json:"definitions"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	d := &Document{definitions: raw.Definitions}
	for template, operations := range raw.Paths {
		pattern := "^" + regexp.MustCompile(`\\\{[^/]+\\\}`).ReplaceAllString(regexp.QuoteMeta(template), `[^/]+`) + "$"
		methods := make(map[string]operation, len(operations))
		for method, op := range operations {
			methods[strings.ToUpper(method)] = op
		}
		d.paths = append(d.paths, path{template: template, pattern: regexp.MustCompile(pattern), operations: methods})
	}
	// Literal paths before templated ones, so that /a/b wins over /a/{x}.
	sort.Slice(d.paths, func(i, j int) bool {
		return strings.Count(d.paths[i].template, "{") < strings.Count(d.paths[j].template, "{")
	})
	return d, nil
}

// Check returns the problems of a response with the given status, content
// type, and body to method urlPath. documented is false when the document
// has no operation for it, and then nothing is checked.
func (d *Document) Check(method, urlPath string, status int, contentType string, body []byte) (documented bool, problems []string) {
	op, template, found := d.operation(method, urlPath)
	if !found {
		return false, nil
	}
	resp, ok := op.Responses[strconv.Itoa(status)]
	if !ok {
		if resp, ok = op.Responses["default"]; !ok {
			return true, []string{fmt.Sprintf("status %d is not documented for %s %s", status, method, template)}
		}
	}
	if resp.Schema == nil {
		return true, nil
	}
	if !strings.HasPrefix(contentType, "application/json") {
		return true, []string{fmt.Sprintf("%s %s %d: the content type is %q, not JSON", method, template, status, contentType)}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value interface{}
	if err := decoder.Decode(&value); err != nil {
		return true, []string{fmt.Sprintf("%s %s %d: the body is not JSON: %v", method, template, status, err)}
	}
	var c checker
	c.document = d
	c.check("body", value, resp.Schema)
	for i := range c.problems {
		c.problems[i] = fmt.Sprintf("%s %s %d: %s", method, template, status, c.problems[i])
	}
	return true, c.problems
}

func (d *Document) operation(method, urlPath string) (operation, string, bool) {
	for _, p := range d.paths {
		if p.pattern.MatchString(urlPath) {
			op, ok := p.operations[method]
			return op, p.template, ok
		}
	}
	return operation{}, "", false
}

type checker struct {
	document *Document
	problems []string
}

func (c *checker) fail(at, format string, args ...interface{}) {
	c.problems = append(c.problems, at+": "+fmt.Sprintf(format, args...))
}

// resolve follows $ref and merges allOf into one schema, whose later parts
// override the properties of earlier ones, as swag composes the envelope
// with its info.
func (c *checker) resolve(s *Schema) *Schema {
	if s == nil {
		return nil
	}
	if s.Ref != "" {
		name := strings.TrimPrefix(s.Ref, "#/definitions/")
		def, ok := c.document.definitions[name]
		if !ok {
			c.fail(s.Ref, "unknown definition")
			return &Schema{}
		}
		return c.resolve(def)
	}
	if len(s.AllOf) == 0 {
		return s
	}
	merged := &Schema{Type: s.Type, Properties: map[string]*Schema{}}
	for _, part := range s.AllOf {
		part = c.resolve(part)
		if part.Type != "" {
			merged.Type = part.Type
		}
		for name, prop := range part.Properties {
			merged.Properties[name] = prop
		}
		if part.AdditionalProperties != nil {
			merged.AdditionalProperties = part.AdditionalProperties
		}
	}
	return merged
}

func (c *checker) check(at string, value interface{}, s *Schema) {
	s = c.resolve(s)
	// Swagger 2.0 cannot say that a value may be null; Go writes null for
	// nil slices, maps, and pointers, so null passes.
	if s == nil || value == nil {
		return
	}
	if len(s.Enum) > 0 && !inEnum(value, s.Enum) {
		c.fail(at, "%v is not one of %v", value, s.Enum)
	}
	switch s.Type {
	case "":
		// An empty schema, such as the info of the plain envelope: anything.
		if len(s.Properties) > 0 {
			c.checkObject(at, value, s)
		}
	case "object":
		c.checkObject(at, value, s)
	case "array":
		items, ok := value.([]interface{})
		if !ok {
			c.fail(at, "is %T, not an array", value)
			return
		}
		for i, item := range items {
			c.check(fmt.Sprintf("%s[%d]", at, i), item, s.Items)
		}
	case "string":
		if _, ok := value.(string); !ok {
			c.fail(at, "is %T, not a string", value)
		}
	case "integer":
		n, ok := value.(json.Number)
		if _, err := n.Int64(); !ok || err != nil {
			c.fail(at, "is %v, not an integer", value)
		}
	case "number":
		if _, ok := value.(json.Number); !ok {
			c.fail(at, "is %T, not a number", value)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			c.fail(at, "is %T, not a boolean", value)
		}
	default:
		c.fail(at, "the schema has the unknown type %q", s.Type)
	}
}

func (c *checker) checkObject(at string, value interface{}, s *Schema) {
	object, ok := value.(map[string]interface{})
	if !ok {
		c.fail(at, "is %T, not an object", value)
		return
	}
	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if prop, ok := s.Properties[name]; ok {
			c.check(at+"."+name, object[name], prop)
		} else if s.AdditionalProperties != nil {
			c.check(at+"."+name, object[name], s.AdditionalProperties)
		} else {
			c.fail(at+"."+name, "is not in the documented schema")
		}
	}
}

func inEnum(value interface{}, enum []interface{}) bool {
	for _, allowed := range enum {
		if fmt.Sprint(allowed) == fmt.Sprint(value) {
			return true
		}
	}
	return false
}
