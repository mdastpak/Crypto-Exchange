package keys

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTransit answers the Transit encrypt and decrypt endpoints of one key,
// with a reversible "encryption", and records the requests.
type fakeTransit struct {
	mu       sync.Mutex
	tokens   []string
	paths    []string
	failWith int
}

func (f *fakeTransit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.tokens = append(f.tokens, r.Header.Get("X-Vault-Token"))
	f.paths = append(f.paths, r.URL.Path)
	failWith := f.failWith
	f.mu.Unlock()

	if failWith != 0 {
		w.WriteHeader(failWith)
		_, _ = io.WriteString(w, `{"errors":["1 error occurred:\n\t* permission denied\n\n"]}`)
		return
	}
	var body map[string]string
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/encrypt/data-keys"):
		_, _ = fmt.Fprintf(w, `{"data":{"ciphertext":"vault:v1:%s","key_version":1}}`, body["plaintext"])
	case strings.HasSuffix(r.URL.Path, "/decrypt/data-keys"):
		_, _ = fmt.Fprintf(w, `{"data":{"plaintext":%q}}`, strings.TrimPrefix(body["ciphertext"], "vault:v1:"))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"errors":[]}`)
	}
}

func newFakeTransit(t *testing.T, cfg TransitConfig) (*fakeTransit, *TransitProvider) {
	t.Helper()
	fake := &fakeTransit{}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	cfg.Address = server.URL + "/"
	if cfg.Key == "" {
		cfg.Key = "data-keys"
	}
	if cfg.Token == "" && cfg.TokenFile == "" {
		cfg.Token = "s.token"
	}
	p, err := NewTransitProvider(cfg)
	require.NoError(t, err)
	return fake, p
}

func TestTransitProvider_CallsEncryptAndDecrypt(t *testing.T) {
	ctx := context.Background()
	fake, p := newFakeTransit(t, TransitConfig{Mount: "/kms/transit/"})

	wrapped, err := p.Wrap(ctx, []byte("bound key"))
	require.NoError(t, err)
	assert.Equal(t, "vault:v1:"+base64.StdEncoding.EncodeToString([]byte("bound key")), string(wrapped))
	plaintext, err := p.Unwrap(ctx, wrapped)
	require.NoError(t, err)
	assert.Equal(t, "bound key", string(plaintext))

	assert.Equal(t, []string{"/v1/kms/transit/encrypt/data-keys", "/v1/kms/transit/decrypt/data-keys"}, fake.paths)
	assert.Equal(t, []string{"s.token", "s.token"}, fake.tokens)
	assert.Equal(t, "transit:kms/transit/data-keys", p.ID())
	assert.False(t, p.Development())
	assert.True(t, p.PlainHTTP())
}

func TestTransitProvider_ReadsTheTokenFileOnEveryRequest(t *testing.T) {
	ctx := context.Background()
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("first\n"), 0o600))
	fake, p := newFakeTransit(t, TransitConfig{TokenFile: tokenFile})

	_, err := p.Wrap(ctx, []byte("k"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tokenFile, []byte("renewed"), 0o600))
	_, err = p.Wrap(ctx, []byte("k"))
	require.NoError(t, err)
	assert.Equal(t, []string{"first", "renewed"}, fake.tokens, "an agent may renew the token in place")

	require.NoError(t, os.Remove(tokenFile))
	_, err = p.Wrap(ctx, []byte("k"))
	assert.ErrorContains(t, err, "token_file")
}

func TestTransitProvider_ReportsServerErrorsWithoutTheRequest(t *testing.T) {
	ctx := context.Background()
	fake, p := newFakeTransit(t, TransitConfig{})
	fake.failWith = http.StatusForbidden

	_, err := p.Wrap(ctx, []byte("secret-key-bytes"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 403")
	assert.Contains(t, err.Error(), "permission denied")
	assert.NotContains(t, err.Error(), "secret-key-bytes")
	assert.NotContains(t, err.Error(), base64.StdEncoding.EncodeToString([]byte("secret-key-bytes")))
	assert.NotContains(t, err.Error(), "s.token")
}

func TestTransitProvider_RejectsMalformedResponses(t *testing.T) {
	ctx := context.Background()
	for name, body := range map[string]string{
		"not JSON":            `<html>proxy error</html>`,
		"no data":             `{"warnings":null}`,
		"no ciphertext":       `{"data":{}}`,
		"invalid plaintext":   `{"data":{"plaintext":"%%%","ciphertext":"vault:v1:x"}}`,
		"ciphertext no vault": `{"data":{"ciphertext":"plain"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			p, err := NewTransitProvider(TransitConfig{Address: server.URL, Token: "t", Key: "k"})
			require.NoError(t, err)

			wrapped, wrapErr := p.Wrap(ctx, []byte("k"))
			_, unwrapErr := p.Unwrap(ctx, []byte("vault:v1:x"))
			if wrapErr == nil {
				// Only the invalid plaintext is a valid encrypt response.
				assert.Equal(t, "vault:v1:x", string(wrapped))
			}
			assert.Error(t, unwrapErr)
		})
	}
}

