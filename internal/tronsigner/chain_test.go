package tronsigner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/tron"
)

// The scenario of #550: the network took the payment, and the signer's
// node lags 15 minutes, so it finds nothing. Past the expiration by the
// signer's clock, the payment stays pending and nothing is signed again.
func TestSigner_ALaggingNodeNeverLeadsToASecondPayment(t *testing.T) {
	ctx := context.Background()
	node := newNode()
	c := &clock{at: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	s := newSigner(t, testConfig(t), node, c)
	req := Request{Key: "w-4000", To: customer, Asset: "USDT", Amount: "4000000000"}
	_, err := s.Pay(ctx, req)
	require.NoError(t, err)
	require.Len(t, node.sent, 1)

	node.lag = 15 * time.Minute
	c.at = c.at.Add(11*time.Minute + time.Second)
	st, err := s.Status(ctx, req.Key)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, st.Status, "a stale node decides nothing")
	again, err := s.Pay(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, again.Status)
	assert.Len(t, node.sent, 1, "nothing signed or sent again")
}

// A head that is fresh but a solidified chain not yet past the expiration
// decides nothing either: the expiration is the chain's, not the clock's.
func TestSigner_TheSolidHeadDecidesTheExpiration(t *testing.T) {
	ctx := context.Background()
	node := newNode()
	c := &clock{at: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	s := newSigner(t, testConfig(t), node, c)
	req := Request{Key: "k", To: customer, Asset: "TRX", Amount: "5"}
	_, err := s.Pay(ctx, req)
	require.NoError(t, err)

	// 30 s behind is not stale, but the solid head is 84 s behind the
	// clock: at expiration + 1m + 30s it is not past expiration + 1m.
	node.lag = 30 * time.Second
	c.at = c.at.Add(tron.Lifetime + time.Minute + 30*time.Second)
	st, err := s.Status(ctx, req.Key)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, st.Status)
	c.at = c.at.Add(2 * time.Minute)
	st, err = s.Status(ctx, req.Key)
	require.NoError(t, err)
	assert.Equal(t, StatusNotReceived, st.Status)
}

// A payment signed again whose first transaction the chain took after
// all completes by that one, and a third call signs nothing.
func TestSigner_AnEarlierTransactionStillCompletesThePayment(t *testing.T) {
	ctx := context.Background()
	node := newNode()
	c := &clock{at: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	s := newSigner(t, testConfig(t), node, c)
	req := Request{Key: "k", To: customer, Asset: "TRX", Amount: "5"}
	first, err := s.Pay(ctx, req)
	require.NoError(t, err)

	c.at = c.at.Add(tron.Lifetime + 3*time.Minute)
	second, err := s.Pay(ctx, req)
	require.NoError(t, err)
	require.NotEqual(t, first.TxID, second.TxID, "expired by the solid chain: signed again")
	require.Len(t, node.sent, 2)
	assert.Equal(t, []string{first.TxID}, s.state.Payments["k"].PreviousTxIDs, "the first is kept")

	node.mine(first.TxID)
	node.advance(18)
	st, err := s.Status(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, st.Status)
	assert.Equal(t, first.TxID, st.TxID, "completed by the first")
	third, err := s.Pay(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, third.Status)
	assert.Len(t, node.sent, 2, "no third transaction")

	// The record survives a restart with both.
	restarted := newSigner(t, s.cfg, node, c)
	st, err = restarted.Status(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, first.TxID, st.TxID)
}

// The sweeps follow the same rules: a top-up is not sent again on a
// lagging node, and an earlier top-up taken after all still counts.
func TestSweep_TopUpOnALaggingNode(t *testing.T) {
	ctx := context.Background()
	node := newNode()
	c := &clock{at: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	s := newSigner(t, sweepConfig(t), node, c)
	from := depositAddress(t, s, 7)
	node.setToken(usdtContract, from, 25_000_000)
	req := SweepRequest{Key: "sweep-7", Index: 7, Asset: usdtContract}
	sw, err := s.Sweep(ctx, req)
	require.NoError(t, err)
	firstTopUp := sw.TopUpTxID
	require.Len(t, node.sent, 1)

	node.lag = 15 * time.Minute
	c.at = c.at.Add(tron.Lifetime + 3*time.Minute)
	sw, err = s.Sweep(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, firstTopUp, sw.TopUpTxID)
	assert.Len(t, node.sent, 1, "no second top-up on a stale node")

	// Fresh again, and expired by the solid chain: topped up again, the
	// first kept; it then lands after all, and the sweep goes on.
	node.lag = 0
	sw, err = s.Sweep(ctx, req)
	require.NoError(t, err)
	require.NotEqual(t, firstTopUp, sw.TopUpTxID)
	assert.Equal(t, []string{firstTopUp}, s.state.Sweeps["sweep-7"].PriorTopUps)
	node.mine(firstTopUp)
	node.advance(18)
	node.setTRX(from, 30_000_000)
	sw, err = s.Sweep(ctx, req)
	require.NoError(t, err)
	assert.NotEmpty(t, sw.TxID, "the first top-up counts: the sweep is sent")
	assert.Len(t, node.sent, 3, "two top-ups, one sweep, nothing more")
}

func TestConfig_StaleAfterDefaults(t *testing.T) {
	c := testConfig(t)
	assert.Equal(t, 60, c.StaleAfterSeconds)
	assert.Equal(t, time.Minute, c.staleAfter())
}
