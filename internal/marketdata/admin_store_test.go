package marketdata

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adminOrigin is a change by an administrator.
var adminOrigin = audit.Event{ActorType: audit.ActorAdmin, ActorID: "0192f3a4-0000-7000-8000-000000000001"}

type auditRow struct {
	ActorType string
	Target    string
	Changes   audit.Changes
}

// auditEvents returns the events of action, oldest first.
func auditEvents(t *testing.T, db *sql.DB, action string) []auditRow {
	t.Helper()
	rows, err := db.Query(`SELECT actor_type, target_type || ':' || target_id, COALESCE(changes, '{}')
		FROM audit_events WHERE action = $1 ORDER BY occurred_at, hk`, action)
	require.NoError(t, err)
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		var changes []byte
		require.NoError(t, rows.Scan(&r.ActorType, &r.Target, &changes))
		require.NoError(t, json.Unmarshal(changes, &r.Changes))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestAdmin_SourcePauseAndResume(t *testing.T) {
	store, db := newStore(t)
	ctx := context.Background()

	_, err := store.SetSourcePaused(ctx, "coingecko", true, "", adminOrigin)
	require.ErrorIs(t, err, ErrInvalid, "a pause needs a reason")
	_, err = store.SetSourcePaused(ctx, "Coin Gecko", true, "x", adminOrigin)
	require.ErrorIs(t, err, ErrInvalid)
	_, err = store.SetSourcePaused(ctx, "coingecko", true, strings.Repeat("x", 1001), adminOrigin)
	require.ErrorIs(t, err, ErrInvalid)

	c, err := store.SetSourcePaused(ctx, "coingecko", true, "  The plan ran out.  ", adminOrigin)
	require.NoError(t, err)
	assert.True(t, c.Paused)
	assert.Equal(t, "The plan ran out.", c.Reason)
	_, err = store.SetSourcePaused(ctx, "coingecko", true, "again", adminOrigin)
	require.NoError(t, err)
	events := auditEvents(t, db, audit.ActionMarketSourcePause)
	require.Len(t, events, 1, "a pause of a paused source changes nothing")
	assert.Equal(t, "admin", events[0].ActorType)
	assert.Equal(t, "market_source:coingecko", events[0].Target)
	assert.Equal(t, "The plan ran out.", events[0].Changes.After["reason"])

	controls, err := store.SourceControls(ctx)
	require.NoError(t, err)
	assert.True(t, controls["coingecko"].Paused)

	c, err = store.SetSourcePaused(ctx, "coingecko", false, "", adminOrigin)
	require.NoError(t, err)
	assert.False(t, c.Paused)
	assert.Len(t, auditEvents(t, db, audit.ActionMarketSourceResume), 1)
}

func TestAdmin_JobsRunsAndUsage(t *testing.T) {
	store, db := newStore(t)
	ctx := context.Background()

	assert.ErrorIs(t, store.RunJobNow(ctx, "tickers:binance", adminOrigin), ErrNotFound, "no worker has claimed it")
	_, err := db.Exec(`INSERT INTO market_job_leases (job, next_run_at) VALUES ('tickers:binance', NOW() + interval '1 hour'),
		('candles:binance:1h', NOW() + interval '1 hour')`)
	require.NoError(t, err)
	require.NoError(t, store.RunJobNow(ctx, "tickers:binance", adminOrigin))
	states, err := store.JobStates(ctx)
	require.NoError(t, err)
	require.Len(t, states, 2)
	assert.Equal(t, "candles:binance:1h", states[0].Job)
	var due bool
	require.NoError(t, db.QueryRow(`SELECT next_run_at <= NOW() FROM market_job_leases WHERE job = 'tickers:binance'`).Scan(&due))
	assert.True(t, due)
	require.Len(t, auditEvents(t, db, audit.ActionMarketJobRun), 1)

	_, err = db.Exec(`UPDATE market_job_leases SET holder = 'node-a', lease_until = NOW() + interval '1 minute' WHERE job = 'candles:binance:1h'`)
	require.NoError(t, err)
	assert.ErrorIs(t, store.RunJobNow(ctx, "candles:binance:1h", adminOrigin), ErrJobRunning)
	states, err = store.JobStates(ctx)
	require.NoError(t, err)
	assert.True(t, states[0].Running)
	assert.Equal(t, "node-a", states[0].Holder)

	assert.Equal(t, "binance", SourceOf("candles:binance:1h"))
	assert.Equal(t, "", SourceOf("tiers"))

	var ids []int64
	for _, job := range []string{"tickers:binance", "catalog:coingecko", "tickers:binance"} {
		id, err := store.StartRun(ctx, job, "node-a", ModeLive)
		require.NoError(t, err)
		ids = append(ids, id)
	}
	require.NoError(t, store.FinishRun(ctx, ids[0], 1, 1, nil))
	runs, err := store.ListRuns(ctx, RunFilter{Job: "tickers:binance"})
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, ids[2], runs[0].ID, "newest first")
	runs, err = store.ListRuns(ctx, RunFilter{Status: "succeeded"})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	runs, err = store.ListRuns(ctx, RunFilter{BeforeID: ids[2], Limit: 1})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, ids[1], runs[0].ID)
	_, err = store.ListRuns(ctx, RunFilter{Status: "lost"})
	assert.ErrorIs(t, err, ErrInvalid)

	_, err = db.Exec(`INSERT INTO market_source_usage (source, window_kind, window_start, requests) VALUES
		('coingecko', 'minute', date_trunc('minute', NOW() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', 7),
		('coingecko', 'month', date_trunc('month', NOW() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', 900),
		('binance', 'minute', NOW() - interval '1 hour', 50)`)
	require.NoError(t, err)
	usage, err := store.SourceUsages(ctx)
	require.NoError(t, err)
	assert.Equal(t, SourceUsage{Minute: 7, Month: 900}, usage["coingecko"])
	assert.Equal(t, SourceUsage{}, usage["binance"], "a passed window counts nothing")
}

