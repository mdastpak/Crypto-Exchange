package tronsigner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/testutil/redirecttest"
	"github.com/mdastpak/crypto-exchange/internal/withdrawals"
)

// The client certificate is the server's credential at the signer: the
// client follows no redirect, which could present it to another host.
func TestClient_FollowsNoRedirect(t *testing.T) {
	trap := redirecttest.New(t, true)
	pool := x509.NewCertPool()
	pool.AddCert(trap.Origin.Certificate())
	client := NewClient(trap.URL, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	_, err := client.Status(context.Background(), withdrawals.Transfer{PayoutID: "p1", IdempotencyKey: "w1-1"})
	require.Error(t, err)
	assert.False(t, trap.Reached())
}
