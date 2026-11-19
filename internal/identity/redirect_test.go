package identity

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/testutil/redirecttest"
)

// The keys go in the body of the token request and the token in
// Authorization; neither may reach the target of a redirect.
func TestJibit_FollowsNoRedirect(t *testing.T) {
	trap := redirecttest.New(t, false)
	j := NewJibit(trap.URL+"/", "key", "secret", 2*time.Second)
	j.retryDelay = time.Millisecond
	_, err := j.Match(context.Background(), testNationalID, testMobile)
	require.Error(t, err)
	assert.False(t, trap.Reached())
}
