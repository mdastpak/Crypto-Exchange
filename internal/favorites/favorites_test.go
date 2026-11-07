package favorites_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/favorites"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDBName = "testdb_favorites"

var ctx = context.Background()

func insertCurrency(t *testing.T, db *sql.DB, symbol, status string) string {
	t.Helper()
	info, err := json.Marshal(models.CurrencyInfo{EnName: symbol, FaName: symbol, Symbol: symbol})
	require.NoError(t, err)
	var hk string
	require.NoError(t, db.QueryRow(`INSERT INTO currencies (status, info) VALUES ($1, $2::jsonb) RETURNING hk`, status, info).Scan(&hk))
	return hk
}

func insertPair(t *testing.T, db *sql.DB, base, quote, status string) string {
	t.Helper()
	asset := func(symbol string) string {
		var id string
		require.NoError(t, db.QueryRow(`INSERT INTO assets (symbol, name, type) VALUES ($1, $1, 'COIN')
			ON CONFLICT (symbol) DO UPDATE SET name = EXCLUDED.name RETURNING id`, symbol).Scan(&id))
		return id
	}
	var id string
	require.NoError(t, db.QueryRow(`INSERT INTO markets (base_asset_id, quote_asset_id, status) VALUES ($1, $2, $3) RETURNING id`,
		asset(base), asset(quote), status).Scan(&id))
	return id
}

func symbols(list favorites.List) []string {
	out := []string{}
	for _, c := range list.Currencies {
		out = append(out, c.Symbol)
	}
	return out
}

func TestFavorites_AddListReorderRemove(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	user := usertest.Insert(t, db, usertest.User{Email: "fav@example.com"})
	btc, eth, usdt := insertCurrency(t, db, "BTC", "approved"), insertCurrency(t, db, "ETH", "approved"), insertCurrency(t, db, "USDT", "approved")
	pair := insertPair(t, db, "BTC", "USDT", "ACTIVE")

	for _, hk := range []string{btc, eth, usdt} {
		added, err := favorites.Add(ctx, db, user, favorites.KindCurrency, hk, 10)
		require.NoError(t, err)
		assert.True(t, added)
	}
	added, err := favorites.Add(ctx, db, user, favorites.KindCurrency, btc, 10)
	require.NoError(t, err)
	assert.False(t, added, "a repeated add changes nothing")
	added, err = favorites.Add(ctx, db, user, favorites.KindPair, pair, 10)
	require.NoError(t, err)
	assert.True(t, added)

	list, err := favorites.Get(ctx, db, user)
	require.NoError(t, err)
	assert.Equal(t, []string{"BTC", "ETH", "USDT"}, symbols(list))
	require.Len(t, list.Pairs, 1)
	assert.Equal(t, "BTC", list.Pairs[0].Base)
	assert.Equal(t, "USDT", list.Pairs[0].Quote)

	require.NoError(t, favorites.Reorder(ctx, db, user, favorites.KindCurrency, []string{usdt, btc, eth}))
	list, _ = favorites.Get(ctx, db, user)
	assert.Equal(t, []string{"USDT", "BTC", "ETH"}, symbols(list))
	assert.ErrorIs(t, favorites.Reorder(ctx, db, user, favorites.KindCurrency, []string{usdt, btc}), favorites.ErrOrderMismatch, "one is missing")
	assert.ErrorIs(t, favorites.Reorder(ctx, db, user, favorites.KindCurrency, []string{usdt, btc, btc}), favorites.ErrOrderMismatch, "one is twice")

	removed, err := favorites.Remove(ctx, db, user, favorites.KindCurrency, btc)
	require.NoError(t, err)
	assert.True(t, removed)
	removed, err = favorites.Remove(ctx, db, user, favorites.KindCurrency, btc)
	require.NoError(t, err)
	assert.False(t, removed, "a repeated remove changes nothing")
	set, err := favorites.CurrencyHKs(ctx, db, user)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{usdt: true, eth: true}, set)

	// A new favorite goes to the end.
	_, err = favorites.Add(ctx, db, user, favorites.KindCurrency, btc, 10)
	require.NoError(t, err)
	list, _ = favorites.Get(ctx, db, user)
	assert.Equal(t, []string{"USDT", "ETH", "BTC"}, symbols(list))
}

func TestFavorites_OnlyListedOnesAreAddedAndLaterChangesShow(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	user := usertest.Insert(t, db, usertest.User{Email: "listed@example.com"})
	pending := insertCurrency(t, db, "PEN", "pending")
	inactive := insertPair(t, db, "AAA", "BBB", "INACTIVE")
	for name, tc := range map[string]struct {
		kind   favorites.Kind
		target string
	}{
		"a pending currency": {favorites.KindCurrency, pending},
		"no such currency":   {favorites.KindCurrency, uuid.NewString()},
		"an inactive pair":   {favorites.KindPair, inactive},
	} {
		_, err := favorites.Add(ctx, db, user, tc.kind, tc.target, 10)
		assert.ErrorIs(t, err, favorites.ErrNotListed, name)
	}

	btc, eth := insertCurrency(t, db, "BTC", "approved"), insertCurrency(t, db, "ETH", "approved")
	for _, hk := range []string{btc, eth} {
		_, err := favorites.Add(ctx, db, user, favorites.KindCurrency, hk, 10)
		require.NoError(t, err)
	}
	_, err := db.Exec(`UPDATE currencies SET status = 'suspend' WHERE hk = $1`, btc)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE currencies SET status = 'deleted', deleted_at = NOW() WHERE hk = $1`, eth)
	require.NoError(t, err)
	list, err := favorites.Get(ctx, db, user)
	require.NoError(t, err)
	require.Len(t, list.Currencies, 2, "favorites stay when their currency changes")
	assert.Equal(t, "suspend", list.Currencies[0].Status)
	assert.Equal(t, "deleted", list.Currencies[1].Status)
}

// TestFavorites_TheCapHoldsUnderConcurrentAdds adds ten currencies at once,
// as ten instances would, to a user with a cap of three.
func TestFavorites_TheCapHoldsUnderConcurrentAdds(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	user := usertest.Insert(t, db, usertest.User{Email: "cap@example.com"})
	var hks []string
	for _, s := range []string{"A1", "A2", "A3", "A4", "A5", "A6", "A7", "A8", "A9", "B1"} {
		hks = append(hks, insertCurrency(t, db, s, "approved"))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var added, limited int
	for _, hk := range hks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := favorites.Add(ctx, db, user, favorites.KindCurrency, hk, 3)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, favorites.ErrLimit):
				limited++
			case err == nil && ok:
				added++
			default:
				t.Errorf("add: %v", err)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 3, added)
	assert.Equal(t, 7, limited)
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_favorites WHERE user_hk = $1`, user).Scan(&n))
	assert.Equal(t, 3, n)

	// Pairs count in the same cap.
	pair := insertPair(t, db, "CCC", "DDD", "ACTIVE")
	_, err := favorites.Add(ctx, db, user, favorites.KindPair, pair, 3)
	assert.ErrorIs(t, err, favorites.ErrLimit)
}

func TestFavorites_GoWithTheUser(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	user := usertest.Insert(t, db, usertest.User{Email: "gone@example.com"})
	btc := insertCurrency(t, db, "BTC", "approved")
	_, err := favorites.Add(ctx, db, user, favorites.KindCurrency, btc, 10)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE users SET deleted_at = NOW() WHERE hk = $1`, user)
	require.NoError(t, err)
	_, err = favorites.Add(ctx, db, user, favorites.KindCurrency, btc, 10)
	assert.ErrorIs(t, err, favorites.ErrUserNotFound)
}
