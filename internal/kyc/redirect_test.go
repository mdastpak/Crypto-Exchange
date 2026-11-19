package kyc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/testutil/redirecttest"
)

// The app token is a custom header; with the default client, Sumsub
// follows no redirect.
func TestSumsub_FollowsNoRedirect(t *testing.T) {
	trap := redirecttest.New(t, false)
	s := &Sumsub{BaseURL: trap.URL, AppToken: appToken, SecretKey: secretKey, WebhookSecret: webhookSecret, Level: "basic-kyc-level"}
	_, err := s.Start(context.Background(), "app-1")
	require.Error(t, err)
	assert.False(t, trap.Reached())
}
