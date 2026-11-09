package marketdataadmin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
)

const adminID = "0192f3a4-0000-7000-8000-0000000000ad"

type fixture struct {
	db          *sql.DB
	store       *marketdata.Store
	router      *gin.Engine
	invalidated *atomic.Int32
	btc, usdt   int64
	pair        int64
	currency    string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	conn := pgtest.Migrated(t, "testdb_marketdata_admin")
	store := marketdata.NewStore(conn)
	ctx := context.Background()
	f := fixture{db: conn, store: store, invalidated: &atomic.Int32{}}
	var err error
	f.btc, _, err = store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", marketdata.AssetDetails{Symbol: "BTC", Name: "Bitcoin", Kind: marketdata.KindCoin})
	require.NoError(t, err)
	f.usdt, _, err = store.CreateAsset(ctx, "tether", "coingecko", "tether", marketdata.AssetDetails{Symbol: "USDT", Name: "Tether", Kind: marketdata.KindToken})
	require.NoError(t, err)
	require.NoError(t, store.UpsertVenue(ctx, marketdata.Venue{ID: "binance", Name: "Binance", Kind: marketdata.VenueCEX, Region: marketdata.RegionGlobal}))
	ids, err := store.UpsertVenuePairs(ctx, "binance", []marketdata.VenuePair{{VenueSymbol: "XBTUSDT", BaseSymbol: "XBT", QuoteSymbol: "USDT"}})
	require.NoError(t, err)
	f.pair = ids["XBTUSDT"]
	info, err := json.Marshal(models.CurrencyInfo{Symbol: "BTC", EnName: "Bitcoin", FaName: "Bitcoin"})
	require.NoError(t, err)
	require.NoError(t, conn.QueryRow(`INSERT INTO currencies (status, info) VALUES ('approved', $1::jsonb) RETURNING hk`, info).Scan(&f.currency))
	_, err = conn.Exec(`INSERT INTO market_job_leases (job, next_run_at) VALUES ('tickers:binance', NOW() + interval '1 hour'),
		('catalog:coingecko', NOW() + interval '1 hour'), ('tiers', NOW())`)
	require.NoError(t, err)
	_, err = conn.Exec(`UPDATE market_job_leases SET failures = 2 WHERE job = 'catalog:coingecko'`)
	require.NoError(t, err)
	_, err = store.RecordConflict(ctx, marketdata.Conflict{Source: "coinmarketcap", ExternalID: "1", Kind: marketdata.ConflictSymbol, Subject: "BTC", AssetID: &f.btc})
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	f.router = gin.New()
	g := f.router.Group("/api/v1/admin", func(c *gin.Context) {
		auth.SetClaims(c, &auth.Claims{UUID: adminID, Role: auth.RoleAdmin})
	})
	New(store, func(context.Context) error { f.invalidated.Add(1); return nil }).Register(g)
	return f
}

type response struct {
	Code int
	Raw  string
	Body map[string]any
	Type string
}

func (r response) info() map[string]any {
	info, _ := r.Body["info"].(map[string]any)
	return info
}

func (r response) items() []any {
	items, _ := r.info()["items"].([]any)
	return items
}

func (f fixture) call(t *testing.T, method, path string, body any) response {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	res := response{Code: rec.Code, Raw: rec.Body.String(), Type: rec.Header().Get("Content-Type")}
	_ = json.Unmarshal(rec.Body.Bytes(), &res.Body)
	return res
}

// audited returns the actor and target of the events of action.
func (f fixture) audited(t *testing.T, action string) []string {
	t.Helper()
	rows, err := f.db.Query(`SELECT actor_type || ' ' || actor_id::text || ' ' || target_type || ':' || target_id
		FROM audit_events WHERE action = $1 ORDER BY occurred_at`, action)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	return out
}

