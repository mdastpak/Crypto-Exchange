package tron

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestShasta reads the Shasta testnet: the head, a block of transfers, and
// a transaction of it in the canonical chain (#68). It runs only with
// TRON_SHASTA_URL set, such as https://api.shasta.trongrid.io, as CI has no
// network to the testnet.
func TestShasta(t *testing.T) {
	url := os.Getenv("TRON_SHASTA_URL")
	if url == "" {
		t.Skip("TRON_SHASTA_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	n := Node{BaseURL: url, APIKey: os.Getenv("TRON_SHASTA_API_KEY")}
	head, err := n.Head(ctx)
	require.NoError(t, err)
	require.Positive(t, head.Number)
	assert.WithinDuration(t, time.Now(), head.Time, 5*time.Minute, "the head's time is the chain's")
	solid, err := n.SolidHead(ctx)
	require.NoError(t, err)
	assert.Less(t, solid.Number, head.Number+1)
	assert.False(t, solid.Time.After(head.Time), "the solid head is not ahead of the head")
	var found *Transfer
	for number := solid.Number - 1; number > solid.Number-200 && found == nil; number-- {
		b, err := n.Block(ctx, number)
		require.NoError(t, err)
		next, err := n.Block(ctx, number+1)
		require.NoError(t, err)
		assert.Equal(t, b.ID, next.ParentID, "the chain links by parent")
		for i := range b.Transfers {
			found = &b.Transfers[i]
			at, ok, err := n.TxBlock(ctx, found.TxID)
			require.NoError(t, err)
			assert.True(t, ok)
			assert.Equal(t, number, at)
			break
		}
	}
	if found == nil {
		t.Skip("no transfer in the last 200 blocks of Shasta")
	}
	assert.Positive(t, found.Amount.Sign())
	receipt, err := n.Receipt(ctx, found.TxID)
	require.NoError(t, err)
	assert.True(t, receipt.Found)
	solidReceipt, err := n.SolidReceipt(ctx, found.TxID)
	require.NoError(t, err)
	assert.True(t, solidReceipt.Found, "below the solid head, it is solidified")
	assert.Equal(t, receipt.Block, solidReceipt.Block)
	assert.True(t, receipt.Success, "only successful transfers are read")
	balance, err := n.Balance(ctx, found.To)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, balance.Sign(), 0)
	if found.Token != (Address{}) {
		tokens, err := n.TokenBalance(ctx, found.Token, found.To)
		require.NoError(t, err)
		t.Logf("%s holds %s of %s", found.To, tokens, found.Token)
	}
	t.Logf("transfer %s: %s from %s to %s (token %s)", found.TxID, found.Amount, found.From, found.To, found.Token)
}
