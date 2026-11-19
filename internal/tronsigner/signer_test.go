package tronsigner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/tron"
)

var seed, _ = hex.DecodeString("000102030405060708090a0b0c0d0e0f")

const (
	usdtContract = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	customer     = "TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7"
)

// fakeNode is a chain that takes the broadcast transactions into blocks
// when the test says.
type fakeNode struct {
	mu        sync.Mutex
	head      int64
	sent      []tron.Tx
	inBlock   map[string]int64
	failed    map[string]string
	refuseAll bool
}

func newNode() *fakeNode {
	return &fakeNode{head: 1000, inBlock: map[string]int64{}, failed: map[string]string{}}
}

func (n *fakeNode) Head(context.Context) (tron.Block, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return tron.Block{Number: n.head, ID: "00000000000003e8" + strings.Repeat("ab", 24)}, nil
}

func (n *fakeNode) Receipt(_ context.Context, id string) (tron.Receipt, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	b, ok := n.inBlock[id]
	if !ok {
		return tron.Receipt{}, nil
	}
	if r, bad := n.failed[id]; bad {
		return tron.Receipt{Found: true, Block: b, Result: r}, nil
	}
	return tron.Receipt{Found: true, Block: b, Success: true}, nil
}

func (n *fakeNode) Broadcast(_ context.Context, t tron.Tx) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.refuseAll {
		return errors.New("node down")
	}
	n.sent = append(n.sent, t)
	return nil
}

// mine puts the transaction in the next block.
func (n *fakeNode) mine(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.head++
	n.inBlock[id] = n.head
}

func (n *fakeNode) advance(blocks int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.head += blocks
}

func testConfig(t *testing.T) *Config {
	t.Helper()
	c := &Config{Listen: ":0", NodeURL: "http://node", StatePath: filepath.Join(t.TempDir(), "state.json"), FeeLimitSun: 30_000_000,
		Assets: []Asset{
			{Symbol: "TRX", MaxPerTransfer: "1000000000", MaxPerDay: "1500000000"},
			{Symbol: "USDT", Contract: usdtContract, MaxPerTransfer: "5000000000", MaxPerDay: "8000000000"},
		}}
	c.TLS.CertFile, c.TLS.KeyFile, c.TLS.ClientCAFile = "c", "k", "ca"
	c.TLS.AllowedClients = []string{"exchange-server"}
	c.Seed.VaultAddr, c.Seed.TransitKey, c.Seed.CiphertextFile = "http://vault", "tron-seed", "seed.enc"
	require.NoError(t, c.Check())
	return c
}

type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

func newSigner(t *testing.T, cfg *Config, node Node, c *clock) *Signer {
	t.Helper()
	s, err := New(cfg, node, seed, c.now)
	require.NoError(t, err)
	return s
}

// The hot wallet is the first address of account 1, which the server's
// xpub of account 0 cannot derive.
func TestSigner_HotWallet(t *testing.T) {
	s := newSigner(t, testConfig(t), newNode(), &clock{at: time.Now()})
	m, _ := tron.MasterKey(seed)
	acct, _ := m.Account(tron.HotAccount)
	k, _ := acct.AddressKey(0)
	assert.Equal(t, tron.AddressOf(k.PubKey()).String(), s.HotWallet())
	deposits, _ := m.Account(tron.DepositAccount)
	first, _ := deposits.Public().DepositAddress(0)
	assert.NotEqual(t, first.String(), s.HotWallet())
	server, err := tron.ParseXPub(s.DepositXPub())
	require.NoError(t, err)
	got, err := server.DepositAddress(0)
	require.NoError(t, err)
	assert.Equal(t, first, got, "the server derives the signer's deposit addresses")
}

// A payment is signed by the hot wallet and broadcast once by its key; it
// completes after 19 confirmations.
func TestSigner_PaysOnceByKey(t *testing.T) {
	ctx := context.Background()
	node := newNode()
	c := &clock{at: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	s := newSigner(t, testConfig(t), node, c)
	req := Request{Key: "payout-1", To: customer, Asset: "USDT", Amount: "25000000"}
	p, err := s.Pay(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, p.Status)
	require.Len(t, node.sent, 1)
	assert.Equal(t, p.TxID, node.sent[0].ID())
	signer, err := node.sent[0].Signer()
	require.NoError(t, err)
	assert.Equal(t, s.HotWallet(), signer.String())

	again, err := s.Pay(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, p.TxID, again.TxID, "the same transaction")
	assert.Len(t, node.sent, 2, "broadcast again while the network may take it")
	_, err = s.Pay(ctx, Request{Key: "payout-1", To: customer, Asset: "USDT", Amount: "26000000"})
	assert.ErrorIs(t, err, ErrKeyReused)

	node.mine(p.TxID)
	st, err := s.Status(ctx, "payout-1")
	require.NoError(t, err)
	assert.Equal(t, StatusPending, st.Status, "one confirmation")
	_, err = s.Pay(ctx, req)
	require.NoError(t, err)
	assert.Len(t, node.sent, 2, "not sent again once a block has it")
	node.advance(18)
	st, err = s.Status(ctx, "payout-1")
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, st.Status)
	_, err = s.Status(ctx, "payout-2")
	assert.ErrorIs(t, err, ErrNotFound)
}

