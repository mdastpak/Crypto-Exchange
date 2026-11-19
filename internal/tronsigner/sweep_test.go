package tronsigner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/tron"
)

const coldStorage = "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL"

func sweepConfig(t *testing.T) *Config {
	t.Helper()
	c := testConfig(t)
	c.Sweeps.ColdAddress = coldStorage
	c.Sweeps.TopUpSun = 30_000_000
	c.Sweeps.ReserveSun = 1_000_000
	c.Assets[1].HotCap = "100000000" // 100 USDT
	require.NoError(t, c.Check())
	return c
}

// depositAddress is the address of index i, as the server derives it from
// the xpub.
func depositAddress(t *testing.T, s *Signer, i uint32) string {
	t.Helper()
	x, err := tron.ParseXPub(s.DepositXPub())
	require.NoError(t, err)
	a, err := x.DepositAddress(i)
	require.NoError(t, err)
	return a.String()
}

// A token sweep: the hot wallet sends the fee's TRX, then the address
// sends its whole balance to the hot wallet, each once final (#68).
func TestSweep_TokenWithTopUp(t *testing.T) {
	ctx := context.Background()
	node := newNode()
	c := &clock{at: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	s := newSigner(t, sweepConfig(t), node, c)
	from := depositAddress(t, s, 7)
	node.setToken(usdtContract, from, 25_000_000)
	node.setTRX(from, 2_000_000)
	req := SweepRequest{Key: "sweep-1", Index: 7, Asset: usdtContract}

	sw, err := s.Sweep(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, sw.Status)
	assert.Equal(t, from, sw.Address)
	require.NotEmpty(t, sw.TopUpTxID)
	require.Len(t, node.sent, 1)
	signer, _ := node.sent[0].Signer()
	assert.Equal(t, s.HotWallet(), signer.String(), "the hot wallet pays the fee")

	again, err := s.Sweep(ctx, req)
	require.NoError(t, err)
	assert.Empty(t, again.TxID, "waits for the top-up")

	node.mine(sw.TopUpTxID)
	node.advance(18)
	node.setTRX(from, 30_000_000)
	sw, err = s.Sweep(ctx, req)
	require.NoError(t, err)
	require.NotEmpty(t, sw.TxID)
	assert.Equal(t, "25000000", sw.Amount)
	assert.Equal(t, s.HotWallet(), sw.To, "below the hot cap")
	signer, _ = node.sent[len(node.sent)-1].Signer()
	assert.Equal(t, from, signer.String(), "the deposit address signs its sweep")

	node.mine(sw.TxID)
	node.advance(18)
	sw, err = s.Sweep(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, sw.Status)
	sent := len(node.sent)
	sw, err = s.Sweep(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, sw.Status)
	assert.Len(t, node.sent, sent, "nothing more")

	_, err = s.Sweep(ctx, SweepRequest{Key: "sweep-1", Index: 8, Asset: usdtContract})
	assert.ErrorIs(t, err, ErrKeyReused)
	st, err := s.SweepStatus("sweep-1")
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, st.Status)
}

// Above the hot wallet's cap, a sweep goes to the cold storage; a TRX
// sweep leaves the reserve; an empty address moves nothing.
func TestSweep_ColdTRXAndEmpty(t *testing.T) {
	ctx := context.Background()
	node := newNode()
	c := &clock{at: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	s := newSigner(t, sweepConfig(t), node, c)
	from := depositAddress(t, s, 1)
	node.setToken(usdtContract, s.HotWallet(), 90_000_000)
	node.setToken(usdtContract, from, 20_000_000)
	node.setTRX(from, 40_000_000)
	sw, err := s.Sweep(ctx, SweepRequest{Key: "cold", Index: 1, Asset: "USDT"})
	require.NoError(t, err)
	assert.Empty(t, sw.TopUpTxID, "the address holds the fee")
	assert.Equal(t, coldStorage, sw.To, "the hot wallet would hold 110 USDT")

	trx, err := s.Sweep(ctx, SweepRequest{Key: "trx", Index: 1, Asset: "TRX"})
	require.NoError(t, err)
	assert.Equal(t, "39000000", trx.Amount, "less the reserve")
	assert.Equal(t, s.HotWallet(), trx.To, "TRX has no cap")

	empty, err := s.Sweep(ctx, SweepRequest{Key: "empty", Index: 2, Asset: "USDT"})
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, empty.Status)
	assert.Equal(t, "0", empty.Amount)

	for _, bad := range []SweepRequest{
		{Key: "x1", Index: tron.Hardened, Asset: "TRX"},
		{Key: "x2", Index: 1, Asset: "BTC"},
		{Key: "bad key", Index: 1, Asset: "TRX"},
	} {
		_, err := s.Sweep(ctx, bad)
		assert.ErrorIs(t, err, ErrRefused, bad.Key)
	}
}

// A sweep that failed on chain fails; one whose transaction expired
// untaken is built again from the balance; the state survives a restart.
func TestSweep_FailedExpiredAndRestart(t *testing.T) {
	ctx := context.Background()
	cfg := sweepConfig(t)
	node := newNode()
	c := &clock{at: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	s := newSigner(t, cfg, node, c)
	from := depositAddress(t, s, 3)
	node.setTRX(from, 10_000_000)
	sw, err := s.Sweep(ctx, SweepRequest{Key: "a", Index: 3, Asset: "TRX"})
	require.NoError(t, err)
	first := sw.TxID

	c.at = c.at.Add(tron.Lifetime + 2*time.Minute)
	restarted := newSigner(t, cfg, node, c)
	sw, err = restarted.Sweep(ctx, SweepRequest{Key: "a", Index: 3, Asset: "TRX"})
	require.NoError(t, err)
	assert.NotEqual(t, first, sw.TxID, "signed again after it expired")
	assert.Equal(t, StatusPending, sw.Status)

	node.mine(sw.TxID)
	node.failed[sw.TxID] = "FAILED"
	sw, err = restarted.Sweep(ctx, SweepRequest{Key: "a", Index: 3, Asset: "TRX"})
	require.NoError(t, err)
	assert.Equal(t, StatusFailed, sw.Status)
	assert.Contains(t, sw.Reason, "FAILED")
}

// The cold storage is required with a hot cap.
func TestConfig_HotCapNeedsCold(t *testing.T) {
	c := testConfig(t)
	c.Assets[1].HotCap = "100"
	assert.ErrorContains(t, c.Check(), "cold_address")
}
