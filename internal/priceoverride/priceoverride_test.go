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
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"
)

const adminHK = "5f0c9b4e-2d1a-4c3b-9e8f-7a6b5c4d3e2f"

func d(s string) shared.Decimal { return shared.MustNewDecimalFromString(s) }

func newOverride(market string, lasts time.Duration) New {
	return New{Market: market, Bid: d("64000"), Ask: d("64100"), Mid: d("64050"), Reason: "Binance and Kraken are down", For: lasts}
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
