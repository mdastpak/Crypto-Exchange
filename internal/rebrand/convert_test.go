package rebrand_test

import (
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rebrand"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type holdings struct {
	t  *testing.T
	db *sql.DB
}

func (h holdings) user() ledger.Account {
	h.t.Helper()
	var hk shared.UUID
	require.NoError(h.t, h.db.QueryRow(`INSERT INTO users (status) VALUES ('ACTIVE') RETURNING hk`).Scan(&hk))
	return ledger.Account{UserHK: hk}
}

func (h holdings) fund(a ledger.Account, currency, value string) {
	h.t.Helper()
	hk, err := shared.UUIDFromString(currency)
	require.NoError(h.t, err)
	amount := ledger.Amount{Currency: hk, Value: shared.MustNewDecimalFromString(value)}
	key := "fund:" + uuid.NewString()
	l := ledger.Ledger{DB: h.db}
	_, err = l.DetectDeposit(ctx, key+":detect", a, amount, ledger.Ref{Type: "deposit", ID: key})
	require.NoError(h.t, err)
	_, err = l.CreditDeposit(ctx, key+":credit", a, amount, ledger.Ref{Type: "deposit", ID: key})
	require.NoError(h.t, err)
}

// balance returns available, locked, and pending.
func (h holdings) balance(a ledger.Account, currency string) [3]string {
	h.t.Helper()
	hk, err := shared.UUIDFromString(currency)
	require.NoError(h.t, err)
	b, err := ledger.Ledger{DB: h.db}.Balance(ctx, a, hk)
	require.NoError(h.t, err)
	return [3]string{b.Available.String(), b.Locked.String(), b.PendingDeposit.String()}
}

func (h holdings) network(currency string, decimals int) {
	h.t.Helper()
	_, err := h.db.Exec(`INSERT INTO currency_networks (currency_hk, network, decimals, custody_mode) VALUES ($1, 'net', $2, 'ON_CHAIN')`,
		currency, decimals)
	require.NoError(h.t, err)
}

func (h holdings) phase(target string) string {
	h.t.Helper()
	var p string
	require.NoError(h.t, h.db.QueryRow(`SELECT phase FROM currency_migrations WHERE target_hk = $1`, target).Scan(&p))
	return p
}

// TestConvert_AForcedMigrationConvertsEveryHolder converts the holders of
// two sources into one target at their ratios, keeps the dust under the
// target's precision, and completes only once no source is held: a locked
// balance holds the completion until it is released and converted.
func TestConvert_AForcedMigrationConvertsEveryHolder(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	h := holdings{t: t, db: db}
	lend, agix, aave := insertCurrency(t, db, "LEND"), insertCurrency(t, db, "AGIX"), insertCurrency(t, db, "AAVE")
	h.network(aave, 2)
	alice, bob, carol := h.user(), h.user(), h.user()
	h.fund(alice, lend, "150.5")
	h.fund(bob, lend, "0.1")
	h.fund(bob, agix, "3")
	h.fund(carol, agix, "10")
	l := ledger.Ledger{DB: db}
	agixHK, _ := shared.UUIDFromString(agix)
	_, err := l.Lock(ctx, "hold:carol", carol, ledger.Amount{Currency: agixHK, Value: shared.MustNewDecimalFromString("4")},
		ledger.Ref{Type: "withdrawal", ID: "w1"})
	require.NoError(t, err)

	start := time.Now()
	m, err := rebrand.Schedule(ctx, db, aave, plan(start, true, []rebrand.Source{{CurrencyHK: lend, Ratio: "0.01"}, {CurrencyHK: agix, Ratio: "0.5"}},
		allPhases...), origin())
	require.NoError(t, err)
	_, err = rebrand.Advance(ctx, db, start.Add(24*time.Hour), 10)
	require.NoError(t, err)

	assert.Equal(t, [3]string{"0", "0", "0"}, h.balance(alice, lend))
	assert.Equal(t, [3]string{"1.5", "0", "0"}, h.balance(alice, aave), "150.5 × 0.01 is 1.505; 0.005 is dust")
	assert.Equal(t, [3]string{"0", "0", "0"}, h.balance(bob, lend), "all dust")
	assert.Equal(t, [3]string{"1.5", "0", "0"}, h.balance(bob, aave))
	assert.Equal(t, [3]string{"0", "4", "0"}, h.balance(carol, agix), "locked funds wait")
	assert.Equal(t, [3]string{"3", "0", "0"}, h.balance(carol, aave))
	assert.Equal(t, "converting", h.phase(aave), "a held source keeps the migration from completing")

	_, err = l.Release(ctx, "hold:carol:release", carol, ledger.Amount{Currency: agixHK, Value: shared.MustNewDecimalFromString("4")},
		ledger.Ref{Type: "withdrawal", ID: "w1"})
	require.NoError(t, err)
	_, err = rebrand.Advance(ctx, db, start.Add(24*time.Hour), 10)
	require.NoError(t, err)
	assert.Equal(t, [3]string{"5", "0", "0"}, h.balance(carol, aave))
	_, err = rebrand.Advance(ctx, db, start.Add(24*time.Hour), 10)
	require.NoError(t, err)
	assert.Equal(t, "completed", h.phase(aave))

	var entries int
	var dust string
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM ledger_entries WHERE kind = 'conversion' AND reference_id = $1`, m.ID).Scan(&entries))
	assert.Equal(t, 5, entries, "one entry per converted balance")
	report, err := l.Reconcile(ctx, nil)
	require.NoError(t, err)
	assert.True(t, report.OK(), "%+v", report)
	for _, c := range report.Currencies {
		if c.Currency.String() == aave {
			dust = c.Accounts[ledger.AccountFees].String()
		}
	}
	assert.Equal(t, "0.006", dust)

	progress, err := rebrand.Progress(ctx, db, aave, m.ID)
	require.NoError(t, err)
	require.Len(t, progress, 2)
	byHK := map[string]rebrand.SourceProgress{}
	for _, p := range progress {
		assert.Zero(t, p.Holders)
		assert.Equal(t, "0", p.Remaining)
		byHK[p.CurrencyHK] = p
	}
	assert.Equal(t, rebrand.SourceProgress{CurrencyHK: lend, Remaining: "0", Conversions: 2, Converted: "150.6", Credited: "1.5", Dust: "0.006"}, byHK[lend])
	assert.Equal(t, rebrand.SourceProgress{CurrencyHK: agix, Remaining: "0", Conversions: 3, Converted: "13", Credited: "6.5", Dust: "0"}, byHK[agix])
	_, err = rebrand.Progress(ctx, db, lend, m.ID)
	assert.ErrorIs(t, err, rebrand.ErrNotFound, "through its target only")
}

// TestConvert_InstancesShareTheHoldings runs eight schedulers over many
// holders: each balance converts once, and every holder gets the target.
func TestConvert_InstancesShareTheHoldings(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	h := holdings{t: t, db: db}
	source, target := insertCurrency(t, db, "S"+uuid.NewString()[:6]), insertCurrency(t, db, "T"+uuid.NewString()[:6])
	var users []ledger.Account
	for i := range 40 {
		u := h.user()
		h.fund(u, source, fmt.Sprint(i+1))
		users = append(users, u)
	}
	start := time.Now()
	m, err := rebrand.Schedule(ctx, db, target, plan(start, true, []rebrand.Source{{CurrencyHK: source, Ratio: "1000"}}, allPhases...), origin())
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
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
		assert.Equal(t, [3]string{fmt.Sprint((i + 1) * 1000), "0", "0"}, h.balance(u, target))
		assert.Equal(t, [3]string{"0", "0", "0"}, h.balance(u, source))
	}
	var entries int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM ledger_entries WHERE kind = 'conversion' AND reference_id = $1`, m.ID).Scan(&entries))
	assert.Equal(t, len(users), entries)
	assert.Equal(t, "completed", h.phase(target))
}