func TestTransitProvider_RefusesRedirectsOutOfTLS(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the token must not be sent over http after a redirect")
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer secure.Close()

	p, err := NewTransitProvider(TransitConfig{Address: secure.URL, Token: "t", Key: "k"})
	require.NoError(t, err)
	p.client.Transport = secure.Client().Transport

	_, err = p.Wrap(context.Background(), []byte("k"))
	assert.ErrorContains(t, err, "redirect")
}

func TestTransitProvider_TimesOut(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer server.Close()
	defer close(release)

	p, err := NewTransitProvider(TransitConfig{Address: server.URL, Token: "t", Key: "k", Timeout: 100 * time.Millisecond})
	require.NoError(t, err)
	start := time.Now()
	_, err = p.Wrap(context.Background(), []byte("k"))
	assert.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestNewTransitProvider_ChecksTheConfiguration(t *testing.T) {
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile, []byte("not a certificate"), 0o600))
	for name, cfg := range map[string]TransitConfig{
		"no address":      {Token: "t", Key: "k"},
		"no scheme":       {Address: "vault:8200", Token: "t", Key: "k"},
		"another scheme":  {Address: "ftp://vault", Token: "t", Key: "k"},
		"no key":          {Address: "https://vault", Token: "t"},
		"a key path":      {Address: "https://vault", Token: "t", Key: "a/b"},
		"no token":        {Address: "https://vault", Key: "k"},
		"missing CA file": {Address: "https://vault", Token: "t", Key: "k", CACert: filepath.Join(t.TempDir(), "none.pem")},
		"invalid CA file": {Address: "https://vault", Token: "t", Key: "k", CACert: caFile},
	} {
		_, err := NewTransitProvider(cfg)
		assert.Error(t, err, name)
	}

	p, err := NewTransitProvider(TransitConfig{Address: "https://vault.internal:8200", Token: "t", Key: "data-keys"})
	require.NoError(t, err)
	assert.Equal(t, "transit:transit/data-keys", p.ID(), "the mount defaults to transit")
	assert.False(t, p.PlainHTTP())
}

// The tests below run against a real OpenBao (or Vault) dev server, as CI
// provides it:
//
//	docker run -d --name ce-test-openbao -e BAO_DEV_ROOT_TOKEN_ID=test -p 8200:8200 openbao/openbao:2.4.1
const (
	openBaoAddress = "http://localhost:8200"
	openBaoToken   = "test"
)

// openBao enables a Transit engine at a mount of its own, so that parallel
// test packages do not share keys, creates a key in it, and returns a
// provider for that key, with the mount. The mount is removed when the test
// ends.
func openBao(t *testing.T) (*TransitProvider, string, func(path string, body string)) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	call := func(method, path, body string) (int, string) {
		req, err := http.NewRequest(method, openBaoAddress+"/v1/"+path, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("X-Vault-Token", openBaoToken)
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	resp, err := client.Get(openBaoAddress + "/v1/sys/health")
	if err != nil {
		t.Skipf("OpenBao at %s is not reachable: %v", openBaoAddress, err)
	}
	resp.Body.Close()

	mount := "transit-test-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	status, body := call(http.MethodPost, "sys/mounts/"+mount, `{"type":"transit"}`)
	require.Less(t, status, 300, body)
	t.Cleanup(func() { call(http.MethodDelete, "sys/mounts/"+mount, "") })
	status, body = call(http.MethodPost, mount+"/keys/data-keys", `{"type":"aes256-gcm96"}`)
	require.Less(t, status, 300, body)

	p, err := NewTransitProvider(TransitConfig{Address: openBaoAddress, Token: openBaoToken, Mount: mount, Key: "data-keys"})
	require.NoError(t, err)
	post := func(path, body string) {
		t.Helper()
		status, raw := call(http.MethodPost, mount+"/"+path, body)
		require.Less(t, status, 300, raw)
	}
	return p, mount, post
}

func TestTransitProvider_OpenBao(t *testing.T) {
	ctx := context.Background()
	p, mount, post := openBao(t)

	wrapped, err := p.Wrap(ctx, []byte("bound key"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(wrapped), "vault:v1:"), string(wrapped))
	plaintext, err := p.Unwrap(ctx, wrapped)
	require.NoError(t, err)
	assert.Equal(t, "bound key", string(plaintext))

	tampered := []byte(strings.TrimSuffix(string(wrapped), string(wrapped[len(wrapped)-4:])) + "AAAA")
	_, err = p.Unwrap(ctx, tampered)
	assert.Error(t, err, "a changed ciphertext must not decrypt")

	// After the KEK is rotated in OpenBao, old ciphertexts still decrypt
	// and new ones use the new version.
	post("keys/data-keys/rotate", "")
	again, err := p.Wrap(ctx, []byte("bound key"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(again), "vault:v2:"), string(again))
	_, err = p.Unwrap(ctx, wrapped)
	assert.NoError(t, err)

	wrongToken, err := NewTransitProvider(TransitConfig{Address: openBaoAddress, Token: "wrong", Mount: mount, Key: "data-keys"})
	require.NoError(t, err)
	_, err = wrongToken.Unwrap(ctx, wrapped)
	assert.ErrorContains(t, err, "HTTP 403")
}
