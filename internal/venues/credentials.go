package venues

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Secret is a credential. It prints, formats, and marshals as [redacted],
// so that no log, error, or answer holds it; Reveal is for the signer only.
type Secret struct{ v string }

// NewSecret returns s as a Secret.
func NewSecret(s string) Secret { return Secret{v: s} }

const redacted = "[redacted]"

func (s Secret) String() string                { return redacted }
func (s Secret) GoString() string              { return redacted }
func (s Secret) MarshalJSON() ([]byte, error)  { return json.Marshal(redacted) }
func (s Secret) MarshalText() ([]byte, error)  { return []byte(redacted), nil }
func (s Secret) Format(f fmt.State, verb rune) { _, _ = io.WriteString(f, redacted) }
func (s Secret) Empty() bool                   { return s.v == "" }
func (s Secret) Reveal() string                { return s.v }
func (s *Secret) UnmarshalJSON(b []byte) error { return json.Unmarshal(b, &s.v) }

// Credentials are our keys at a venue: an API key and its secret (Binance),
// or a token (Nobitex).
type Credentials struct {
	APIKey    Secret `json:"api_key"`
	APISecret Secret `json:"api_secret"`
	Token     Secret `json:"token"`
}

// CredentialSource returns the credentials of a venue by its name.
type CredentialSource interface {
	Credentials(ctx context.Context, venue string) (Credentials, error)
}

// ErrNoCredentials is a venue with no credentials in its source.
var ErrNoCredentials = errors.New("venues: no credentials for the venue")

// OpenBaoConfig locates the credentials in the KV version 2 engine of
// OpenBao or Vault: one secret per venue at <mount>/data/<prefix><venue>,
// with the fields api_key, api_secret, and token.
type OpenBaoConfig struct {
	Address string
	// Token authenticates; TokenFile, when set, is read on every request,
	// so that an agent can renew it in place.
	Token, TokenFile string
	Mount, Prefix    string
	CACert           string
	Timeout          time.Duration
}

// OpenBao reads the credentials from OpenBao.
type OpenBao struct {
	cfg    OpenBaoConfig
	client *http.Client
	base   string
}

// NewOpenBao checks cfg. It does not contact the server.
func NewOpenBao(cfg OpenBaoConfig) (*OpenBao, error) {
	address, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.Address), "/"))
	if err != nil || (address.Scheme != "https" && address.Scheme != "http") || address.Host == "" {
		return nil, errors.New("venues: the OpenBao address must be an http or https URL")
	}
	if cfg.Token == "" && cfg.TokenFile == "" {
		return nil, errors.New("venues: OpenBao needs a token or a token file")
	}
	if cfg.Mount = strings.Trim(cfg.Mount, "/"); cfg.Mount == "" {
		cfg.Mount = "secret"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CACert != "" {
		pem, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("venues: reading the CA of OpenBao: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("venues: the CA of OpenBao holds no PEM certificate")
		}
		transport.TLSClientConfig.RootCAs = pool
	}
	return &OpenBao{cfg: cfg, base: address.String(), client: &http.Client{
		Transport: transport, Timeout: cfg.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("venues: OpenBao redirected") },
	}}, nil
}

// Credentials reads the secret of venue.
func (o *OpenBao) Credentials(ctx context.Context, venue string) (Credentials, error) {
	token := o.cfg.Token
	if o.cfg.TokenFile != "" {
		b, err := os.ReadFile(o.cfg.TokenFile)
		if err != nil {
			return Credentials{}, fmt.Errorf("venues: reading the OpenBao token file: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	path := o.base + "/v1/" + o.cfg.Mount + "/data/" + url.PathEscape(o.cfg.Prefix+venue)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return Credentials{}, err
	}
	req.Header.Set("X-Vault-Token", token)
	resp, err := o.client.Do(req)
	if err != nil {
		return Credentials{}, errors.New("venues: OpenBao cannot be reached")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Credentials{}, fmt.Errorf("%w %s in OpenBao", ErrNoCredentials, venue)
	case resp.StatusCode != http.StatusOK:
		return Credentials{}, fmt.Errorf("venues: OpenBao answered %d for the credentials of %s", resp.StatusCode, venue)
	}
	var body struct {
		Data struct {
			Data Credentials `json:"data"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return Credentials{}, fmt.Errorf("venues: the credentials of %s in OpenBao cannot be read", venue)
	}
	return body.Data.Data, nil
}

// File reads the credentials from a JSON file that maps each venue to its
// credentials, for development; release mode refuses it.
type File struct{ Path string }

// Credentials reads the file on every call, so that a change applies
// without a restart.
func (f File) Credentials(_ context.Context, venue string) (Credentials, error) {
	b, err := os.ReadFile(f.Path)
	if err != nil {
		return Credentials{}, fmt.Errorf("venues: reading the credentials file: %w", err)
	}
	var all map[string]Credentials
	if err := json.Unmarshal(b, &all); err != nil {
		return Credentials{}, errors.New("venues: the credentials file is not a JSON object of venues")
	}
	c, ok := all[venue]
	if !ok {
		return Credentials{}, fmt.Errorf("%w %s in the credentials file", ErrNoCredentials, venue)
	}
	return c, nil
}

// Static is a fixed set of credentials, for tests.
type Static map[string]Credentials

func (s Static) Credentials(_ context.Context, venue string) (Credentials, error) {
	c, ok := s[venue]
	if !ok {
		return Credentials{}, fmt.Errorf("%w %s", ErrNoCredentials, venue)
	}
	return c, nil
}

// Cached keeps the credentials of each venue for TTL, so that a rotation
// in the source applies within TTL without a restart, and Forget drops
// them at once, as after the venue refused them.
type Cached struct {
	Source CredentialSource
	TTL    time.Duration

	mu    sync.Mutex
	cache map[string]cachedCredentials
	now   func() time.Time
}

type cachedCredentials struct {
	c  Credentials
	at time.Time
}

// NewCached returns a cache of source.
func NewCached(source CredentialSource, ttl time.Duration) *Cached {
	return &Cached{Source: source, TTL: ttl, cache: map[string]cachedCredentials{}, now: time.Now}
}

func (c *Cached) Credentials(ctx context.Context, venue string) (Credentials, error) {
	c.mu.Lock()
	hit, ok := c.cache[venue]
	c.mu.Unlock()
	if ok && c.now().Sub(hit.at) < c.TTL {
		return hit.c, nil
	}
	creds, err := c.Source.Credentials(ctx, venue)
	if err != nil {
		if ok {
			// Keep signing with the last ones while the source is away.
			return hit.c, nil
		}
		return Credentials{}, err
	}
	c.mu.Lock()
	c.cache[venue] = cachedCredentials{c: creds, at: c.now()}
	c.mu.Unlock()
	return creds, nil
}

// Forget drops the credentials of venue, so that the next call reads them
// again.
func (c *Cached) Forget(venue string) {
	c.mu.Lock()
	delete(c.cache, venue)
	c.mu.Unlock()
}