// TestConvert_AnOptionalMigrationConvertsOnRequest converts a user's
// holding of the source of an optional migration when they ask, only while
// it converts, and refuses a restricted user.
func TestConvert_AnOptionalMigrationConvertsOnRequest(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	h := holdings{t: t, db: db}
	mkr, sky, other := insertCurrency(t, db, "MKR"), insertCurrency(t, db, "SKY"), insertCurrency(t, db, "OTHER")
	alice, bob := h.user(), h.user()
	h.fund(alice, mkr, "2.5")
	start := time.Now()
	m, err := rebrand.Schedule(ctx, db, sky, plan(start, false, []rebrand.Source{{CurrencyHK: mkr, Ratio: "24000"}},
		rebrand.PhaseAnnounced, rebrand.PhaseConverting, rebrand.PhaseCompleted), origin())
	require.NoError(t, err)

	_, err = rebrand.ConvertOwn(ctx, db, m.ID, mkr, alice)
	assert.ErrorIs(t, err, rebrand.ErrNotConverting, "not before it converts")
	_, err = rebrand.Advance(ctx, db, start.Add(90*time.Minute), 10)
	require.NoError(t, err)
	assert.Equal(t, [3]string{"2.5", "0", "0"}, h.balance(alice, mkr), "an optional migration converts no one by itself")

	_, err = rebrand.ConvertOwn(ctx, db, m.ID, other, alice)
	assert.ErrorIs(t, err, rebrand.ErrNotASource)
	_, err = rebrand.ConvertOwn(ctx, db, m.ID, mkr, bob)
	assert.ErrorIs(t, err, rebrand.ErrNothingToConvert)
	_, err = rebrand.ConvertOwn(ctx, db, uuid.NewString(), mkr, alice)
	assert.ErrorIs(t, err, rebrand.ErrNotFound)

	carol := h.user()
	h.fund(carol, mkr, "1")
	admin := uuid.NewString()
	_, err = restrictions.Create(ctx, db, restrictions.New{UserHK: carol.UserHK.String(), Scope: restrictions.ScopeAsset, CurrencyHK: sky,
		Operations: []restrictions.Operation{restrictions.OpConvert}, Reason: restrictions.ReasonAMLReview, CreatedBy: admin},
		audit.Event{ActorType: audit.ActorAdmin, ActorID: admin})
	require.NoError(t, err)
	_, err = rebrand.ConvertOwn(ctx, db, m.ID, mkr, carol)
	var r *restrictions.Refusal
	require.ErrorAs(t, err, &r, "a restriction of the target blocks it")
	assert.Equal(t, [3]string{"1", "0", "0"}, h.balance(carol, mkr))

	out, err := rebrand.ConvertOwn(ctx, db, m.ID, mkr, alice)
	require.NoError(t, err)
	assert.Equal(t, "60000", out.Credited)
	assert.Equal(t, "2.5", out.Amount)
	assert.Equal(t, [3]string{"0", "0", "0"}, h.balance(alice, mkr))
	assert.Equal(t, [3]string{"60000", "0", "0"}, h.balance(alice, sky))
	_, err = rebrand.ConvertOwn(ctx, db, m.ID, mkr, alice)
	assert.ErrorIs(t, err, rebrand.ErrNothingToConvert, "once")

	_, err = rebrand.Advance(ctx, db, start.Add(3*time.Hour), 10)
	require.NoError(t, err)
	_, err = rebrand.ConvertOwn(ctx, db, m.ID, mkr, alice)
	assert.ErrorIs(t, err, rebrand.ErrNotConverting, "not after it completes")
}