func TestAdmin_VenueToggle(t *testing.T) {
	store, db := newStore(t)
	ctx := context.Background()
	require.NoError(t, store.UpsertVenue(ctx, Venue{ID: "binance", Name: "Binance", Kind: VenueCEX, Region: RegionGlobal}))

	v, err := store.SetVenueEnabled(ctx, "binance", true, "Contract signed.", adminOrigin)
	require.NoError(t, err)
	assert.True(t, v.Enabled)
	enabled, err := store.VenueEnabled(ctx, "binance")
	require.NoError(t, err)
	assert.True(t, enabled)
	_, err = store.SetVenueEnabled(ctx, "binance", true, "", adminOrigin)
	require.NoError(t, err)
	assert.Len(t, auditEvents(t, db, audit.ActionMarketVenueEnable), 1)

	_, err = store.SetVenueEnabled(ctx, "binance", false, "", adminOrigin)
	require.NoError(t, err)
	assert.Len(t, auditEvents(t, db, audit.ActionMarketVenueDisable), 1)
	_, err = store.SetVenueEnabled(ctx, "nowhere", true, "", adminOrigin)
	assert.ErrorIs(t, err, ErrNotFound)
}

// currencyRow is a listed currency, as stored, to show that a link never
// changes it.
func currencyRow(t *testing.T, db *sql.DB, hk string) string {
	t.Helper()
	var row string
	require.NoError(t, db.QueryRow(`SELECT row_to_json(c)::text FROM currencies c WHERE hk = $1`, hk).Scan(&row))
	return row
}

func addCurrency(t *testing.T, db *sql.DB, symbol string) string {
	t.Helper()
	info, err := json.Marshal(models.CurrencyInfo{Symbol: symbol, EnName: symbol, FaName: symbol})
	require.NoError(t, err)
	var hk string
	require.NoError(t, db.QueryRow(`INSERT INTO currencies (status, info) VALUES ('approved', $1::jsonb) RETURNING hk`, info).Scan(&hk))
	return hk
}

