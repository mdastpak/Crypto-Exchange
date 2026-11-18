package sanctions

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/aml"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"
)

func migrated(t *testing.T) *sql.DB {
	t.Helper()
	db := pgtest.Migrated(t, "testdb_sanctions")
	sealtest.Use(t, db, rowseal.ModeEnforce)
	return db
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(query, args...).Scan(&n))
	return n
}

// ofacServer serves the fixture, and counts the downloads.
func ofacServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads++
		switch r.URL.Path {
		case "/sdn.csv":
			_, _ = w.Write([]byte(sdnCSV))
		case "/alt.csv":
			_, _ = w.Write([]byte(altCSV))
		}
	}))
	t.Cleanup(server.Close)
	return server, &downloads
}

// A download keeps a version only when the list changed, records that it
// is current otherwise, and downloads nothing before it is due (#73).
func TestUpdate_StoresAVersionPerChange(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)
	server, downloads := ofacServer(t)

	v, stored, err := Update(ctx, db, server.Client(), server.URL+"/sdn.csv", time.Hour)
	require.NoError(t, err)
	assert.True(t, stored)
	assert.Equal(t, 3, v.EntryCount)
	assert.Equal(t, 2, *downloads, "sdn.csv and alt.csv")

	again, stored, err := Update(ctx, db, server.Client(), server.URL+"/sdn.csv", time.Hour)
	require.NoError(t, err)
	assert.False(t, stored)
	assert.Equal(t, v.ID, again.ID)
	assert.Equal(t, 2, *downloads, "not due")

	_, err = db.Exec(`UPDATE sanctions_lists SET checked_at = NOW() - INTERVAL '2 hours'`)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, db, rowseal.SanctionsLists, "id = $1", v.ID))
	again, stored, err = Update(ctx, db, server.Client(), server.URL+"/sdn.csv", time.Hour)
	require.NoError(t, err)
	assert.False(t, stored, "the same list")
	assert.Equal(t, v.ID, again.ID)
	assert.True(t, again.CheckedAt.After(v.CheckedAt.Add(-time.Minute)), "found current again")
	assert.Equal(t, 1, count(t, db, `SELECT COUNT(*) FROM sanctions_lists`))

	list, err := Load(ctx, db, again)
	require.NoError(t, err)
	assert.Len(t, list.MatchAddress("TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8"), 1)
}

