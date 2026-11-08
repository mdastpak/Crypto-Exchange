package catalog

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/jobs"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSource is a catalog source whose listing and records the test sets.
type fakeSource struct {
	name    string
	listing []source.Asset
	records map[string]source.Asset
	reads   int
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Assets(ctx context.Context, opts source.ListOptions) ([]source.Asset, error) {
	var out []source.Asset
	for _, a := range f.listing {
		if len(opts.IDs) > 0 && !slices.Contains(opts.IDs, a.ExternalID) {
			continue
		}
		out = append(out, a)
		if opts.Limit > 0 && len(out) == opts.Limit {
			break
		}
	}
	return out, nil
}

func (f *fakeSource) Asset(ctx context.Context, id string) (source.Asset, error) {
	f.reads++
	a, ok := f.records[id]
	if !ok {
		return source.Asset{}, source.ErrNotFound
	}
	return a, nil
}

func dec(s string) *shared.Decimal {
	d := shared.MustNewDecimalFromString(s)
	return &d
}

func rank(n int) *int { return &n }

func listed(id, symbol, name string, r int, contracts ...source.Contract) source.Asset {
	return source.Asset{ExternalID: id, Slug: id, Contracts: contracts, Details: marketdata.AssetDetails{
		Symbol: symbol, Name: name, MarketCapRank: rank(r), CirculatingSupply: dec("1000"), Attribution: "Fake"}}
}

var usdtEthereum = source.Contract{Network: "ethereum", Address: "0xdAC17F958D2ee523a2206206994597C13D831ec7"}

func coingecko() *fakeSource {
	return &fakeSource{name: "coingecko",
		listing: []source.Asset{
			listed("bitcoin", "BTC", "Bitcoin", 1),
			listed("tether", "USDT", "Tether", 3),
			listed("uniswap", "UNI", "Uniswap", 25),
		},
		records: map[string]source.Asset{
			"bitcoin": {ExternalID: "bitcoin", Details: marketdata.AssetDetails{Symbol: "BTC", Name: "Bitcoin", Kind: marketdata.KindCoin,
				Description: "The first coin.", Links: map[string]string{"website": "https://bitcoin.org"}}},
			"tether": {ExternalID: "tether", Contracts: []source.Contract{usdtEthereum, {Network: "tron", Address: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"}},
				Details: marketdata.AssetDetails{Symbol: "USDT", Name: "Tether", Kind: marketdata.KindToken, Description: "A stablecoin."}},
			"uniswap": {ExternalID: "uniswap", Contracts: []source.Contract{{Network: "ethereum", Address: "0x1f9840a85d5aF5bf1D1762F925BDADdC4201F984"}},
				Details: marketdata.AssetDetails{Symbol: "UNI", Name: "Uniswap", Kind: marketdata.KindToken}},
		}}
}

func coinmarketcap() *fakeSource {
	return &fakeSource{name: "coinmarketcap",
		listing: []source.Asset{
			// The same coin, by symbol and name.
			listed("1", "BTC", "Bitcoin", 1),
			// The same token, by contract, under another name.
			listed("825", "USDT", "Tether USDt", 4, usdtEthereum),
			// Another asset with the symbol UNI: never merged by symbol.
			listed("99999", "UNI", "Unicorn Token", 4000),
			// An asset only this source knows.
			listed("777", "ONLY", "Only Here", 900),
		}}
}

var priority = []string{"coingecko", "coinmarketcap"}

func newJob(t *testing.T, store *marketdata.Store, src *fakeSource) *Job {
	t.Helper()
	j, err := New(src, store, Options{Priority: priority, Coverage: CoverageAll, DetailsPerRun: 10,
		DetailsMaxAge: 30 * 24 * time.Hour, DeactivateAfter: 7 * 24 * time.Hour})
	require.NoError(t, err)
	return j
}

func live() *jobs.Run { return jobs.NewRun(marketdata.ModeLive, nil) }

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(query, args...).Scan(&n))
	return n
}

func assetOf(t *testing.T, store *marketdata.Store, src, id string) marketdata.Asset {
	t.Helper()
	assetID, err := store.AssetBySource(context.Background(), src, id)
	require.NoError(t, err, "%s %s", src, id)
	a, err := store.Asset(context.Background(), assetID)
	require.NoError(t, err)
	return a
}

func TestPrimary_CreatesAndFillsTheCatalog(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_marketdata_catalog")
	store := marketdata.NewStore(db)
	ctx := context.Background()

	run := live()
	require.NoError(t, newJob(t, store, coingecko()).Run(ctx, run))
	assert.Equal(t, 3, count(t, db, `SELECT count(*) FROM market_assets`))

	usdt := assetOf(t, store, "coingecko", "tether")
	assert.Equal(t, marketdata.StatusUnverified, usdt.Status)
	assert.Equal(t, "A stablecoin.", usdt.Description, "the record was read in the same run")
	assert.Equal(t, marketdata.KindToken, usdt.Kind)
	assert.Equal(t, "1000", usdt.CirculatingSupply.String(), "the record keeps the listing's supply")
	owner, err := store.AssetByContract(ctx, "tron", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")
	require.NoError(t, err)
	assert.Equal(t, usdt.ID, owner)

	btc := assetOf(t, store, "coingecko", "bitcoin")
	assert.Equal(t, "https://bitcoin.org", btc.Links["website"])
	assert.Equal(t, 3, run.Read()-3, "three listed and three records read")
}

func TestPrimary_ASecondRunChangesNothing(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_marketdata_catalog")
	store := marketdata.NewStore(db)
	ctx := context.Background()
	src := coingecko()
	require.NoError(t, newJob(t, store, src).Run(ctx, live()))
	before := count(t, db, `SELECT count(*) FROM market_assets WHERE updated_at < NOW()`)
	reads := src.reads

	run := live()
	require.NoError(t, newJob(t, store, src).Run(ctx, run))
	assert.Equal(t, 0, run.Written(), "nothing new, nothing written")
	assert.Equal(t, reads, src.reads, "the records are fresh, so none is read again")
	assert.Equal(t, before, count(t, db, `SELECT count(*) FROM market_assets WHERE updated_at < NOW()`))

	// A new supply in the listing is written.
	src.listing[0].Details.CirculatingSupply = dec("1001")
	run = live()
	require.NoError(t, newJob(t, store, src).Run(ctx, run))
	assert.Equal(t, 1, run.Written())
	assert.Equal(t, "1001", assetOf(t, store, "coingecko", "bitcoin").CirculatingSupply.String())
}

func TestSecondary_LinksMatchesAndNeverMergesBySymbol(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_marketdata_catalog")
	store := marketdata.NewStore(db)
	ctx := context.Background()
	require.NoError(t, newJob(t, store, coingecko()).Run(ctx, live()))
	require.NoError(t, newJob(t, store, coinmarketcap()).Run(ctx, live()))

	assert.Equal(t, assetOf(t, store, "coingecko", "bitcoin").ID, assetOf(t, store, "coinmarketcap", "1").ID,
		"a coin of the same symbol and name is one asset")
	usdt := assetOf(t, store, "coinmarketcap", "825")
	assert.Equal(t, assetOf(t, store, "coingecko", "tether").ID, usdt.ID, "a token of the same contract is one asset")
	assert.Equal(t, "Tether", usdt.Name, "the primary source owns the fields")
	assert.Equal(t, 3, *usdt.MarketCapRank)

	_, err := store.AssetBySource(ctx, "coinmarketcap", "99999")
	assert.ErrorIs(t, err, marketdata.ErrNotFound, "a second UNI waits for an administrator")
	conflicts, err := store.OpenConflicts(ctx, "coinmarketcap")
	require.NoError(t, err)
	require.Len(t, conflicts, 1)
	assert.Equal(t, marketdata.ConflictSymbol, conflicts[0].Kind)
	assert.Equal(t, "UNI", conflicts[0].Subject)

	only := assetOf(t, store, "coinmarketcap", "777")
	assert.Equal(t, "Only Here", only.Name, "an asset only the secondary knows is created, and it owns its fields")
	assert.Equal(t, 4, count(t, db, `SELECT count(*) FROM market_assets`))

	// The conflict is recorded once while it is open.
	require.NoError(t, newJob(t, store, coinmarketcap()).Run(ctx, live()))
	conflicts, err = store.OpenConflicts(ctx, "coinmarketcap")
	require.NoError(t, err)
	assert.Len(t, conflicts, 1)
}

func TestSecondaryFirst_ThePrimaryLinksTheSameCoin(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_marketdata_catalog")
	store := marketdata.NewStore(db)
	ctx := context.Background()
	// CoinGecko was out of reach, so CoinMarketCap ran first.
	cmc := coinmarketcap()
	cmc.listing = cmc.listing[:1]
	require.NoError(t, newJob(t, store, cmc).Run(ctx, live()))
	cg := coingecko()
	cg.listing = cg.listing[:1]
	require.NoError(t, newJob(t, store, cg).Run(ctx, live()))

	assert.Equal(t, 1, count(t, db, `SELECT count(*) FROM market_assets`))
	assert.Equal(t, "The first coin.", assetOf(t, store, "coingecko", "bitcoin").Description, "the primary now owns the fields")
}

func TestRecord_AContractOfAnotherAssetIsAConflict(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_marketdata_catalog")
	store := marketdata.NewStore(db)
	ctx := context.Background()
	src := coingecko()
	// A fake token claims Uniswap's contract.
	fake := listed("fake-uni", "FUNI", "Fake Uni", 5000)
	src.listing = append(src.listing, fake)
	src.records["fake-uni"] = source.Asset{ExternalID: "fake-uni", Contracts: src.records["uniswap"].Contracts,
		Details: marketdata.AssetDetails{Symbol: "FUNI", Name: "Fake Uni", Kind: marketdata.KindToken}}
	require.NoError(t, newJob(t, store, src).Run(ctx, live()))

	conflicts, err := store.OpenConflicts(ctx, "coingecko")
	require.NoError(t, err)
	require.Len(t, conflicts, 1)
	assert.Equal(t, marketdata.ConflictContract, conflicts[0].Kind)
	assert.Equal(t, "ethereum:0x1f9840a85d5af5bf1d1762f925bdaddc4201f984", conflicts[0].Subject)
	assert.Equal(t, assetOf(t, store, "coingecko", "uniswap").ID, *conflicts[0].AssetID, "the contract stays with its first asset")
}

func TestDryRun_WritesNothing(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_marketdata_catalog")
	store := marketdata.NewStore(db)
	run := jobs.NewRun(marketdata.ModeDryRun, nil)
	require.NoError(t, newJob(t, store, coingecko()).Run(context.Background(), run))
	assert.Equal(t, 0, count(t, db, `SELECT count(*) FROM market_assets`))
	assert.Equal(t, 3, run.Written(), "it counts what it would create")
}

func TestCoverage_ListAndTop(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_marketdata_catalog")
	store := marketdata.NewStore(db)
	ctx := context.Background()
	j, err := New(coingecko(), store, Options{Priority: priority, Coverage: CoverageList, IDs: []string{"tether"}})
	require.NoError(t, err)
	require.NoError(t, j.Run(ctx, live()))
	assert.Equal(t, 1, count(t, db, `SELECT count(*) FROM market_assets`))

	j, err = New(coingecko(), store, Options{Priority: priority, Coverage: CoverageTop, Top: 2})
	require.NoError(t, err)
	require.NoError(t, j.Run(ctx, live()))
	assert.Equal(t, 2, count(t, db, `SELECT count(*) FROM market_assets`))

	_, err = New(coingecko(), store, Options{Priority: priority, Coverage: CoverageList})
	assert.Error(t, err)
	_, err = New(coingecko(), store, Options{Priority: []string{"coinmarketcap"}, Coverage: CoverageAll})
	assert.Error(t, err)
}

func TestPrimaryAll_DeactivatesWhatNoSourceLists(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_marketdata_catalog")
	store := marketdata.NewStore(db)
	ctx := context.Background()
	src := coingecko()
	require.NoError(t, newJob(t, store, src).Run(ctx, live()))
	_, err := db.Exec(`UPDATE market_assets SET last_seen_at = NOW() - interval '8 days' WHERE slug = 'uniswap'`)
	require.NoError(t, err)
	src.listing = src.listing[:2]
	require.NoError(t, newJob(t, store, src).Run(ctx, live()))
	assert.False(t, assetOf(t, store, "coingecko", "uniswap").Active)
	assert.True(t, assetOf(t, store, "coingecko", "bitcoin").Active)
}

func TestRun_StopsWhenTheLeaseIsLost(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_marketdata_catalog")
	store := marketdata.NewStore(db)
	run := jobs.NewRun(marketdata.ModeLive, func(context.Context) error { return jobs.ErrLeaseLost })
	err := newJob(t, store, coingecko()).Run(context.Background(), run)
	assert.ErrorIs(t, err, jobs.ErrLeaseLost)
	assert.Equal(t, 0, count(t, db, `SELECT count(*) FROM market_assets`))
}
