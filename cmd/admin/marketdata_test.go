package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseArgs_MarketData(t *testing.T) {
	cmd, err := parseArgs([]string{"market-data", "pause", "-source", "coingecko", "-reason", "Plan ran out"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "market-data pause", cmd.name)
	assert.Equal(t, "coingecko", cmd.market.source)

	cmd, err = parseArgs([]string{"market-data", "tier", "-pair", "7", "-tier", "none"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, int64(7), cmd.market.pair)

	for _, args := range []string{
		"market-data", "market-data delete", "market-data sources extra", "market-data pause -source coingecko",
		"market-data resume", "market-data run", "market-data runs -limit 0", "market-data venue-enable",
		"market-data hide -asset 1", "market-data link -asset 1", "market-data unhide",
		"market-data tier -tier hot", "market-data tier -asset 1 -pair 2 -tier hot", "market-data tier -asset 1 -tier warm",
		"market-data map -pair 1 -base 2", "market-data resolve -id 1",
	} {
		_, err := parseArgs(strings.Fields(args), &bytes.Buffer{})
		assert.Error(t, err, args)
	}
}

// TestRun_AdministersTheMarketDataCatalog runs every change through the
// command, with the operator in the audit log, and shows that a link never
// changes the currency.
func TestRun_AdministersTheMarketDataCatalog(t *testing.T) {
	resetCLITestDB(t)
	base := append(writeConfig(t, ""), "-operator", "Sara")
	conn, err := sql.Open("pgx", fmt.Sprintf("host=localhost port=5432 user=postgres password=test dbname=%s sslmode=disable", cliTestDBName))
	require.NoError(t, err)
	defer conn.Close()
	// The schema is migrated by the first command.
	code, _, stderr := runCLI(t, append(base, "market-data", "sources")...)
	require.Equal(t, 0, code, stderr)

	ctx := context.Background()
	store := marketdata.NewStore(conn)
	btc, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", marketdata.AssetDetails{Symbol: "BTC", Name: "Bitcoin", Kind: marketdata.KindCoin})
	require.NoError(t, err)
	usdt, _, err := store.CreateAsset(ctx, "tether", "coingecko", "tether", marketdata.AssetDetails{Symbol: "USDT", Name: "Tether", Kind: marketdata.KindToken})
	require.NoError(t, err)
	require.NoError(t, store.UpsertVenue(ctx, marketdata.Venue{ID: "binance", Name: "Binance", Kind: marketdata.VenueCEX, Region: marketdata.RegionGlobal}))
	pairs, err := store.UpsertVenuePairs(ctx, "binance", []marketdata.VenuePair{{VenueSymbol: "XBTUSDT", BaseSymbol: "XBT", QuoteSymbol: "USDT"}})
	require.NoError(t, err)
	_, err = conn.Exec(`INSERT INTO market_job_leases (job, next_run_at, failures) VALUES ('tickers:binance', NOW() + interval '1 hour', 1)`)
	require.NoError(t, err)
	_, err = store.RecordConflict(ctx, marketdata.Conflict{Source: "coinmarketcap", ExternalID: "1", Kind: marketdata.ConflictSymbol, Subject: "BTC"})
	require.NoError(t, err)
	info, err := json.Marshal(models.CurrencyInfo{Symbol: "BTC", EnName: "Bitcoin", FaName: "Bitcoin"})
	require.NoError(t, err)
	var currency, before string
	require.NoError(t, conn.QueryRow(`INSERT INTO currencies (status, info) VALUES ('approved', $1::jsonb) RETURNING hk`, info).Scan(&currency))
	require.NoError(t, conn.QueryRow(`SELECT row_to_json(c)::text FROM currencies c WHERE hk = $1`, currency).Scan(&before))

	ok := func(args ...string) string {
		t.Helper()
		code, stdout, stderr := runCLI(t, append(base, append([]string{"market-data"}, args...)...)...)
		require.Equal(t, 0, code, "%v: %s", args, stderr)
		return stdout
	}
	asset, pair := fmt.Sprint(btc), fmt.Sprint(pairs["XBTUSDT"])

	ok("pause", "-source", "binance", "-reason", "Changed API")
	out := ok("sources")
	assert.Regexp(t, `binance\s+paused\s+1\s+1`, out)
	assert.Contains(t, out, "Changed API")
	ok("resume", "-source", "binance")
	assert.Contains(t, ok("jobs"), "tickers:binance")
	ok("run", "-job", "tickers:binance")
	assert.Contains(t, ok("runs"), "JOB")
	ok("venue-enable", "-venue", "binance")
	assert.Regexp(t, `binance\s+Binance\s+global\s+enabled`, ok("venues"))
	ok("venue-disable", "-venue", "binance", "-reason", "Paused for review")
	ok("hide", "-asset", asset, "-reason", "Check first")
	assert.Regexp(t, `bitcoin\s+BTC\s+Bitcoin\s+hidden`, ok("assets", "-status", "hidden"))
	ok("unhide", "-asset", asset)
	ok("link", "-asset", asset, "-currency", currency)
	assert.Contains(t, ok("assets", "-q", "btc"), currency)
	ok("unlink", "-asset", asset)
	ok("tier", "-asset", asset, "-tier", "hot")
	ok("tier", "-pair", pair, "-tier", "none")
	ok("map", "-pair", pair, "-base", asset, "-quote", fmt.Sprint(usdt))
	out = ok("conflicts")
	assert.Contains(t, out, "coinmarketcap")
	ok("resolve", "-id", strings.Fields(strings.Split(out, "\n")[1])[0], "-note", "Same coin")

	code, _, stderr = runCLI(t, append(base, "market-data", "unlink", "-asset", asset)...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "not linked")
	code, _, stderr = runCLI(t, append(base, "market-data", "run", "-job", "nothing")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "not found")

	var after string
	require.NoError(t, conn.QueryRow(`SELECT row_to_json(c)::text FROM currencies c WHERE hk = $1`, currency).Scan(&after))
	assert.Equal(t, before, after, "a link never changes the currency")

	rows, err := conn.Query(`SELECT action, actor_type, user_agent FROM audit_events WHERE action LIKE 'market_data.%' ORDER BY occurred_at, hk`)
	require.NoError(t, err)
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var action, actor, agent string
		require.NoError(t, rows.Scan(&action, &actor, &agent))
		assert.Equal(t, "system", actor)
		assert.True(t, strings.HasPrefix(agent, "cmd/admin market-data "), agent)
		assert.Contains(t, agent, "operator=Sara")
		actions = append(actions, action)
	}
	assert.Equal(t, []string{"market_data.source_pause", "market_data.source_resume", "market_data.job_run",
		"market_data.venue_enable", "market_data.venue_disable", "market_data.asset_hide", "market_data.asset_unhide",
		"market_data.asset_link", "market_data.asset_unlink", "market_data.tier_override", "market_data.pair_map",
		"market_data.conflict_resolve"}, actions,
		"clearing a tier that was never set changes nothing")
}
