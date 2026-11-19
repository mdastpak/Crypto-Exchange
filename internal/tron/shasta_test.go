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
	var found *Transfer
	for number := head.Number - 1; number > head.Number-200 && found == nil; number-- {
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
	t.Logf("transfer %s: %s from %s to %s (token %s)", found.TxID, found.Amount, found.From, found.To, found.Token)
}
