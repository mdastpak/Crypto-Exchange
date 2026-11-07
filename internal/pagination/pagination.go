// Package pagination is the convention of the list endpoints that can grow
// (#53): keyset pagination with an opaque cursor.
//
// A client asks for a page with the query parameters limit and cursor, and
// gets {"items": [...], "next_cursor": "..."} in the info of the envelope;
// the last page has no next_cursor. The cursor holds the sort key of the
// last item of the page, so the next page starts right after it:
//
//	SELECT ... WHERE (created_at, hk) < ($1, $2)
//	ORDER BY created_at DESC, hk DESC LIMIT <limit + 1>
//
// Unlike an offset, a cursor keeps the pages stable while rows are added,
// every page costs one range scan of the index on the sort key however deep
// it is, and no state is kept on the server, so any instance can serve the
// next page. Fetching limit + 1 rows tells whether there is a next page
// without counting.
//
// A cursor is opaque by convention only: it is base64 of JSON, which anyone
// can decode. The sort key must therefore hold only what the client may
// see. Break ties with a public identifier, such as a UUID, never with an
// internal serial id, which would tell how many rows a table has.
package pagination

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/gin-gonic/gin"
)

const (
	// DefaultLimit is the page size when the request sets none.
	DefaultLimit = 50
	// MaxLimit is the largest page size a request may ask for.
	MaxLimit = 200
	// maxCursorLength bounds the cursor a client sends.
	maxCursorLength = 512
	// cursorVersion is the version of the cursor format: a cursor of
	// another version is refused instead of being misread.
	cursorVersion = 1
)

// ErrInvalidCursor is returned for a cursor that this list did not issue.
var ErrInvalidCursor = errors.New("not a cursor of this list")

// Request is a page request.
type Request struct {
	// Limit is the number of items of the page.
	Limit int
	after json.RawMessage
}

// Page is the info of a page of a list.
type Page[T any] struct {
	Items []T `json:"items"`
	// NextCursor asks for the next page. It is left out on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

type cursor struct {
	Version int             `json:"v"`
	After   json.RawMessage `json:"a"`
}

// Parse reads the page request from the query of c. Its field errors name
// the parameter and what is wrong with it, for a 400 validation_failed.
func Parse(c *gin.Context) (Request, map[string]string) {
	r := Request{Limit: DefaultLimit}
	errs := map[string]string{}
	if raw, ok := c.GetQuery("limit"); ok {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > MaxLimit {
			errs["limit"] = fmt.Sprintf("must be a number from 1 to %d", MaxLimit)
		} else {
			r.Limit = limit
		}
	}
	if raw := c.Query("cursor"); raw != "" {
		after, err := decode(raw)
		if err != nil {
			errs["cursor"] = "is not a cursor of this list"
		} else {
			r.after = after
		}
	}
	if len(errs) > 0 {
		return Request{}, errs
	}
	return r, nil
}

// After decodes the position of the request, the sort key of the last item
// of the previous page, into position. It reports false for the first page.
func (r Request) After(position interface{}) (bool, error) {
	if r.after == nil {
		return false, nil
	}
	if err := json.Unmarshal(r.after, position); err != nil {
		return false, ErrInvalidCursor
	}
	return true, nil
}

// Cursor returns the cursor of the page that starts after position.
func Cursor(position interface{}) (string, error) {
	after, err := json.Marshal(position)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(cursor{Version: cursorVersion, After: after})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// NewPage builds the page of r from items, which the caller fetched with a
// limit of r.Limit + 1. When there are more than r.Limit, the extra one is
// dropped and the position of the last item kept becomes the next cursor.
func NewPage[T any](items []T, r Request, position func(T) interface{}) (Page[T], error) {
	page := Page[T]{Items: items}
	if page.Items == nil {
		page.Items = []T{}
	}
	if len(items) <= r.Limit {
		return page, nil
	}
	page.Items = items[:r.Limit]
	next, err := Cursor(position(page.Items[r.Limit-1]))
	if err != nil {
		return Page[T]{}, err
	}
	page.NextCursor = next
	return page, nil
}

func decode(raw string) (json.RawMessage, error) {
	if len(raw) > maxCursorLength {
		return nil, ErrInvalidCursor
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	var c cursor
	if err := json.Unmarshal(data, &c); err != nil || c.Version != cursorVersion || len(c.After) == 0 {
		return nil, ErrInvalidCursor
	}
	return c.After, nil
}
