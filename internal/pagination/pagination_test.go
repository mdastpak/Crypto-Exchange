package pagination

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func request(t *testing.T, query string) (Request, map[string]string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/items?"+query, nil)
	return Parse(c)
}

// position is the sort key of the items in these tests: newest first, with
// the public identifier to break ties.
type position struct {
	CreatedAt time.Time `json:"t"`
	HK        string    `json:"hk"`
}

type item struct {
	HK        string
	CreatedAt time.Time
}

func positionOf(i item) interface{} { return position{CreatedAt: i.CreatedAt, HK: i.HK} }

func TestParse_Limits(t *testing.T) {
	r, errs := request(t, "")
	require.Nil(t, errs)
	assert.Equal(t, DefaultLimit, r.Limit)
	after, err := r.After(&position{})
	require.NoError(t, err)
	assert.False(t, after, "the first page")

	for _, limit := range []string{"1", "200"} {
		r, errs := request(t, "limit="+limit)
		require.Nil(t, errs, limit)
		assert.Equal(t, limit, strconv.Itoa(r.Limit))
	}
	for _, limit := range []string{"0", "201", "-5", "ten", "1.5", ""} {
		_, errs := request(t, "limit="+limit)
		assert.Equal(t, map[string]string{"limit": "must be a number from 1 to 200"}, errs, "limit=%q", limit)
	}
}

func TestCursor_RoundTrips(t *testing.T) {
	want := position{CreatedAt: time.Date(2026, 11, 7, 10, 0, 0, 123, time.UTC), HK: "019a5a5e-7c1b-7d2e-9f30-1a2b3c4d5e6f"}
	cursor, err := Cursor(want)
	require.NoError(t, err)
	assert.NotContains(t, cursor, "=", "URL-safe, without padding")

	r, errs := request(t, "limit=10&cursor="+cursor)
	require.Nil(t, errs)
	var got position
	after, err := r.After(&got)
	require.NoError(t, err)
	assert.True(t, after)
	assert.Equal(t, want, got)
}

func TestParse_RefusesCursorsItDidNotIssue(t *testing.T) {
	encode := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, cursor := range map[string]string{
		"not base64":          "not*base64!",
		"not JSON":            encode("not json"),
		"another version":     encode(`{"v": 2, "a": {"hk": "x"}}`),
		"no position":         encode(`{"v": 1}`),
		"padded base64":       base64.URLEncoding.EncodeToString([]byte(`{"v":1,"a":1}`)),
		"longer than allowed": strings.Repeat("a", maxCursorLength+1),
	} {
		_, errs := request(t, "cursor="+cursor)
		assert.Equal(t, map[string]string{"cursor": "is not a cursor of this list"}, errs, name)
	}

	_, errs := request(t, "limit=0&cursor=%25%25")
	assert.Len(t, errs, 2, "every invalid parameter is reported")
}

func TestRequest_AfterRefusesAPositionOfAnotherShape(t *testing.T) {
	cursor, err := Cursor("a string, not a position")
	require.NoError(t, err)
	r, errs := request(t, "cursor="+cursor)
	require.Nil(t, errs)
	_, err = r.After(&position{})
	assert.ErrorIs(t, err, ErrInvalidCursor)
}

func TestNewPage_KnowsTheNextPageFromTheExtraItem(t *testing.T) {
	base := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	items := []item{{HK: "c", CreatedAt: base.Add(3 * time.Hour)}, {HK: "b", CreatedAt: base.Add(2 * time.Hour)}, {HK: "a", CreatedAt: base}}
	r := Request{Limit: 2}

	page, err := NewPage(items, r, positionOf)
	require.NoError(t, err)
	assert.Equal(t, items[:2], page.Items, "the extra item is dropped")
	require.NotEmpty(t, page.NextCursor)
	next, errs := request(t, "cursor="+page.NextCursor)
	require.Nil(t, errs)
	var after position
	_, err = next.After(&after)
	require.NoError(t, err)
	assert.Equal(t, position{CreatedAt: items[1].CreatedAt, HK: "b"}, after, "the next page starts after the last item kept")

	page, err = NewPage(items[:2], r, positionOf)
	require.NoError(t, err)
	assert.Empty(t, page.NextCursor, "the last page has no cursor")

	empty, err := NewPage[item](nil, r, positionOf)
	require.NoError(t, err)
	data, err := json.Marshal(empty)
	require.NoError(t, err)
	assert.JSONEq(t, `{"items": []}`, string(data), "an empty page is an empty array, not null")
}
