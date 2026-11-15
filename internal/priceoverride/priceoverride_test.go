package priceoverride

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/price"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"
)

const adminHK = "5f0c9b4e-2d1a-4c3b-9e8f-7a6b5c4d3e2f"

func d(s string) shared.Decimal { return shared.MustNewDecimalFromString(s) }

// newOverride is an override within the bound of the live reference
// 64000, which applies at once.
func newOverride(market string, lasts time.Duration) New {
	ref := d("64000")
	return New{Market: market, Bid: d("64000"), Ask: d("64100"), Mid: d("64050"), Reason: "Binance and Kraken are down", For: lasts, Reference: &ref}
}

func TestDeviation(t *testing.T) {
	assert.Equal(t, int64(8), Deviation(d("64050"), d("64000")), "7.8 bps, rounded up")
	assert.Equal(t, int64(300), Deviation(d("97"), d("100")))
	assert.Equal(t, int64(301), Deviation(d("103.001"), d("100")))
}

func TestValidate(t *testing.T) {
	assert.NoError(t, Validate(newOverride("BTC/USDT", time.Minute)))
	for name, n := range map[string]New{
		"bid of 0":         {Market: "BTC/USDT", Bid: d("0"), Ask: d("1"), Mid: d("1"), Reason: "r", For: time.Minute},
		"bid above mid":    {Market: "BTC/USDT", Bid: d("2"), Ask: d("3"), Mid: d("1"), Reason: "r", For: time.Minute},
		"mid above ask":    {Market: "BTC/USDT", Bid: d("1"), Ask: d("2"), Mid: d("3"), Reason: "r", For: time.Minute},
		"too long":         {Market: "BTC/USDT", Bid: d("1"), Ask: d("1"), Mid: d("1"), Reason: "r", For: 25 * time.Hour},
		"too short":        {Market: "BTC/USDT", Bid: d("1"), Ask: d("1"), Mid: d("1"), Reason: "r", For: time.Millisecond},
		"no reason":        {Market: "BTC/USDT", Bid: d("1"), Ask: d("1"), Mid: d("1"), For: time.Minute},
		"not a market":     {Market: "BTCUSDT", Bid: d("1"), Ask: d("1"), Mid: d("1"), Reason: "r", For: time.Minute},
		"a long reason":    {Market: "BTC/USDT", Bid: d("1"), Ask: d("1"), Mid: d("1"), Reason: string(make([]byte, 501)), For: time.Minute},
		"no prices at all": {Market: "BTC/USDT", Reason: "r", For: time.Minute},
	} {
		assert.ErrorIs(t, Validate(n), ErrInvalid, name)
	}
}

func TestStore_SetEndAndWatch(t *testing.T) {
	ctx := context.Background()
	conn := pgtest.Migrated(t, "testdb_priceoverride")
	sealtest.Use(t, conn, rowseal.ModeEnforce)
	s := Store{DB: conn}
	w := NewWatcher(conn, time.Second, log.New(io.Discard, "", 0))

	o, err := s.Set(ctx, newOverride("btc-usdt", time.Minute), adminHK, audit.Event{IP: "203.0.113.9"})
	require.NoError(t, err)
	assert.Equal(t, "BTC/USDT", o.Market, "normalized")
	assert.True(t, o.InForce)
	assert.Equal(t, "64050", o.Mid.String())
	assert.WithinDuration(t, o.CreatedAt.Add(time.Minute), o.ExpiresAt, time.Millisecond)

	var n int
	require.NoError(t, conn.QueryRow(`SELECT count(*) FROM audit_events WHERE action = $1 AND target_id = $2 AND actor_id = $3`,
		audit.ActionPriceOverrideSet, o.ID, adminHK).Scan(&n))
	assert.Equal(t, 1, n)

	_, err = s.Set(ctx, newOverride("BTC/USDT", time.Minute), adminHK, audit.Event{})
	assert.ErrorIs(t, err, ErrExists, "one in force per market")

	require.NoError(t, w.Refresh(ctx))
	active, ok := w.Active("BTC/USDT", time.Now())
	require.True(t, ok)
	assert.Equal(t, o.ID, active.ID)
	assert.Equal(t, "64000", active.Bid.String())
	_, ok = w.Active("BTC/USDT", o.ExpiresAt.Add(time.Second))
	assert.False(t, ok, "never past its expiry, even before the next read")
	_, ok = w.Active("ETH/USDT", time.Now())
	assert.False(t, ok)

	list, err := s.List(ctx, true, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)

	ended, err := s.End(ctx, o.ID, adminHK, audit.Event{})
	require.NoError(t, err)
	assert.False(t, ended.InForce)
	require.NotNil(t, ended.EndedAt)
	assert.Equal(t, adminHK, ended.EndedBy)
	_, err = s.End(ctx, o.ID, adminHK, audit.Event{})
	assert.ErrorIs(t, err, ErrEnded)
	_, err = s.End(ctx, "not-a-uuid", adminHK, audit.Event{})
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = s.End(ctx, "00000000-0000-0000-0000-000000000000", adminHK, audit.Event{})
	assert.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, w.Refresh(ctx))
	_, ok = w.Active("BTC/USDT", time.Now())
	assert.False(t, ok, "ended")
	list, err = s.List(ctx, true, 10)
	require.NoError(t, err)
	assert.Empty(t, list)
	list, err = s.List(ctx, false, 10)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	// A market whose override expired takes a new one.
	short, err := s.Set(ctx, newOverride("ETH/USDT", time.Second), adminHK, audit.Event{})
	require.NoError(t, err)
	time.Sleep(1100 * time.Millisecond)
	_, err = s.Set(ctx, newOverride("ETH/USDT", time.Minute), adminHK, audit.Event{})
	require.NoError(t, err)
	all, err := s.List(ctx, false, 10)
	require.NoError(t, err)
	for _, o := range all {
		if o.ID == short.ID {
			require.NotNil(t, o.EndedAt)
			assert.Equal(t, short.ExpiresAt, *o.EndedAt, "closed at its expiry")
		}
	}
}

