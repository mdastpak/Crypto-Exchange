package tron

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/testutil/redirecttest"
)

// TRON-PRO-API-KEY is a custom header; with the default client, the node
// follows no redirect.
func TestNode_FollowsNoRedirect(t *testing.T) {
	trap := redirecttest.New(t, false)
	_, err := Node{BaseURL: trap.URL, APIKey: "key"}.Head(context.Background())
	require.Error(t, err)
	assert.False(t, trap.Reached())
}
