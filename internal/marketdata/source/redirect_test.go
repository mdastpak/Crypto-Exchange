package source

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/testutil/redirecttest"
)

// The key of a source is a custom header, which Go's default policy sends
// to any host, even over plain http (#420).
func TestClient_FollowsNoRedirect(t *testing.T) {
	trap := redirecttest.New(t, true)
	c, _ := newTestClient(t, trap.URL, ClientOptions{Header: http.Header{"X-Api-Key": {"secret-key"}},
		Transport: trap.Origin.Client().Transport})
	_, err := c.Get(context.Background(), "/coins/list", nil)
	require.Error(t, err)
	assert.False(t, trap.Reached())
}