// An override lengthened in SQL fails its seal: it is not served.
func TestWatcher_SkipsAnOverrideChangedInSQL(t *testing.T) {
	ctx := context.Background()
	conn := pgtest.Migrated(t, "testdb_priceoverride_seal")
	sealtest.Use(t, conn, rowseal.ModeEnforce)
	s := Store{DB: conn}
	o, err := s.Set(ctx, newOverride("BTC/USDT", time.Minute), adminHK, audit.Event{})
	require.NoError(t, err)
	_, err = conn.Exec(`UPDATE price_overrides SET mid = '1', bid = '1', ask = '1' WHERE id = $1`, o.ID)
	require.NoError(t, err)

	w := NewWatcher(conn, time.Second, log.New(io.Discard, "", 0))
	require.NoError(t, w.Refresh(ctx))
	_, ok := w.Active("BTC/USDT", time.Now())
	assert.False(t, ok)
	_, err = s.List(ctx, false, 10)
	assert.ErrorIs(t, err, rowseal.ErrUnverified)
}

// While the database cannot be read, the watcher keeps the overrides it
// read last.
func TestWatcher_KeepsTheLastOverridesWhileTheDatabaseIsDown(t *testing.T) {
	ctx := context.Background()
	conn := pgtest.Migrated(t, "testdb_priceoverride_down")
	sealtest.Use(t, conn, rowseal.ModeEnforce)
	_, err := Store{DB: conn}.Set(ctx, newOverride("BTC/USDT", time.Minute), adminHK, audit.Event{})
	require.NoError(t, err)
	w := NewWatcher(conn, 10*time.Millisecond, log.New(io.Discard, "", 0))
	require.NoError(t, w.Refresh(ctx))
	require.NoError(t, conn.Close())
	runCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	w.Run(runCtx)
	_, ok := w.Active("BTC/USDT", time.Now())
	assert.True(t, ok)
	assert.True(t, w.failing)
}

const secondAdminHK = "6a1d0c5f-3e2b-4d4c-8f9a-8b7c6d5e4f3a"