func TestAdmin_HideAndLinkAssets(t *testing.T) {
	store, db := newStore(t)
	ctx := context.Background()
	btc, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", bitcoin())
	require.NoError(t, err)
	scam, _, err := store.CreateAsset(ctx, "scam", "coingecko", "scam", AssetDetails{Symbol: "SCAM", Name: "Scam", Kind: KindToken})
	require.NoError(t, err)
	hk := addCurrency(t, db, "BTC")
	before := currencyRow(t, db, hk)

	_, err = store.SetAssetHidden(ctx, scam, true, "", adminOrigin)
	require.ErrorIs(t, err, ErrInvalid, "hiding needs a reason")
	a, err := store.SetAssetHidden(ctx, scam, true, "A copy of a known token.", adminOrigin)
	require.NoError(t, err)
	assert.Equal(t, StatusHidden, a.Status)
	_, err = store.LinkAsset(ctx, scam, hk, "", adminOrigin)
	assert.ErrorIs(t, err, ErrAssetHidden)
	hidden, err := store.AdminAssets(ctx, AssetFilter{Status: StatusHidden})
	require.NoError(t, err)
	require.Len(t, hidden, 1)
	assert.Equal(t, scam, hidden[0].ID)
	a, err = store.SetAssetHidden(ctx, scam, false, "", adminOrigin)
	require.NoError(t, err)
	assert.Equal(t, StatusUnverified, a.Status)
	assert.Len(t, auditEvents(t, db, audit.ActionMarketAssetHide), 1)
	assert.Len(t, auditEvents(t, db, audit.ActionMarketAssetUnhide), 1)

	_, err = store.LinkAsset(ctx, btc, "not-a-uuid", "", adminOrigin)
	assert.ErrorIs(t, err, ErrInvalid)
	_, err = store.LinkAsset(ctx, btc, "0192f3a4-0000-7000-8000-00000000abcd", "", adminOrigin)
	assert.ErrorIs(t, err, ErrCurrencyNotFound)
	a, err = store.LinkAsset(ctx, btc, strings.ToUpper(hk), "Same coin.", adminOrigin)
	require.NoError(t, err)
	assert.Equal(t, StatusLinked, a.Status)
	require.NotNil(t, a.CurrencyHK)
	assert.Equal(t, hk, *a.CurrencyHK)
	_, err = store.LinkAsset(ctx, btc, hk, "", adminOrigin)
	require.NoError(t, err, "linking again to the same currency changes nothing")
	_, err = store.LinkAsset(ctx, scam, hk, "", adminOrigin)
	assert.ErrorIs(t, err, ErrCurrencyLinked, "a currency is linked to one asset")
	other := addCurrency(t, db, "WBTC")
	_, err = store.LinkAsset(ctx, btc, other, "", adminOrigin)
	assert.ErrorIs(t, err, ErrAssetLinked)
	_, err = store.SetAssetHidden(ctx, btc, true, "x", adminOrigin)
	assert.ErrorIs(t, err, ErrAssetLinked)
	events := auditEvents(t, db, audit.ActionMarketAssetLink)
	require.Len(t, events, 1)
	assert.Equal(t, "market_asset:"+strconv.FormatInt(btc, 10), events[0].Target)
	assert.Equal(t, hk, events[0].Changes.After["currency_hk"])

	a, err = store.UnlinkAsset(ctx, btc, "Wrong coin.", adminOrigin)
	require.NoError(t, err)
	assert.Equal(t, StatusUnverified, a.Status)
	assert.Nil(t, a.CurrencyHK)
	_, err = store.UnlinkAsset(ctx, btc, "", adminOrigin)
	assert.ErrorIs(t, err, ErrAssetNotLinked)
	events = auditEvents(t, db, audit.ActionMarketAssetUnlink)
	require.Len(t, events, 1)
	assert.Equal(t, hk, events[0].Changes.Before["currency_hk"])

	assert.Equal(t, before, currencyRow(t, db, hk), "a link never changes the currency")

	found, err := store.AdminAssets(ctx, AssetFilter{Query: "bit"})
	require.NoError(t, err)
	require.Len(t, found, 1)
	found, err = store.AdminAssets(ctx, AssetFilter{Query: "scam"})
	require.NoError(t, err)
	require.Len(t, found, 1)
	found, err = store.AdminAssets(ctx, AssetFilter{AfterID: btc, Limit: 1})
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, scam, found[0].ID)
	_, err = store.AdminAsset(ctx, 999999)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestAdmin_PairsTiersAndConflicts(t *testing.T) {
	store, db := newStore(t)
	ctx := context.Background()
	btc, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", bitcoin())
	require.NoError(t, err)
	usdt, _, err := store.CreateAsset(ctx, "tether", "coingecko", "tether", AssetDetails{Symbol: "USDT", Name: "Tether", Kind: KindToken})
	require.NoError(t, err)
	require.NoError(t, store.UpsertVenue(ctx, Venue{ID: "binance", Name: "Binance", Kind: VenueCEX, Region: RegionGlobal, Enabled: true}))
	ids, err := store.UpsertVenuePairs(ctx, "binance", []VenuePair{{VenueSymbol: "XBTUSDT", BaseSymbol: "XBT", QuoteSymbol: "USDT"}})
	require.NoError(t, err)
	pair := ids["XBTUSDT"]

	unmapped, err := store.AdminPairs(ctx, PairFilter{Venue: "binance", Unmapped: true})
	require.NoError(t, err)
	require.Len(t, unmapped, 1)
	assert.ErrorIs(t, store.MapPair(ctx, pair, btc, btc, adminOrigin), ErrInvalid)
	assert.ErrorIs(t, store.MapPair(ctx, pair, btc, 999999, adminOrigin), ErrNotFound)
	assert.ErrorIs(t, store.MapPair(ctx, 999999, btc, usdt, adminOrigin), ErrNotFound)
	require.NoError(t, store.MapPair(ctx, pair, btc, usdt, adminOrigin))
	unmapped, err = store.AdminPairs(ctx, PairFilter{Unmapped: true})
	require.NoError(t, err)
	assert.Empty(t, unmapped)
	_, err = store.UpsertVenuePairs(ctx, "binance", []VenuePair{{VenueSymbol: "XBTUSDT", BaseSymbol: "XBT", QuoteSymbol: "USDT"}})
	require.NoError(t, err)
	pairs, err := store.AdminPairs(ctx, PairFilter{})
	require.NoError(t, err)
	require.Len(t, pairs, 1)
	require.NotNil(t, pairs[0].BaseAssetID)
	assert.Equal(t, btc, *pairs[0].BaseAssetID, "the jobs keep the mapping")
	assert.Len(t, auditEvents(t, db, audit.ActionMarketPairMap), 1)

	hot := TierHot
	require.NoError(t, store.SetPairTierOverride(ctx, pair, &hot, adminOrigin))
	require.NoError(t, store.SetPairTierOverride(ctx, pair, &hot, adminOrigin))
	require.NoError(t, store.SetAssetTierOverride(ctx, btc, &hot, adminOrigin))
	require.NoError(t, store.SetPairTierOverride(ctx, pair, nil, adminOrigin))
	events := auditEvents(t, db, audit.ActionMarketTierOverride)
	require.Len(t, events, 3, "setting the same tier again changes nothing")
	assert.Equal(t, "market_pair:"+strconv.FormatInt(pair, 10), events[0].Target)
	assert.Equal(t, "market_asset:"+strconv.FormatInt(btc, 10), events[1].Target)
	warm := "warm"
	assert.ErrorIs(t, store.SetPairTierOverride(ctx, pair, &warm, adminOrigin), ErrInvalid)
	a, err := store.AdminAsset(ctx, btc)
	require.NoError(t, err)
	require.NotNil(t, a.TierOverride)
	assert.Equal(t, TierHot, *a.TierOverride)

	_, err = store.RecordConflict(ctx, Conflict{Source: "coinmarketcap", ExternalID: "1", Kind: ConflictSymbol, Subject: "BTC", AssetID: &btc})
	require.NoError(t, err)
	open, err := store.ListConflicts(ctx, ConflictFilter{Source: "coinmarketcap"})
	require.NoError(t, err)
	require.Len(t, open, 1)
	assert.ErrorIs(t, store.ResolveConflict(ctx, open[0].ID, " ", adminOrigin), ErrInvalid, "a resolution needs a note")
	require.NoError(t, store.ResolveConflict(ctx, open[0].ID, "The same coin; nothing to change.", adminOrigin))
	assert.ErrorIs(t, store.ResolveConflict(ctx, open[0].ID, "again", adminOrigin), ErrConflictResolved)
	assert.ErrorIs(t, store.ResolveConflict(ctx, 999999, "x", adminOrigin), ErrNotFound)
	open, err = store.ListConflicts(ctx, ConflictFilter{})
	require.NoError(t, err)
	assert.Empty(t, open)
	resolved, err := store.ListConflicts(ctx, ConflictFilter{Resolved: true})
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	assert.Equal(t, "The same coin; nothing to change.", resolved[0].ResolvedNote)
	assert.NotNil(t, resolved[0].ResolvedAt)
	events = auditEvents(t, db, audit.ActionMarketConflictResolve)
	require.Len(t, events, 1)
	assert.Equal(t, "BTC", events[0].Changes.Before["subject"])
}
