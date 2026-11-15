package convhold_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/convhold"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackoff(t *testing.T) {
	assert.Equal(t, time.Minute, convhold.Backoff(1))
	assert.Equal(t, 2*time.Minute, convhold.Backoff(2))
	assert.Equal(t, 32*time.Minute, convhold.Backoff(6))
	assert.Equal(t, time.Hour, convhold.Backoff(7))
	assert.Equal(t, time.Hour, convhold.Backoff(100))
}

func TestCode(t *testing.T) {
	assert.Equal(t, convhold.CodeRowUnverified, convhold.Code(fmt.Errorf("balance x: %w", rowseal.ErrUnverified)))
	assert.Equal(t, convhold.CodeInvalidRequest, convhold.Code(fmt.Errorf("%w: sub-account", ledger.ErrInvalidRequest)))
	assert.Equal(t, convhold.CodeInsufficientBalance, convhold.Code(ledger.ErrInsufficientBalance))
	assert.Equal(t, convhold.CodeFailed, convhold.Code(errors.New("alice@example.com is not known")), "never the text")
}

// TestHold_BacksOffAndReopens holds a balance twice, which doubles its
// wait, releases it, and holds it again, which starts the backoff over.
func TestHold_BacksOffAndReopens(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_convhold")
	ctx := context.Background()
	ref, balance, currency := uuid.NewString(), uuid.NewString(), uuid.NewString()
	wait := func() time.Duration {
		t.Helper()
		var secs float64
		require.NoError(t, db.QueryRow(`SELECT extract(epoch FROM retry_after - now()) FROM currency_conversion_holds WHERE balance_id = $1`,
			balance).Scan(&secs))
		return time.Duration(secs * float64(time.Second))
	}
	n, err := convhold.Hold(ctx, db, convhold.KindMigration, ref, balance, currency, rowseal.ErrUnverified)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.InDelta(t, time.Minute.Seconds(), wait().Seconds(), 5)
	n, err = convhold.Hold(ctx, db, convhold.KindMigration, ref, balance, currency, rowseal.ErrUnverified)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.InDelta(t, convhold.Backoff(2).Seconds(), wait().Seconds(), 5)
	for range 10 {
		_, err = convhold.Hold(ctx, db, convhold.KindMigration, ref, balance, currency, errors.New("x"))
		require.NoError(t, err)
	}
	assert.InDelta(t, time.Hour.Seconds(), wait().Seconds(), 5, "at most an hour")

	blocked, err := convhold.Summary(ctx, db, convhold.KindMigration, ref)
	require.NoError(t, err)
	assert.Equal(t, map[string]convhold.Blocked{currency: {Balances: 1, Codes: map[string]int{convhold.CodeFailed: 1}}}, blocked)

	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, convhold.Release(ctx, tx, convhold.KindMigration, ref, balance))
	require.NoError(t, tx.Commit())
	blocked, err = convhold.Summary(ctx, db, convhold.KindMigration, ref)
	require.NoError(t, err)
	assert.Empty(t, blocked)
	n, err = convhold.Hold(ctx, db, convhold.KindMigration, ref, balance, currency, rowseal.ErrUnverified)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "a hold released starts over")
	assert.InDelta(t, time.Minute.Seconds(), wait().Seconds(), 5)
}