// TestStore_ABoundedOverride checks the bound of #453: an override within
// it applies at once; one beyond it, or without a live reference, waits for
// another administrator, is not served until then, and lapses.
func TestStore_ABoundedOverride(t *testing.T) {
	ctx := context.Background()
	conn := pgtest.Migrated(t, "testdb_priceoverride_bound")
	sealtest.Use(t, conn, rowseal.ModeEnforce)
	s := Store{DB: conn, MaxDeviationBPS: 300, ApprovalWindow: time.Second}
	w := NewWatcher(conn, time.Second, log.New(io.Discard, "", 0))
	served := func(market string) (price.Override, bool) {
		t.Helper()
		require.NoError(t, w.Refresh(ctx))
		return w.Active(market, time.Now())
	}

	// Within the bound: in force, and served unapproved.
	ref := d("100")
	within := New{Market: "SOL/USDT", Bid: d("96"), Ask: d("98"), Mid: d("97"), Reason: "r", For: time.Minute, Reference: &ref}
	o, err := s.Set(ctx, within, adminHK, audit.Event{})
	require.NoError(t, err)
	assert.True(t, o.InForce)
	assert.False(t, o.Pending)
	assert.Equal(t, int64(300), *o.DeviationBPS)
	active, ok := served("SOL/USDT")
	require.True(t, ok)
	assert.False(t, active.Approved)
	_, err = s.Approve(ctx, o.ID, secondAdminHK, audit.Event{})
	assert.ErrorIs(t, err, ErrNotPending)

	// Beyond the bound: pending, and not served.
	beyond := within
	beyond.Market, beyond.Bid, beyond.Mid, beyond.Ask = "BTC/USDT", d("1"), d("1"), d("1")
	beyondRef := d("64000")
	beyond.Reference = &beyondRef
	p, err := s.Set(ctx, beyond, adminHK, audit.Event{})
	require.NoError(t, err)
	assert.True(t, p.Pending)
	assert.False(t, p.InForce)
	require.NotNil(t, p.ApproveBy)
	_, ok = served("BTC/USDT")
	assert.False(t, ok, "a pending override is not served")
	list, err := s.List(ctx, true, 10)
	require.NoError(t, err)
	assert.Len(t, list, 1, "only the one in force")
	var n int
	require.NoError(t, conn.QueryRow(`SELECT count(*) FROM audit_events WHERE action = $1 AND target_id = $2`,
		audit.ActionPriceOverrideRequest, p.ID).Scan(&n))
	assert.Equal(t, 1, n)
	_, err = s.Set(ctx, beyond, adminHK, audit.Event{})
	assert.ErrorIs(t, err, ErrExists, "a pending override holds its market")

	// The administrator who set it cannot approve it; another can.
	_, err = s.Approve(ctx, p.ID, adminHK, audit.Event{})
	assert.ErrorIs(t, err, ErrSameAdmin)
	approved, err := s.Approve(ctx, p.ID, secondAdminHK, audit.Event{IP: "203.0.113.7"})
	require.NoError(t, err)
	assert.True(t, approved.InForce)
	assert.Equal(t, secondAdminHK, approved.ApprovedBy)
	active, ok = served("BTC/USDT")
	require.True(t, ok)
	assert.True(t, active.Approved)
	require.NoError(t, conn.QueryRow(`SELECT count(*) FROM audit_events WHERE action = $1 AND target_id = $2 AND actor_id = $3`,
		audit.ActionPriceOverrideApprove, p.ID, secondAdminHK).Scan(&n))
	assert.Equal(t, 1, n)
	_, err = s.Approve(ctx, p.ID, secondAdminHK, audit.Event{})
	assert.ErrorIs(t, err, ErrNotPending)

	// The database refuses an approval by the same administrator.
	_, err = conn.Exec(`UPDATE price_overrides SET approved_by = created_by WHERE id = $1`, p.ID)
	assert.Error(t, err)

	// Without a live reference it waits too, and lapses unapproved.
	none := within
	none.Market, none.Reference = "ETH/USDT", nil
	l, err := s.Set(ctx, none, adminHK, audit.Event{})
	require.NoError(t, err)
	assert.True(t, l.Pending)
	assert.Nil(t, l.ReferenceMid)
	time.Sleep(1100 * time.Millisecond)
	_, err = s.Approve(ctx, l.ID, secondAdminHK, audit.Event{})
	assert.ErrorIs(t, err, ErrLapsed)
	_, ok = served("ETH/USDT")
	assert.False(t, ok)
	all, err := s.List(ctx, false, 10)
	require.NoError(t, err)
	for _, o := range all {
		if o.ID == l.ID {
			assert.True(t, o.Lapsed)
		}
	}
	// A lapsed override is closed by the next one of its market.
	next, err := s.Set(ctx, within.with("ETH/USDT"), adminHK, audit.Event{})
	require.NoError(t, err)
	assert.True(t, next.InForce)

	// A pending override can be ended.
	pending, err := s.Set(ctx, beyond.with("XRP/USDT"), adminHK, audit.Event{})
	require.NoError(t, err)
	ended, err := s.End(ctx, pending.ID, adminHK, audit.Event{})
	require.NoError(t, err)
	assert.NotNil(t, ended.EndedAt)
	_, err = s.Approve(ctx, pending.ID, secondAdminHK, audit.Event{})
	assert.ErrorIs(t, err, ErrEnded)
}

func (n New) with(market string) New {
	n.Market = market
	return n
}
