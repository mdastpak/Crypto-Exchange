package source

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newLimiter(t *testing.T, db *sql.DB, perMinute, perMonth int) (*UsageLimiter, *noSleep) {
	t.Helper()
	l, ok := NewUsageLimiter(db, "coingecko", perMinute, perMonth).(*UsageLimiter)
	require.True(t, ok)
	n := &noSleep{}
	l.sleep = func(ctx context.Context, d time.Duration) error {
		n.waits = append(n.waits, d)
		// The next minute begins.
		_, err := db.Exec(`UPDATE market_source_usage SET window_start = window_start - interval '1 minute'
			WHERE source = 'coingecko' AND window_kind = 'minute'`)
		return err
	}
	return l, n
}

func TestUsageLimiter_CountsTheMinuteAcrossWorkers(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_marketdata_source")
	ctx := context.Background()
	a, waitedA := newLimiter(t, db, 2, 0)
	b, waitedB := newLimiter(t, db, 2, 0)

	require.NoError(t, a.Acquire(ctx))
	require.NoError(t, b.Acquire(ctx))
	assert.Empty(t, waitedA.waits)
	assert.Empty(t, waitedB.waits)

	// The third request of the minute, from either worker, waits for the
	// next one.
	require.NoError(t, a.Acquire(ctx))
	require.Len(t, waitedA.waits, 1)
	assert.LessOrEqual(t, waitedA.waits[0], 61*time.Second)

	var requests int
	require.NoError(t, db.QueryRow(`SELECT requests FROM market_source_usage WHERE source = 'coingecko' AND window_kind = 'minute'`).Scan(&requests))
	assert.Equal(t, 1, requests, "the new minute counts from one")
}

func TestUsageLimiter_StopsAtTheMonthlyBudget(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_marketdata_source")
	ctx := context.Background()
	l, _ := newLimiter(t, db, 0, 3)
	for range 3 {
		require.NoError(t, l.Acquire(ctx))
	}
	assert.ErrorIs(t, l.Acquire(ctx), ErrBudgetExhausted)

	// A new month starts the count again.
	_, err := db.Exec(`UPDATE market_source_usage SET window_start = window_start - interval '1 month' WHERE window_kind = 'month'`)
	require.NoError(t, err)
	assert.NoError(t, l.Acquire(ctx))
}

func TestNewUsageLimiter_NoLimitIsNoLimiter(t *testing.T) {
	assert.Nil(t, NewUsageLimiter(nil, "coingecko", 0, 0))
}
