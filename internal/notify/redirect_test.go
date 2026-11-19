package notify

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/testutil/redirecttest"
)

// The API key of SMS.ir is a custom header, and Kavenegar's is in the
// path: a followed redirect would send either to the new host.
func TestSMSProviders_FollowNoRedirect(t *testing.T) {
	t.Run("smsir", func(t *testing.T) {
		trap := redirecttest.New(t, false)
		_, err := newTestSMSIR(t, trap.URL).Send(context.Background(), testSMSMessage)
		require.Error(t, err)
		assert.False(t, trap.Reached())
	})
	t.Run("kavenegar", func(t *testing.T) {
		trap := redirecttest.New(t, false)
		_, err := newTestKavenegar(t, trap.URL).Send(context.Background(), testSMSMessage)
		require.Error(t, err)
		assert.False(t, trap.Reached())
	})
}
