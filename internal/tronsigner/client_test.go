package tronsigner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/withdrawals"
)

// The server's client pays a withdrawal on TRON through the signer, over
// mutual TLS, and reads its state, as the payouts do.
func TestClient_PaysThroughTheSigner(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	p := newPKI(t)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(p.caPEM)
	node := newNode()
	c := &clock{at: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	s := newSigner(t, cfg, node, c)
	srv := httptest.NewUnstartedServer(Handler(s, cfg.TLS.AllowedClients))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{p.serverCert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	defer srv.Close()
	client := NewClient(srv.URL, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{p.leaf(t, "exchange-server", x509.ExtKeyUsageClientAuth)}})

	dest := destinations.Resolved{Value: customer}
	dest.Network = "tron"
	transfer := withdrawals.Transfer{PayoutID: "p1", IdempotencyKey: "w1-1", Rail: withdrawals.RailChain, Currency: "USDT",
		Amount: shared.MustNewDecimalFromString("25.5"), Destination: dest, Network: "tron", Contract: usdtContract, Decimals: 6}

	state, err := client.Status(ctx, transfer)
	require.NoError(t, err)
	assert.Equal(t, withdrawals.TransferNotReceived, state.Status, "the signer never got it")

	ref, err := client.Submit(ctx, transfer)
	require.NoError(t, err)
	require.Len(t, node.sent, 1)
	assert.Equal(t, ref, node.sent[0].ID())
	again, err := client.Submit(ctx, transfer)
	require.NoError(t, err)
	assert.Equal(t, ref, again, "once by its key")

	state, err = client.Status(ctx, transfer)
	require.NoError(t, err)
	assert.Equal(t, withdrawals.TransferPending, state.Status)
	node.mine(ref)
	node.advance(18)
	state, err = client.Status(ctx, transfer)
	require.NoError(t, err)
	assert.Equal(t, withdrawals.TransferCompleted, state.Status)
	assert.Equal(t, ref, state.TrackingID, "the hash on chain")

	trx := transfer
	trx.IdempotencyKey, trx.Contract, trx.Amount = "w2-1", "", shared.MustNewDecimalFromString("1.5")
	_, err = client.Submit(ctx, trx)
	require.NoError(t, err)

	for name, bad := range map[string]withdrawals.Transfer{
		"too many decimals": func() withdrawals.Transfer {
			b := transfer
			b.IdempotencyKey = "w3"
			b.Amount = shared.MustNewDecimalFromString("0.0000001")
			return b
		}(),
		"another network": func() withdrawals.Transfer { b := transfer; b.IdempotencyKey = "w4"; b.Network = "ethereum"; return b }(),
		"a fiat rail":     func() withdrawals.Transfer { b := transfer; b.IdempotencyKey = "w5"; b.Rail = "paya"; return b }(),
		"above the cap": func() withdrawals.Transfer {
			b := transfer
			b.IdempotencyKey = "w6"
			b.Amount = shared.MustNewDecimalFromString("6000")
			return b
		}(),
		"the key of another payment": func() withdrawals.Transfer {
			b := transfer
			b.Amount = shared.MustNewDecimalFromString("1")
			return b
		}(),
	} {
		_, err := client.Submit(ctx, bad)
		assert.ErrorIs(t, err, withdrawals.ErrTransferRefused, name)
	}

	// A signer that is not reached leaves the result unknown.
	down := NewClient("https://127.0.0.1:1", &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13})
	_, err = down.Submit(ctx, trx)
	require.Error(t, err)
	assert.NotErrorIs(t, err, withdrawals.ErrTransferRefused)

	// A client without the certificate is refused at the handshake.
	anonymous := Client{URL: srv.URL, HTTP: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool,
		MinVersion: tls.VersionTLS13}}}}
	_, err = anonymous.Status(ctx, transfer)
	assert.Error(t, err)
}