func TestSources_PauseResumeAndHealth(t *testing.T) {
	f := newFixture(t)

	res := f.call(t, http.MethodPost, "/api/v1/admin/market-data/sources/coingecko/pause", nil)
	assert.Equal(t, http.StatusBadRequest, res.Code, "a pause needs a reason")
	assert.Equal(t, "validation_failed", res.Body["code"])
	res = f.call(t, http.MethodPost, "/api/v1/admin/market-data/sources/coingecko/pause", "not an object")
	assert.Equal(t, http.StatusBadRequest, res.Code)

	res = f.call(t, http.MethodPost, "/api/v1/admin/market-data/sources/coingecko/pause", NoteRequest{Reason: "The plan ran out."})
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Equal(t, true, res.info()["paused"])
	assert.Equal(t, []string{"admin " + adminID + " market_source:coingecko"}, f.audited(t, "market_data.source_pause"))

	res = f.call(t, http.MethodGet, "/api/v1/admin/market-data/sources", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	sources := map[string]map[string]any{}
	for _, item := range res.items() {
		s := item.(map[string]any)
		sources[s["source"].(string)] = s
	}
	require.Contains(t, sources, "coingecko")
	require.Contains(t, sources, "binance")
	assert.NotContains(t, sources, "", "a job of no source is no source")
	assert.Equal(t, true, sources["coingecko"]["paused"])
	assert.Equal(t, false, sources["coingecko"]["healthy"], "its job failed")
	assert.Equal(t, true, sources["binance"]["healthy"])
	assert.Len(t, sources["binance"]["jobs"], 1)

	res = f.call(t, http.MethodPost, "/api/v1/admin/market-data/sources/coingecko/resume", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Equal(t, false, res.info()["paused"])
	assert.Len(t, f.audited(t, "market_data.source_resume"), 1)
}

func TestJobsAndRuns(t *testing.T) {
	f := newFixture(t)
	res := f.call(t, http.MethodGet, "/api/v1/admin/market-data/jobs", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Len(t, res.items(), 3)

	res = f.call(t, http.MethodPost, "/api/v1/admin/market-data/jobs/tickers:binance/run", nil)
	require.Equal(t, http.StatusAccepted, res.Code, res.Raw)
	assert.Len(t, f.audited(t, "market_data.job_run"), 1)
	res = f.call(t, http.MethodPost, "/api/v1/admin/market-data/jobs/nothing/run", nil)
	assert.Equal(t, http.StatusNotFound, res.Code)
	assert.Equal(t, CodeNotFound, res.Body["code"])
	_, err := f.db.Exec(`UPDATE market_job_leases SET lease_until = NOW() + interval '1 minute' WHERE job = 'tiers'`)
	require.NoError(t, err)
	res = f.call(t, http.MethodPost, "/api/v1/admin/market-data/jobs/tiers/run", nil)
	assert.Equal(t, http.StatusConflict, res.Code)
	assert.Equal(t, CodeJobRunning, res.Body["code"])

	for i := 0; i < 3; i++ {
		_, err := f.store.StartRun(context.Background(), "tickers:binance", "node-a", marketdata.ModeLive)
		require.NoError(t, err)
	}
	res = f.call(t, http.MethodGet, "/api/v1/admin/market-data/runs?job=tickers:binance&limit=2", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Len(t, res.items(), 2)
	next, _ := res.info()["next_cursor"].(string)
	require.NotEmpty(t, next)
	res = f.call(t, http.MethodGet, "/api/v1/admin/market-data/runs?limit=2&cursor="+next, nil)
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Len(t, res.items(), 1)
	assert.Empty(t, res.info()["next_cursor"])
	for _, bad := range []string{"status=lost", "limit=0", "cursor=nonsense"} {
		res = f.call(t, http.MethodGet, "/api/v1/admin/market-data/runs?"+bad, nil)
		assert.Equal(t, http.StatusBadRequest, res.Code, bad)
	}
}

func TestVenues_Toggle(t *testing.T) {
	f := newFixture(t)
	res := f.call(t, http.MethodPost, "/api/v1/admin/market-data/venues/binance/enable", NoteRequest{Reason: "Ready."})
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Equal(t, true, res.info()["enabled"])
	assert.Equal(t, int32(1), f.invalidated.Load(), "a change drops the cache of the public API")
	res = f.call(t, http.MethodPost, "/api/v1/admin/market-data/venues/binance/disable", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Equal(t, false, res.info()["enabled"])
	assert.Len(t, f.audited(t, "market_data.venue_enable"), 1)
	assert.Len(t, f.audited(t, "market_data.venue_disable"), 1)
	res = f.call(t, http.MethodGet, "/api/v1/admin/market-data/venues", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Len(t, res.items(), 1)
	res = f.call(t, http.MethodPost, "/api/v1/admin/market-data/venues/nowhere/enable", nil)
	assert.Equal(t, http.StatusNotFound, res.Code)
}

func TestAssets_HideLinkAndTier(t *testing.T) {
	f := newFixture(t)
	btc := fmt.Sprint(f.btc)
	var before string
	require.NoError(t, f.db.QueryRow(`SELECT row_to_json(c)::text FROM currencies c WHERE hk = $1`, f.currency).Scan(&before))

	res := f.call(t, http.MethodPost, "/api/v1/admin/market-data/assets/"+btc+"/hide", NoteRequest{Reason: "Check it first."})
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Equal(t, "hidden", res.info()["status"])
	res = f.call(t, http.MethodPost, "/api/v1/admin/market-data/assets/"+btc+"/link", LinkRequest{CurrencyHK: f.currency})
	assert.Equal(t, http.StatusConflict, res.Code)
	assert.Equal(t, CodeAssetHidden, res.Body["code"])
	res = f.call(t, http.MethodPost, "/api/v1/admin/market-data/assets/"+btc+"/unhide", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Raw)

	res = f.call(t, http.MethodPost, "/api/v1/admin/market-data/assets/"+btc+"/link", LinkRequest{CurrencyHK: "0192f3a4-0000-7000-8000-00000000abcd"})
	assert.Equal(t, http.StatusUnprocessableEntity, res.Code)
	assert.Equal(t, CodeCurrencyNotFound, res.Body["code"])
	res = f.call(t, http.MethodPost, "/api/v1/admin/market-data/assets/"+btc+"/link", LinkRequest{CurrencyHK: f.currency, Reason: "Same coin."})
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Equal(t, "linked", res.info()["status"])
	assert.Equal(t, f.currency, res.info()["currency_hk"])
	res = f.call(t, http.MethodPost, fmt.Sprintf("/api/v1/admin/market-data/assets/%d/link", f.usdt), LinkRequest{CurrencyHK: f.currency})
	assert.Equal(t, http.StatusConflict, res.Code)
	assert.Equal(t, CodeCurrencyLinked, res.Body["code"])
	res = f.call(t, http.MethodPost, "/api/v1/admin/market-data/assets/"+btc+"/hide", NoteRequest{Reason: "x"})
	assert.Equal(t, CodeAssetLinked, res.Body["code"])

	res = f.call(t, http.MethodGet, "/api/v1/admin/market-data/assets?status=linked", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Len(t, res.items(), 1)
	res = f.call(t, http.MethodPost, "/api/v1/admin/market-data/assets/"+btc+"/unlink", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Equal(t, "unverified", res.info()["status"])
	res = f.call(t, http.MethodPost, "/api/v1/admin/market-data/assets/"+btc+"/unlink", nil)
	assert.Equal(t, CodeAssetNotLinked, res.Body["code"])

	var after string
	require.NoError(t, f.db.QueryRow(`SELECT row_to_json(c)::text FROM currencies c WHERE hk = $1`, f.currency).Scan(&after))
	assert.Equal(t, before, after, "a link never changes the currency")
	for action, n := range map[string]int{"market_data.asset_hide": 1, "market_data.asset_unhide": 1, "market_data.asset_link": 1, "market_data.asset_unlink": 1} {
		events := f.audited(t, action)
		require.Len(t, events, n, action)
		assert.Equal(t, "admin "+adminID+" market_asset:"+btc, events[0], action)
	}
	assert.Equal(t, int32(4), f.invalidated.Load())

	res = f.call(t, http.MethodPut, "/api/v1/admin/market-data/assets/"+btc+"/tier", map[string]any{"tier": "hot"})
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Equal(t, "hot", res.info()["tier_override"])
	res = f.call(t, http.MethodPut, "/api/v1/admin/market-data/assets/"+btc+"/tier", map[string]any{"tier": "warm"})
	assert.Equal(t, http.StatusBadRequest, res.Code)
	res = f.call(t, http.MethodGet, "/api/v1/admin/market-data/assets/"+btc, nil)
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Equal(t, "bitcoin", res.info()["slug"])
	res = f.call(t, http.MethodGet, "/api/v1/admin/market-data/assets/999999", nil)
	assert.Equal(t, http.StatusNotFound, res.Code)
	res = f.call(t, http.MethodGet, "/api/v1/admin/market-data/assets/abc", nil)
	assert.Equal(t, http.StatusBadRequest, res.Code)
}

func TestPairsAndConflicts(t *testing.T) {
	f := newFixture(t)
	pair := fmt.Sprint(f.pair)
	res := f.call(t, http.MethodGet, "/api/v1/admin/market-data/pairs?unmapped=true", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	assert.Len(t, res.items(), 1)
	res = f.call(t, http.MethodGet, "/api/v1/admin/market-data/pairs?unmapped=maybe", nil)
	assert.Equal(t, http.StatusBadRequest, res.Code)

	res = f.call(t, http.MethodPut, "/api/v1/admin/market-data/pairs/"+pair+"/mapping", MappingRequest{BaseAssetID: f.btc})
	assert.Equal(t, http.StatusBadRequest, res.Code)
	res = f.call(t, http.MethodPut, "/api/v1/admin/market-data/pairs/"+pair+"/mapping", MappingRequest{BaseAssetID: f.btc, QuoteAssetID: f.usdt})
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	res = f.call(t, http.MethodGet, "/api/v1/admin/market-data/pairs?unmapped=true", nil)
	assert.Empty(t, res.items())
	res = f.call(t, http.MethodPut, "/api/v1/admin/market-data/pairs/"+pair+"/tier", map[string]any{"tier": "active"})
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	res = f.call(t, http.MethodPut, "/api/v1/admin/market-data/pairs/"+pair+"/tier", map[string]any{"tier": nil})
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	res = f.call(t, http.MethodGet, "/api/v1/admin/market-data/pairs?venue=binance", nil)
	require.Len(t, res.items(), 1)
	assert.Nil(t, res.items()[0].(map[string]any)["tier_override"])
	assert.Len(t, f.audited(t, "market_data.pair_map"), 1)
	assert.Equal(t, []string{"admin " + adminID + " market_pair:" + pair, "admin " + adminID + " market_pair:" + pair},
		f.audited(t, "market_data.tier_override"))

	res = f.call(t, http.MethodGet, "/api/v1/admin/market-data/conflicts", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	require.Len(t, res.items(), 1)
	id := fmt.Sprint(res.items()[0].(map[string]any)["id"])
	res = f.call(t, http.MethodPost, "/api/v1/admin/market-data/conflicts/"+id+"/resolve", ResolveRequest{})
	assert.Equal(t, http.StatusBadRequest, res.Code)
	res = f.call(t, http.MethodPost, "/api/v1/admin/market-data/conflicts/"+id+"/resolve", ResolveRequest{Note: "Same coin."})
	require.Equal(t, http.StatusOK, res.Code, res.Raw)
	res = f.call(t, http.MethodPost, "/api/v1/admin/market-data/conflicts/"+id+"/resolve", ResolveRequest{Note: "Again."})
	assert.Equal(t, CodeConflictResolved, res.Body["code"])
	res = f.call(t, http.MethodGet, "/api/v1/admin/market-data/conflicts?resolved=true&source=coinmarketcap", nil)
	require.Len(t, res.items(), 1)
	assert.Equal(t, "Same coin.", res.items()[0].(map[string]any)["resolved_note"])
	assert.Len(t, f.audited(t, "market_data.conflict_resolve"), 1)
	assert.True(t, strings.HasPrefix(res.Type, "application/json"))
}