// A list changed in SQL, such as an entry removed from its body, fails its
// seal with its finding, and is not loaded (#73).
func TestLoad_ListChangedInSQL_Refused(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)
	entries, err := ParseOFAC([]byte(sdnCSV), []byte(altCSV))
	require.NoError(t, err)
	v, _, err := Save(ctx, db, ProviderOFAC, []byte("source"), entries)
	require.NoError(t, err)

	smaller, err := encode(entries[:1])
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE sanctions_lists SET body = $1`, smaller)
	require.NoError(t, err)
	_, err = Load(ctx, db, v)
	require.ErrorIs(t, err, rowseal.ErrUnverified)
	assert.Equal(t, 1, count(t, db, `SELECT COUNT(*) FROM integrity_findings WHERE table_name = 'sanctions_lists'`))

	s := &Screener{DB: db, Provider: ProviderOFAC, MaxAge: time.Hour}
	require.ErrorIs(t, s.Refresh(ctx), rowseal.ErrUnverified)
	action, data, err := s.Screen(ctx, db, addressRule(aml.ActionBlock), aml.Event{CounterpartyAddress: "TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8"})
	require.NoError(t, err)
	assert.Equal(t, aml.ActionReview, action, "no list: held for review")
	assert.Equal(t, reasonListUnavailable, data["reason"])
}

func addressRule(action aml.Action) aml.Stored {
	return aml.Stored{Rule: aml.Rule{Code: "sanctioned_address", Type: aml.TypeSanctions, Action: action,
		Config: aml.RuleConfig{Providers: []string{"ofac"}, Screen: "counterparty"}}}
}

func userRule(action aml.Action) aml.Stored {
	r := addressRule(action)
	r.Code, r.Config.Screen = "sanctioned_user", "user"
	return r
}

func TestScreener(t *testing.T) {
	ctx := context.Background()
	db := migrated(t)
	entries, err := ParseOFAC([]byte(sdnCSV), []byte(altCSV))
	require.NoError(t, err)
	_, _, err = Save(ctx, db, ProviderOFAC, []byte("source"), entries)
	require.NoError(t, err)
	now := time.Now()
	s := &Screener{DB: db, Provider: ProviderOFAC, MaxAge: 72 * time.Hour, Now: func() time.Time { return now }}
	require.NoError(t, s.Refresh(ctx))

	assert.True(t, s.Screens(addressRule(aml.ActionBlock).Rule))
	other := addressRule(aml.ActionBlock).Rule
	other.Config.Providers = []string{"chainalysis"}
	assert.False(t, s.Screens(other), "another provider's rule")

	action, data, err := s.Screen(ctx, db, addressRule(aml.ActionBlock), aml.Event{CounterpartyAddress: "TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8"})
	require.NoError(t, err)
	assert.Equal(t, aml.ActionBlock, action, "an address takes the rule's action")
	assert.Equal(t, "address", data["matched"])
	action, _, err = s.Screen(ctx, db, addressRule(aml.ActionBlock), aml.Event{CounterpartyAddress: "TQn9Y2khEsLJW1ChVWFMSMeRDow5KcbLSE"})
	require.NoError(t, err)
	assert.Equal(t, aml.ActionNone, action)
	action, _, err = s.Screen(ctx, db, addressRule(aml.ActionBlock), aml.Event{})
	require.NoError(t, err)
	assert.Equal(t, aml.ActionNone, action, "a fiat destination has no address")

	ali := usertest.Insert(t, db, usertest.User{Email: "ali@example.com"})
	_, err = db.Exec(`UPDATE users SET first_name = 'Ali', last_name = 'Hosseini' WHERE hk = $1`, ali)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, db, rowseal.Users, "hk = $1", ali))
	action, data, err = s.Screen(ctx, db, userRule(aml.ActionFreeze), aml.Event{UserHK: ali})
	require.NoError(t, err)
	assert.Equal(t, aml.ActionReview, action, "a name is held for review, never more")
	assert.Equal(t, "name", data["matched"])
	action, _, err = s.Screen(ctx, db, userRule(aml.ActionAlert), aml.Event{UserHK: ali})
	require.NoError(t, err)
	assert.Equal(t, aml.ActionAlert, action, "nor more than the rule")

	// A name changed in SQL to slip past the list is refused.
	_, err = db.Exec(`UPDATE users SET last_name = 'Tehrani' WHERE hk = $1`, ali)
	require.NoError(t, err)
	_, _, err = s.Screen(ctx, db, userRule(aml.ActionReview), aml.Event{UserHK: ali})
	require.ErrorIs(t, err, rowseal.ErrUnverified)

	nameless := usertest.Insert(t, db, usertest.User{Email: "nameless@example.com"})
	require.NoError(t, rowseal.Reseal(ctx, db, rowseal.Users, "hk = $1", nameless))
	action, _, err = s.Screen(ctx, db, userRule(aml.ActionReview), aml.Event{UserHK: nameless})
	require.NoError(t, err)
	assert.Equal(t, aml.ActionNone, action)

	// A list that no download found current for the maximum age is not
	// used.
	now = now.Add(73 * time.Hour)
	action, data, err = s.Screen(ctx, db, addressRule(aml.ActionBlock), aml.Event{CounterpartyAddress: "TQn9Y2khEsLJW1ChVWFMSMeRDow5KcbLSE"})
	require.NoError(t, err)
	assert.Equal(t, aml.ActionReview, action)
	assert.Equal(t, reasonListUnavailable, data["reason"])
	action, _, err = s.Screen(ctx, db, addressRule(aml.ActionBlock), aml.Event{})
	require.NoError(t, err)
	assert.Equal(t, aml.ActionNone, action, "a fiat destination is not held for want of a list")
	st, err := s.Status(ctx)
	require.NoError(t, err)
	assert.True(t, st.Stale)
}
