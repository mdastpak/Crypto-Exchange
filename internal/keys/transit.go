package keys

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// TransitConfig locates a key of the Transit secrets engine of HashiCorp
// Vault or OpenBao, whose APIs are the same.
type TransitConfig struct {
	// Address is the base URL of the server, such as https://vault:8200.
	Address string
	// Token authenticates the requests. TokenFile, when set, is read on
	// every request instead, so that an agent such as Vault Agent can renew
	// the token in place.
	Token     string
	TokenFile string
	// Mount is the path of the Transit engine, "transit" by default.
	Mount string
	// Key is the name of the Transit key that wraps the data keys.
	Key string
	// CACert is a PEM file of the certificate authorities to trust instead
	// of the system ones.
	CACert  string
	Timeout time.Duration
}

// TransitProvider wraps data keys with the Transit encrypt and decrypt
// endpoints. The KEK never leaves the server; this process only holds a
// token that may use it.
type TransitProvider struct {
	client    *http.Client
	endpoint  string // <address>/v1/<mount>
	key       string
	token     string
	tokenFile string
	id        string
	plainHTTP bool
}

// maxTransitResponse bounds the response bodies read from the server.
const maxTransitResponse = 1 << 20

// NewTransitProvider checks cfg and returns a provider. It does not contact
// the server.
func NewTransitProvider(cfg TransitConfig) (*TransitProvider, error) {
	address, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.Address), "/"))
	if err != nil || (address.Scheme != "https" && address.Scheme != "http") || address.Host == "" {
		return nil, errors.New("keys.transit.address (VAULT_ADDR) must be an http or https URL, such as https://vault:8200")
	}
	mount := strings.Trim(strings.TrimSpace(cfg.Mount), "/")
	if mount == "" {
		mount = "transit"
	}
	key := strings.TrimSpace(cfg.Key)
	if key == "" || strings.Contains(key, "/") {
		return nil, errors.New("keys.transit.key (KEYS_TRANSIT_KEY) must name a Transit key")
	}
	if cfg.Token == "" && cfg.TokenFile == "" {
		return nil, errors.New("keys.transit needs a token: set keys.transit.token (VAULT_TOKEN) or keys.transit.token_file (KEYS_TRANSIT_TOKEN_FILE)")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CACert != "" {
		pem, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("reading keys.transit.ca_cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("keys.transit.ca_cert holds no PEM certificate")
		}
		transport.TLSClientConfig.RootCAs = pool
	}

	segments := strings.Split(mount, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return &TransitProvider{
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			// A standby node may redirect to the active one. The token goes
			// with the request, so a redirect may not leave TLS.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return errors.New("too many redirects")
				}
				if req.URL.Scheme != via[0].URL.Scheme {
					return errors.New("refusing a redirect to another scheme")
				}
				return nil
			},
		},
		endpoint:  address.String() + "/v1/" + strings.Join(segments, "/"),
		key:       url.PathEscape(key),
		token:     cfg.Token,
		tokenFile: cfg.TokenFile,
		id:        "transit:" + mount + "/" + key,
		plainHTTP: address.Scheme == "http",
	}, nil
}

func (p *TransitProvider) ID() string        { return p.id }
func (p *TransitProvider) Development() bool { return false }

// PlainHTTP reports an http address, over which the token and the keys
// travel unencrypted; release mode refuses it.
func (p *TransitProvider) PlainHTTP() bool { return p.plainHTTP }

func (p *TransitProvider) Wrap(ctx context.Context, plaintext []byte) ([]byte, error) {
	var resp struct {
		Ciphertext string `json:"ciphertext"`
	}
	err := p.call(ctx, "encrypt", map[string]string{"plaintext": base64.StdEncoding.EncodeToString(plaintext)}, &resp)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(resp.Ciphertext, "vault:v") {
		return nil, errors.New("transit encrypt returned no ciphertext")
	}
	return []byte(resp.Ciphertext), nil
}

func (p *TransitProvider) Unwrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	var resp struct {
		Plaintext string `json:"plaintext"`
	}
	if err := p.call(ctx, "decrypt", map[string]string{"ciphertext": string(wrapped)}, &resp); err != nil {
		return nil, err
	}
	plaintext, err := base64.StdEncoding.DecodeString(resp.Plaintext)
	if err != nil || len(plaintext) == 0 {
		return nil, errors.New("transit decrypt returned no valid plaintext")
	}
	return plaintext, nil
}

// call posts body to <endpoint>/<operation>/<key> and decodes the data of
// the response into data. Errors carry the server's messages, never the
// request, which holds the key.
func (p *TransitProvider) call(ctx context.Context, operation string, body interface{}, data interface{}) error {
	token := p.token
	if p.tokenFile != "" {
		raw, err := os.ReadFile(p.tokenFile)
		if err != nil {
			return fmt.Errorf("reading keys.transit.token_file: %w", err)
		}
		token = strings.TrimSpace(string(raw))
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/"+operation+"/"+p.key, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vault-Token", token)

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("transit %s: %w", operation, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTransitResponse))
	if err != nil {
		return fmt.Errorf("transit %s: reading the response: %w", operation, err)
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []string        `json:"errors"`
	}
	decodeErr := json.Unmarshal(raw, &envelope)
	if resp.StatusCode != http.StatusOK {
		message := http.StatusText(resp.StatusCode)
		if decodeErr == nil && len(envelope.Errors) > 0 {
			message = strings.Join(strings.Fields(strings.Join(envelope.Errors, "; ")), " ")
		}
		return fmt.Errorf("transit %s of key %s: HTTP %d: %s", operation, p.id, resp.StatusCode, message)
	}
	if decodeErr != nil || len(envelope.Data) == 0 {
		return fmt.Errorf("transit %s: the response is not a Transit response", operation)
	}
	if err := json.Unmarshal(envelope.Data, data); err != nil {
		return fmt.Errorf("transit %s: the response is not a Transit response", operation)
	}
	return nil
}
