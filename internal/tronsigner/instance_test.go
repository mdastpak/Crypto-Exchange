package tronsigner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A second signer on the same state refuses to start; the first keeps
// paying, and once it stops, another may start (#551).
func TestNew_RefusesASecondInstance(t *testing.T) {
	cfg := testConfig(t)
	node := newNode()
	first, err := New(cfg, node, seed, nil)
	require.NoError(t, err)
	_, err = New(cfg, node, seed, nil)
	require.ErrorIs(t, err, ErrRunning)

	_, err = first.Pay(context.Background(), Request{Key: "k", To: customer, Asset: "TRX", Amount: "5"})
	require.NoError(t, err, "the first keeps working")

	require.NoError(t, first.Close())
	again, err := New(cfg, node, seed, nil)
	require.NoError(t, err, "free once the first stops")
	assert.Contains(t, again.state.Payments, "k", "with the first's state")
	require.NoError(t, again.Close())
}

// The seed and the Vault token, the TronGrid key and the transactions
// never cross plain HTTP but to the loopback (#551).
func TestConfig_RefusesPlainHTTP(t *testing.T) {
	for _, bad := range []string{"http://vault.internal:8200", "http://10.0.0.5", "ftp://vault", "vault:8200", ""} {
		c := testConfig(t)
		c.Seed.VaultAddr = bad
		assert.ErrorContains(t, c.Check(), "seed.vault_addr", bad)
		c = testConfig(t)
		c.NodeURL = bad
		assert.ErrorContains(t, c.Check(), "node_url", bad)
	}
	for _, good := range []string{"https://vault.internal:8200", "http://127.0.0.1:8200", "http://localhost:8090", "http://[::1]:8200"} {
		c := testConfig(t)
		c.Seed.VaultAddr, c.NodeURL = good, good
		assert.NoError(t, c.Check(), good)
	}
}

// With vault_ca_file, Vault's certificate must be signed by that CA: one
// of another CA is refused before the token is sent.
func TestVaultClient_PinsTheCA(t *testing.T) {
	p := newPKI(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"plaintext": base64.StdEncoding.EncodeToString(seed)}})
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{p.leaf(t, "vault", x509.ExtKeyUsageServerAuth)}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	cfg := testConfig(t)
	dir := t.TempDir()
	cfg.Seed.VaultAddr = srv.URL
	cfg.Seed.CiphertextFile = filepath.Join(dir, "seed.enc")
	require.NoError(t, os.WriteFile(cfg.Seed.CiphertextFile, []byte("vault:v1:abc"), 0o600))
	cfg.Seed.VaultCAFile = filepath.Join(dir, "vault-ca.pem")
	require.NoError(t, os.WriteFile(cfg.Seed.VaultCAFile, p.caPEM, 0o600))
	client, err := VaultClient(cfg)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := UnwrapSeed(ctx, client, cfg, "s.token")
	require.NoError(t, err)
	assert.Equal(t, seed, got)

	other := newPKI(t)
	require.NoError(t, os.WriteFile(cfg.Seed.VaultCAFile, other.caPEM, 0o600))
	client, err = VaultClient(cfg)
	require.NoError(t, err)
	_, err = UnwrapSeed(ctx, client, cfg, "s.token")
	assert.Error(t, err, "a certificate of another CA")

	require.NoError(t, os.WriteFile(cfg.Seed.VaultCAFile, []byte("not a pem"), 0o600))
	_, err = VaultClient(cfg)
	assert.Error(t, err)
}
