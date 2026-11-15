package rebrand_test

import (
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/convhold"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rebrand"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// breakBalance changes a balance of a in SQL so that it fails its seal, and
// returns its id.
func breakBalance(t *testing.T, db *sql.DB, a ledger.Account, currency string) string {
	t.Helper()
	var id string
	require.NoError(t, db.QueryRow(`UPDATE balances SET row_version = row_version + 7 WHERE user_id = $1 AND currency_hk = $2 RETURNING id`,
		a.UserHK, currency).Scan(&id))
	return id
}

// holdOf returns the attempts and the code of the hold of a balance, and
// whether it is open.
func holdOf(t *testing.T, db *sql.DB, balanceID string) (int, string, bool) {
	t.Helper()
	var attempts int
	var code string
	var open bool
	err := db.QueryRow(`SELECT attempts, last_error_code, released_at IS NULL FROM currency_conversion_holds WHERE balance_id = $1`,
		balanceID).Scan(&attempts, &code, &open)
	if err == sql.ErrNoRows {
		return 0, "", false
	}
	require.NoError(t, err)
	return attempts, code, open
}

// TestHolds_OneBalanceThatCannotConvertDoesNotStopTheOthers converts three
// holders of a forced migration, the second with a balance changed in SQL
// (#452): the others convert, and it is held with row_unverified, shown in
// the progress, and the migration waits for it. After a re-seal and a
// retry, it converts and the migration completes.
func TestHolds_OneBalanceThatCannotConvertDoesNotStopTheOthers(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	sealer := sealtest.Use(t, db, rowseal.ModeEnforce)
	h := holdings{t: t, db: db}
	lend, aave := insertCurrency(t, db, "LEND"), insertCurrency(t, db, "AAVE")
	alice, bob, carol := h.user(), h.user(), h.user()
	h.fund(alice, lend, "1")
	h.fund(bob, lend, "2")
	h.fund(carol, lend, "3")
	broken := breakBalance(t, db, bob, lend)
	start := time.Now()
	m, err := rebrand.Schedule(ctx, db, aave, plan(start, true, []rebrand.Source{{CurrencyHK: lend, Ratio: "10"}}, allPhases...), origin())
	require.NoError(t, err)
	for range 3 {
		_, err = rebrand.Advance(ctx, db, start.Add(24*time.Hour), 10)
		require.NoError(t, err)
	}
	assert.Equal(t, [3]string{"10", "0", "0"}, h.balance(alice, aave))
	assert.Equal(t, [3]string{"30", "0", "0"}, h.balance(carol, aave))
	attempts, code, open := holdOf(t, db, broken)
	assert.True(t, open)
	assert.Equal(t, convhold.CodeRowUnverified, code)
	assert.Equal(t, 1, attempts, "the claim skips it until its retry")
	assert.Equal(t, "converting", h.phase(aave), "the migration waits for the held balance")
	progress, err := rebrand.Progress(ctx, db, aave, m.ID)
	require.NoError(t, err)
	require.Len(t, progress, 1)
	assert.Equal(t, 1, progress[0].Blocked)
	assert.Equal(t, map[string]int{convhold.CodeRowUnverified: 1}, progress[0].BlockedCodes)

	// The operator re-seals the balance, and an administrator retries.
	findings, err := rowseal.ListFindings(ctx, db, rowseal.ListOptions{Limit: 100})
	require.NoError(t, err)
	for _, f := range findings {
		if f.Table == rowseal.Balances.Name && f.RowKey == broken {
			_, err = sealer.ResealFinding(ctx, f.ID, audit.Event{ActorType: audit.ActorSystem}, "the balance was reviewed")
			require.NoError(t, err)
		}
	}
	_, err = rebrand.Retry(ctx, db, lend, m.ID, origin())
	assert.ErrorIs(t, err, rebrand.ErrNotFound, "through its target only")
	n, err := rebrand.Retry(ctx, db, aave, m.ID, origin())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 1, count(t, db, `SELECT count(*) FROM audit_events WHERE action = 'currency.conversion_retry' AND target_id = $1`, aave))
	for range 2 {
		_, err = rebrand.Advance(ctx, db, start.Add(24*time.Hour), 10)
		require.NoError(t, err)
	}
	assert.Equal(t, [3]string{"20", "0", "0"}, h.balance(bob, aave))
	assert.Equal(t, "completed", h.phase(aave))
	_, _, open = holdOf(t, db, broken)
	assert.False(t, open, "the conversion released the hold")
	progress, err = rebrand.Progress(ctx, db, aave, m.ID)
	require.NoError(t, err)
	assert.Zero(t, progress[0].Blocked)
}

// TestHolds_InstancesConvertEachBalanceOnce runs schedulers at once over
// holders with one broken balance: every other balance converts once, and
// the broken one is held.
func TestHolds_InstancesConvertEachBalanceOnce(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	sealtest.Use(t, db, rowseal.ModeEnforce)
	h := holdings{t: t, db: db}
	source, target := insertCurrency(t, db, "SRC"), insertCurrency(t, db, "TGT")
	var users []ledger.Account
	for i := range 20 {
		u := h.user()
		h.fund(u, source, fmt.Sprint(i+1))
		users = append(users, u)
	}
	broken := breakBalance(t, db, users[7], source)
	start := time.Now()
	m, err := rebrand.Schedule(ctx, db, target, plan(start, true, []rebrand.Source{{CurrencyHK: source, Ratio: "2"}}, allPhases...), origin())
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if _, err := rebrand.Advance(ctx, db, start.Add(24*time.Hour), 3); err != nil {
					t.Errorf("advance: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	_, err = rebrand.Advance(ctx, db, start.Add(24*time.Hour), 100)
	require.NoError(t, err)
	for i, u := range users {
		if i == 7 {
			continue
		}
		assert.Equal(t, [3]string{fmt.Sprint((i + 1) * 2), "0", "0"}, h.balance(u, target))
	}
	assert.Equal(t, len(users)-1, count(t, db, `SELECT count(*) FROM ledger_entries WHERE kind = 'conversion' AND reference_id = $1`, m.ID))
	attempts, code, open := holdOf(t, db, broken)
	assert.True(t, open)
	assert.Equal(t, convhold.CodeRowUnverified, code)
	assert.Positive(t, attempts)
	assert.Equal(t, "converting", h.phase(target))
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(query, args...).Scan(&n))
	return n
}