// The caps of one payment and of the day, the assets it pays, and the
// addresses it pays to.
func TestSigner_Refusals(t *testing.T) {
	ctx := context.Background()
	node := newNode()
	c := &clock{at: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	s := newSigner(t, testConfig(t), node, c)
	for _, r := range []Request{
		{Key: "a", To: customer, Asset: "BTC", Amount: "1"},
		{Key: "b", To: "0x7e5f4552091a69125d5dfcb7b8c2659029395bdf", Asset: "TRX", Amount: "1"},
		{Key: "c", To: s.HotWallet(), Asset: "TRX", Amount: "1"},
		{Key: "d", To: usdtContract, Asset: "USDT", Amount: "1"},
		{Key: "e", To: customer, Asset: "TRX", Amount: "-5"},
		{Key: "f", To: customer, Asset: "TRX", Amount: "01"},
		{Key: "g", To: customer, Asset: "TRX", Amount: "1000000001"},
		{Key: "bad key!", To: customer, Asset: "TRX", Amount: "1"},
	} {
		_, err := s.Pay(ctx, r)
		assert.ErrorIs(t, err, ErrRefused, r.Key)
	}
	_, err := s.Pay(ctx, Request{Key: "day-1", To: customer, Asset: "TRX", Amount: "1000000000"})
	require.NoError(t, err)
	_, err = s.Pay(ctx, Request{Key: "day-2", To: customer, Asset: "TRX", Amount: "600000000"})
	assert.ErrorIs(t, err, ErrRefused, "above the day's cap")
	_, err = s.Pay(ctx, Request{Key: "day-3", To: customer, Asset: "TRX", Amount: "500000000"})
	require.NoError(t, err, "within it")
	c.at = c.at.Add(24 * time.Hour)
	_, err = s.Pay(ctx, Request{Key: "day-4", To: customer, Asset: "TRX", Amount: "600000000"})
	require.NoError(t, err, "the next day")
	assert.Len(t, node.sent, 3)
}

// A transaction that expired untaken is not received, and the same key
// signs a new one; one that failed on chain is failed.
func TestSigner_ExpiredAndFailed(t *testing.T) {
	ctx := context.Background()
	node := newNode()
	c := &clock{at: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	s := newSigner(t, testConfig(t), node, c)
	req := Request{Key: "k1", To: customer, Asset: "TRX", Amount: "1500000"}
	p, err := s.Pay(ctx, req)
	require.NoError(t, err)
	c.at = c.at.Add(tron.Lifetime + 2*time.Minute)
	st, err := s.Status(ctx, "k1")
	require.NoError(t, err)
	assert.Equal(t, StatusNotReceived, st.Status)
	again, err := s.Pay(ctx, req)
	require.NoError(t, err)
	assert.NotEqual(t, p.TxID, again.TxID, "signed again")
	assert.Equal(t, StatusPending, again.Status)

	other, err := s.Pay(ctx, Request{Key: "k2", To: customer, Asset: "USDT", Amount: "1"})
	require.NoError(t, err)
	node.mine(other.TxID)
	node.failed[other.TxID] = "OUT_OF_ENERGY"
	st, err = s.Status(ctx, "k2")
	require.NoError(t, err)
	assert.Equal(t, StatusFailed, st.Status)
	assert.Contains(t, st.Reason, "OUT_OF_ENERGY")
}

// The payments survive a restart: the state file, written before the
// broadcast, which may fail.
func TestSigner_StateSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	node := newNode()
	node.refuseAll = true
	c := &clock{at: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	s := newSigner(t, cfg, node, c)
	p, err := s.Pay(ctx, Request{Key: "k1", To: customer, Asset: "TRX", Amount: "1000000000"})
	require.Error(t, err)
	require.NotEmpty(t, p.TxID, "recorded before the broadcast")

	node.refuseAll = false
	restarted := newSigner(t, cfg, node, c)
	again, err := restarted.Pay(ctx, Request{Key: "k1", To: customer, Asset: "TRX", Amount: "1000000000"})
	require.NoError(t, err)
	assert.Equal(t, p.TxID, again.TxID)
	_, err = restarted.Pay(ctx, Request{Key: "k2", To: customer, Asset: "TRX", Amount: "600000000"})
	assert.ErrorIs(t, err, ErrRefused, "the day's cap counts the payment of before the restart")
	info, err := os.Stat(cfg.StatePath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestConfig_Check(t *testing.T) {
	c := testConfig(t)
	c.Assets = append(c.Assets, Asset{Symbol: "BAD", Contract: "nope", MaxPerTransfer: "10", MaxPerDay: "5"})
	err := c.Check()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "assets[2]")
	assert.Error(t, (&Config{}).Check())
}

type pki struct {
	caPEM      []byte
	ca         *x509.Certificate
	caKey      *ecdsa.PrivateKey
	serverCert tls.Certificate
}

func newPKI(t *testing.T) pki {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	p := pki{caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), ca: ca, caKey: key}
	p.serverCert = p.leaf(t, "tron-signer", x509.ExtKeyUsageServerAuth)
	return p
}

func (p pki) leaf(t *testing.T, cn string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		ExtKeyUsage: []x509.ExtKeyUsage{usage}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// Only a client with a certificate of the client CA, of an allowed name,
// reaches the signer.
func TestHandler_MutualTLS(t *testing.T) {
	cfg := testConfig(t)
	p := newPKI(t)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(p.caPEM)
	s := newSigner(t, cfg, newNode(), &clock{at: time.Now()})
	srv := httptest.NewUnstartedServer(Handler(s, cfg.TLS.AllowedClients))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{p.serverCert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	defer srv.Close()

	client := func(certs ...tls.Certificate) *http.Client {
		return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: certs,
			MinVersion: tls.VersionTLS13}}}
	}
	resp, err := client(p.leaf(t, "exchange-server", x509.ExtKeyUsageClientAuth)).Get(srv.URL + "/v1/hot-wallet")
	require.NoError(t, err)
	var out map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, s.HotWallet(), out["address"])

	resp, err = client(p.leaf(t, "someone-else", x509.ExtKeyUsageClientAuth)).Get(srv.URL + "/v1/hot-wallet")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "a name that is not allowed")

	_, err = client().Get(srv.URL + "/v1/hot-wallet")
	assert.Error(t, err, "no client certificate")

	stranger := newPKI(t)
	_, err = client(stranger.leaf(t, "exchange-server", x509.ExtKeyUsageClientAuth)).Get(srv.URL + "/v1/hot-wallet")
	assert.Error(t, err, "a certificate of another CA")

	ok := client(p.leaf(t, "exchange-server", x509.ExtKeyUsageClientAuth))
	resp, err = ok.Post(srv.URL+"/v1/payments", "application/json", strings.NewReader(`{"key":"k","to":"`+customer+`","asset":"TRX","amount":"5"}`))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp, err = ok.Post(srv.URL+"/v1/payments", "application/json", strings.NewReader(`{"key":"k2","to":"`+customer+`","asset":"BTC","amount":"5"}`))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	resp, err = ok.Post(srv.URL+"/v1/payments", "application/json", strings.NewReader(`{"key":"k3","extra":1}`))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp, err = ok.Get(srv.URL + "/v1/payments/k")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp, err = ok.Get(srv.URL + "/v1/payments/missing")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// The seed is decrypted by Vault's Transit engine, with the token.
func TestUnwrapSeed(t *testing.T) {
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "s.token" || r.URL.Path != "/v1/transit/decrypt/tron-seed" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["ciphertext"] != "vault:v1:abc" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"plaintext": base64.StdEncoding.EncodeToString(seed)}})
	}))
	defer vault.Close()
	cfg := testConfig(t)
	cfg.Seed.VaultAddr = vault.URL
	cfg.Seed.CiphertextFile = filepath.Join(t.TempDir(), "seed.enc")
	require.NoError(t, os.WriteFile(cfg.Seed.CiphertextFile, []byte("vault:v1:abc\n"), 0o600))
	got, err := UnwrapSeed(context.Background(), vault.Client(), cfg, "s.token")
	require.NoError(t, err)
	assert.Equal(t, seed, got)
	_, err = UnwrapSeed(context.Background(), vault.Client(), cfg, "wrong")
	assert.Error(t, err)
	_, err = UnwrapSeed(context.Background(), vault.Client(), cfg, "")
	assert.Error(t, err)
}
