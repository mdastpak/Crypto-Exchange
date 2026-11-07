package audit

import (
	"context"
	"database/sql"
	"encoding/base64"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/pagination"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pageRequest parses a page request from a query, as the handler does.
func pageRequest(t *testing.T, query string) pagination.Request {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/audit?"+query, nil)
	r, errs := pagination.Parse(c)
	require.Nil(t, errs)
	return r
}

// seed records events in the order given.
func seed(t *testing.T, db *sql.DB, events ...Event) {
	t.Helper()
	for _, e := range events {
		require.NoError(t, Record(context.Background(), db, e))
		// UUID v7 keys are ordered within a millisecond too, but a pause
		// keeps occurred_at apart for the time filters.
		time.Sleep(2 * time.Millisecond)
	}
}

func actions(page pagination.Page[Entry]) []string {
	out := []string{}
	for _, e := range page.Items {
		out = append(out, e.Action)
	}
	return out
}

func TestList_FiltersNewestFirst(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	admin, user, currency := uuid.NewString(), uuid.NewString(), uuid.NewString()
	seed(t, db,
		Event{ActorType: ActorAdmin, ActorID: admin, Action: "admin.sign_in", Outcome: Success, IP: "203.0.113.1"},
		Event{ActorType: ActorAdmin, ActorID: admin, Action: ActionCurrencyUpdate, Outcome: Success,
			TargetType: TargetCurrency, TargetID: currency,
			Changes: &Changes{Before: map[string]interface{}{"status": "pending"}, After: map[string]interface{}{"status": "approved"}}},
		Event{ActorType: ActorAnonymous, Action: "admin.sign_in", Outcome: Failure, Reason: "email_code_invalid"},
		Event{ActorType: ActorUser, ActorID: user, Action: "user.sign_in", Outcome: Success},
	)
	ctx := context.Background()
	list := func(f Filter) pagination.Page[Entry] {
		t.Helper()
		page, err := List(ctx, db, f, pageRequest(t, ""))
		require.NoError(t, err)
		return page
	}

	assert.Equal(t, []string{"user.sign_in", "admin.sign_in", ActionCurrencyUpdate, "admin.sign_in"}, actions(list(Filter{})), "newest first")
	assert.Equal(t, []string{ActionCurrencyUpdate, "admin.sign_in"}, actions(list(Filter{ActorID: admin})))
	assert.Equal(t, []string{"admin.sign_in", "admin.sign_in"}, actions(list(Filter{Action: "admin.sign_in"})))
	assert.Equal(t, []string{"admin.sign_in"}, actions(list(Filter{Action: "admin.sign_in", Outcome: Failure})), "filters combine")

	changed := list(Filter{TargetType: TargetCurrency, TargetID: currency})
	require.Len(t, changed.Items, 1)
	e := changed.Items[0]
	assert.Equal(t, admin, e.ActorID)
	assert.Equal(t, "approved", e.Changes.After["status"])
	_, err := time.Parse(time.RFC3339Nano, e.OccurredAt)
	assert.NoError(t, err)

	failed := list(Filter{Outcome: Failure}).Items
	require.Len(t, failed, 1)
	assert.Equal(t, ActorAnonymous, failed[0].ActorType)
	assert.Empty(t, failed[0].ActorID)
	assert.Equal(t, "email_code_invalid", failed[0].Reason)
	assert.Empty(t, list(Filter{ActorID: uuid.NewString()}).Items)
	assert.Equal(t, "203.0.113.1", list(Filter{ActorID: admin, Action: "admin.sign_in"}).Items[0].IP)
}

func TestList_PagesThroughEveryEventOnce(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	for i := 0; i < 5; i++ {
		seed(t, db, Event{ActorType: ActorSystem, Action: "test.page", Outcome: Success, TargetType: "test", TargetID: string(rune('a' + i))})
	}
	var seen []string
	query := "limit=2"
	for pages := 0; ; pages++ {
		require.Less(t, pages, 5, "the pages end")
		page, err := List(context.Background(), db, Filter{Action: "test.page"}, pageRequest(t, query))
		require.NoError(t, err)
		for _, e := range page.Items {
			seen = append(seen, e.TargetID)
		}
		if page.NextCursor == "" {
			break
		}
		query = "limit=2&cursor=" + page.NextCursor
	}
	assert.Equal(t, []string{"e", "d", "c", "b", "a"}, seen, "newest first, each event once")
}

func TestList_BoundsTheTime(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	seed(t, db, Event{ActorType: ActorSystem, Action: "test.early", Outcome: Success})
	var middle time.Time
	require.NoError(t, db.QueryRow("SELECT NOW()").Scan(&middle))
	time.Sleep(5 * time.Millisecond)
	seed(t, db, Event{ActorType: ActorSystem, Action: "test.late", Outcome: Success})

	page, err := List(context.Background(), db, Filter{Since: middle}, pageRequest(t, ""))
	require.NoError(t, err)
	assert.Equal(t, []string{"test.late"}, actions(page))
	page, err = List(context.Background(), db, Filter{Until: middle}, pageRequest(t, ""))
	require.NoError(t, err)
	assert.Equal(t, []string{"test.early"}, actions(page))
}

func TestList_RefusesACursorWithoutAUUID(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	cursor, err := pagination.Cursor(map[string]string{"hk": "1 OR 1=1"})
	require.NoError(t, err)
	_, err = List(context.Background(), db, Filter{}, pageRequest(t, "cursor="+cursor))
	assert.ErrorIs(t, err, pagination.ErrInvalidCursor)

	raw := base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"a":{"hk":"not-a-uuid"}}`))
	_, err = List(context.Background(), db, Filter{}, pageRequest(t, "cursor="+raw))
	assert.ErrorIs(t, err, pagination.ErrInvalidCursor)
}
