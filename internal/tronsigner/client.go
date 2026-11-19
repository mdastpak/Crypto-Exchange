package tronsigner

import (
	"bytes"
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
	"time"

	"github.com/mdastpak/crypto-exchange/internal/withdrawals"
)

// ProviderName is the provider of the payouts that the signer pays, as
// the payouts record it.
const ProviderName = "tron-signer"

// Client is the server's side of the signer: the provider of the
// withdrawals on TRON (#68), over mutual TLS.
type Client struct {
	URL  string
	HTTP *http.Client
}

var _ withdrawals.Provider = Client{}

// ClientTLS is the server's TLS to the signer: its client certificate, and
// the CA of the signer's certificate.
func ClientTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(caFile) //nolint:gosec // G304: the path of the configuration, given by the operator.
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("tron-signer: %s holds no certificate", caFile)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS13}, nil
}

// NewClient returns the client of the signer at url, with tlsConfig.
func NewClient(url string, tlsConfig *tls.Config) Client {
	return Client{URL: strings.TrimRight(url, "/"), HTTP: &http.Client{Timeout: 30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsConfig}}}
}

// Name is tron-signer.
func (Client) Name() string { return ProviderName }

// assetOf names the asset of a transfer to the signer: a token by its
// contract, and TRX by its symbol.
func assetOf(t withdrawals.Transfer) string {
	if t.Contract != "" {
		return t.Contract
	}
	return "TRX"
}

// smallest is the amount of a transfer in the smallest unit of its asset;
// false for one with more decimals than the asset has.
func smallest(t withdrawals.Transfer) (string, bool) {
	v := t.Amount.Decimal().Shift(int32(t.Decimals)) //nolint:gosec // G115: decimals are 0 to 36 by the registry's check.
	if !v.IsInteger() || !v.IsPositive() {
		return "", false
	}
	return v.String(), true
}

// Submit asks the signer to pay t, once by its idempotency key, and
// returns the hash of its transaction.
func (c Client) Submit(ctx context.Context, t withdrawals.Transfer) (string, error) {
	if t.Rail != withdrawals.RailChain || t.Network != "tron" || t.Destination.Network != "tron" {
		return "", fmt.Errorf("%w: the signer pays TRON only", withdrawals.ErrTransferRefused)
	}
	amount, ok := smallest(t)
	if !ok {
		return "", fmt.Errorf("%w: the amount has more decimals than the asset", withdrawals.ErrTransferRefused)
	}
	body, err := json.Marshal(Request{Key: t.IdempotencyKey, To: t.Destination.Value, Asset: assetOf(t), Amount: amount})
	if err != nil {
		return "", err
	}
	p, status, err := c.call(ctx, http.MethodPost, "/v1/payments", body)
	switch {
	case err != nil:
		return "", err
	case status == http.StatusOK:
		return p.TxID, nil
	case status == http.StatusUnprocessableEntity, status == http.StatusConflict:
		return "", fmt.Errorf("%w: %s", withdrawals.ErrTransferRefused, p.Reason)
	default:
		// The result is unknown: Status settles it.
		return p.TxID, fmt.Errorf("tron-signer: the signer answered %d: %s", status, p.Reason)
	}
}

// Status asks the signer where t is.
func (c Client) Status(ctx context.Context, t withdrawals.Transfer) (withdrawals.TransferState, error) {
	p, status, err := c.call(ctx, http.MethodGet, "/v1/payments/"+url.PathEscape(t.IdempotencyKey), nil)
	switch {
	case err != nil:
		return withdrawals.TransferState{}, err
	case status == http.StatusNotFound:
		// The signer never recorded it: it may be sent again.
		return withdrawals.TransferState{Status: withdrawals.TransferNotReceived}, nil
	case status != http.StatusOK:
		return withdrawals.TransferState{}, fmt.Errorf("tron-signer: the signer answered %d: %s", status, p.Reason)
	}
	switch p.Status {
	case StatusCompleted:
		return withdrawals.TransferState{Status: withdrawals.TransferCompleted, TrackingID: p.TxID}, nil
	case StatusFailed:
		return withdrawals.TransferState{Status: withdrawals.TransferFailed, TrackingID: p.TxID, Reason: p.Reason}, nil
	case StatusNotReceived:
		return withdrawals.TransferState{Status: withdrawals.TransferNotReceived}, nil
	case StatusPending:
		return withdrawals.TransferState{Status: withdrawals.TransferPending, TrackingID: p.TxID}, nil
	}
	return withdrawals.TransferState{}, fmt.Errorf("tron-signer: unknown status %q", p.Status)
}

// call calls the signer, and reads its payment, or the error it gave as
// the payment's Reason.
func (c Client) call(ctx context.Context, method, path string, body []byte) (Payment, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, reader)
	if err != nil {
		return Payment{}, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Payment{}, 0, fmt.Errorf("tron-signer: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return Payment{}, resp.StatusCode, err
	}
	if resp.StatusCode == http.StatusOK {
		var p Payment
		if err := json.Unmarshal(raw, &p); err != nil {
			return Payment{}, resp.StatusCode, fmt.Errorf("tron-signer: %w", err)
		}
		return p, resp.StatusCode, nil
	}
	var e struct {
		Error   string  `json:"error"`
		Payment Payment `json:"payment"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return Payment{}, resp.StatusCode, errors.New("tron-signer: an answer that is not JSON")
	}
	e.Payment.Reason = e.Error
	return e.Payment, resp.StatusCode, nil
}

// ErrSweepRefused is a sweep that the signer refused: an asset it does not
// sweep, an index that is not one, or a key of another sweep.
var ErrSweepRefused = errors.New("tron-signer: the sweep is refused")

// Sweep asks the signer to take a sweep a step further, and returns where
// it is.
func (c Client) Sweep(ctx context.Context, r SweepRequest) (Sweep, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return Sweep{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/v1/sweeps", bytes.NewReader(body))
	if err != nil {
		return Sweep{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Sweep{}, fmt.Errorf("tron-signer: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return Sweep{}, err
	}
	var out struct {
		Sweep
		Error string `json:"error"`
		Inner *Sweep `json:"sweep"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Sweep{}, errors.New("tron-signer: an answer that is not JSON")
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return out.Sweep, nil
	case http.StatusUnprocessableEntity, http.StatusConflict:
		return Sweep{}, fmt.Errorf("%w: %s", ErrSweepRefused, out.Error)
	}
	sw := Sweep{Key: r.Key}
	if out.Inner != nil {
		sw = *out.Inner
	}
	return sw, fmt.Errorf("tron-signer: the signer answered %d: %s", resp.StatusCode, out.Error)
}
