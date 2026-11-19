package tronsigner

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
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

// Handler serves the signer's API to the clients of AllowedClients.
//
//	GET  /v1/hot-wallet       the address that pays
//	POST /v1/payments         pay a Request, once by its key
//	GET  /v1/payments/{key}   where a payment is
//	POST /v1/sweeps           take a SweepRequest a step further
//	GET  /v1/sweeps/{key}     where a sweep is
func Handler(s *Signer, allowed []string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/hot-wallet", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, map[string]string{"address": s.HotWallet()})
	})
	mux.HandleFunc("POST /v1/payments", func(w http.ResponseWriter, r *http.Request) {
		var req Request
		dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			write(w, http.StatusBadRequest, map[string]string{"error": "the body is not a payment"})
			return
		}
		p, err := s.Pay(r.Context(), req)
		answer(w, req.Key, p, err)
	})
	mux.HandleFunc("GET /v1/payments/{key}", func(w http.ResponseWriter, r *http.Request) {
		p, err := s.Status(r.Context(), r.PathValue("key"))
		answer(w, r.PathValue("key"), p, err)
	})
	mux.HandleFunc("POST /v1/sweeps", func(w http.ResponseWriter, r *http.Request) {
		var req SweepRequest
		dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			write(w, http.StatusBadRequest, map[string]string{"error": "the body is not a sweep"})
			return
		}
		sw, err := s.Sweep(r.Context(), req)
		answerSweep(w, req.Key, sw, err)
	})
	mux.HandleFunc("GET /v1/sweeps/{key}", func(w http.ResponseWriter, r *http.Request) {
		sw, err := s.SweepStatus(r.PathValue("key"))
		answerSweep(w, r.PathValue("key"), sw, err)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The TLS handshake verified the certificate against the client
		// CA; only the named clients may call.
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || !slices.Contains(allowed, r.TLS.PeerCertificates[0].Subject.CommonName) {
			write(w, http.StatusForbidden, map[string]string{"error": "the client is not allowed"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func answer(w http.ResponseWriter, key string, p Payment, err error) {
	switch {
	case err == nil:
		write(w, http.StatusOK, p)
	case errors.Is(err, ErrRefused):
		write(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error(), "code": "refused"})
	case errors.Is(err, ErrKeyReused):
		write(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "key_reused"})
	case errors.Is(err, ErrNotFound):
		write(w, http.StatusNotFound, map[string]string{"error": err.Error(), "code": "not_found"})
	default:
		log.Printf("tron-signer: payment %q: %v", key, err) //nolint:gosec // G706: the key is quoted, so it cannot forge a line.
		// A payment that was recorded is answered too: the client knows
		// its transaction, and asks again.
		if p.TxID != "" {
			write(w, http.StatusBadGateway, map[string]any{"error": "the node did not answer", "code": "node", "payment": p})
			return
		}
		write(w, http.StatusBadGateway, map[string]string{"error": "the node did not answer", "code": "node"})
	}
}

func answerSweep(w http.ResponseWriter, key string, sw Sweep, err error) {
	switch {
	case err == nil:
		write(w, http.StatusOK, sw)
	case errors.Is(err, ErrRefused):
		write(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error(), "code": "refused"})
	case errors.Is(err, ErrKeyReused):
		write(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "key_reused"})
	case errors.Is(err, ErrNotFound):
		write(w, http.StatusNotFound, map[string]string{"error": err.Error(), "code": "not_found"})
	default:
		log.Printf("tron-signer: sweep %q: %v", key, err) //nolint:gosec // G706: the key is quoted, so it cannot forge a line.
		write(w, http.StatusBadGateway, map[string]any{"error": "the node did not answer", "code": "node", "sweep": sw})
	}
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ServerTLS is the TLS of the signer: TLS 1.3, with a client certificate
// signed by the client CA required.
func ServerTLS(cfg *Config) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(cfg.TLS.ClientCAFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("tron-signer: %s holds no certificate", cfg.TLS.ClientCAFile)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS13}, nil
}

// UnwrapSeed decrypts the seed with Vault's Transit engine: the ciphertext
// of the file, "vault:v1:...", by the key, with the token.
func UnwrapSeed(ctx context.Context, client *http.Client, cfg *Config, token string) ([]byte, error) {
	if token == "" {
		return nil, errors.New("tron-signer: VAULT_TOKEN is required to decrypt the seed")
	}
	ciphertext, err := os.ReadFile(cfg.Seed.CiphertextFile)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]string{"ciphertext": strings.TrimSpace(string(ciphertext))})
	url := strings.TrimRight(cfg.Seed.VaultAddr, "/") + "/v1/transit/decrypt/" + cfg.Seed.TransitKey
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tron-signer: decrypt the seed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tron-signer: decrypt the seed: Vault answered %d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return nil, fmt.Errorf("tron-signer: decrypt the seed: %w", err)
	}
	seed, err := base64.StdEncoding.DecodeString(out.Data.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("tron-signer: the seed is not base64: %w", err)
	}
	return seed, nil
}
