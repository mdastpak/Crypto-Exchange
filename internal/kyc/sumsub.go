package kyc

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // G505: Sumsub may sign webhooks with HMAC-SHA1, which stays a sound MAC.
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/redirect"
)

// Sumsub is the Sumsub provider (#72), by its documented API: requests
// signed with the app token and the secret key, applicants by the
// application id as their externalUserId, SDK access tokens, and webhooks
// signed with the webhook secret. It serves the international region only.
//
// It was written from the documentation and tested against a fake server;
// it must be checked against the account's API before production.
type Sumsub struct {
	BaseURL       string
	AppToken      string
	SecretKey     string
	WebhookSecret string
	// Level is the verification level of the account that the applicants
	// are checked at, such as "basic-kyc-level".
	Level  string
	Client *http.Client
	// TokenTTL is how long an SDK token works; 0 is 10 minutes.
	TokenTTL time.Duration
	Now      func() time.Time
}

// ProviderSumsub names the Sumsub provider.
const ProviderSumsub = "sumsub"

// Name implements Provider.
func (s *Sumsub) Name() string { return ProviderSumsub }

func (s *Sumsub) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// sign is the signature of a request: the hex HMAC-SHA256, under the
// secret key, of the time in seconds, the method, the path with its query,
// and the body.
func (s *Sumsub) sign(ts, method, pathQuery string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(s.SecretKey))
	mac.Write([]byte(ts + method + pathQuery))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// sumsubError is an error of the API.
type sumsubError struct {
	Status int
	Body   string
}

func (e *sumsubError) Error() string { return fmt.Sprintf("sumsub: status %d: %s", e.Status, e.Body) }

func (s *Sumsub) do(ctx context.Context, method, pathQuery string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.BaseURL, "/")+pathQuery, bytes.NewReader(body))
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(s.now().Unix(), 10)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-App-Token", s.AppToken)
	req.Header.Set("X-App-Access-Ts", ts)
	req.Header.Set("X-App-Access-Sig", s.sign(ts, method, pathQuery, body))
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, CheckRedirect: redirect.Refuse}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sumsub: %s %s: %w", method, strings.SplitN(pathQuery, "?", 2)[0], err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("sumsub: read the answer: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &sumsubError{Status: resp.StatusCode, Body: string(raw[:min(len(raw), 300)])}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("sumsub: the answer: %w", err)
	}
	return nil
}

// Start implements Provider: it creates the applicant of ref, at the
// level, or finds the one that a retry created.
func (s *Sumsub) Start(ctx context.Context, ref string) (string, error) {
	var created struct {
		ID string `json:"id"`
	}
	q := url.Values{"levelName": {s.Level}}
	err := s.do(ctx, http.MethodPost, "/resources/applicants?"+q.Encode(), map[string]string{"externalUserId": ref}, &created)
	if e, ok := err.(*sumsubError); ok && e.Status == http.StatusConflict {
		err = s.do(ctx, http.MethodGet, "/resources/applicants/-;externalUserId="+url.PathEscape(ref)+"/one", nil, &created)
	}
	if err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", fmt.Errorf("sumsub: the applicant of %s has no id", ref)
	}
	return created.ID, nil
}

// Session implements Provider.
func (s *Sumsub) Session(ctx context.Context, ref string) (Session, error) {
	ttl := s.TokenTTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	var out struct {
		Token string `json:"token"`
	}
	q := url.Values{"userId": {ref}, "levelName": {s.Level}, "ttlInSecs": {strconv.Itoa(int(ttl.Seconds()))}}
	if err := s.do(ctx, http.MethodPost, "/resources/accessTokens?"+q.Encode(), nil, &out); err != nil {
		return Session{}, err
	}
	if out.Token == "" {
		return Session{}, fmt.Errorf("sumsub: no access token for %s", ref)
	}
	return Session{Provider: ProviderSumsub, Token: out.Token, ExpiresAt: s.now().Add(ttl).UTC()}, nil
}

// sumsubWebhook is the part of a webhook that the server reads.
type sumsubWebhook struct {
	Type           string `json:"type"`
	ApplicantID    string `json:"applicantId"`
	ExternalUserID string `json:"externalUserId"`
	ReviewResult   struct {
		ReviewAnswer     string   `json:"reviewAnswer"`
		RejectLabels     []string `json:"rejectLabels"`
		ReviewRejectType string   `json:"reviewRejectType"`
	} `json:"reviewResult"`
}

// Verify implements Provider: the X-Payload-Digest header is the hex HMAC
// of the body under the webhook secret, by the algorithm of
// X-Payload-Digest-Alg, SHA-256 when it is not set.
func (s *Sumsub) Verify(header http.Header, body []byte) (Verdict, error) {
	var h func() hash.Hash
	switch header.Get("X-Payload-Digest-Alg") {
	case "", "HMAC_SHA256_HEX":
		h = sha256.New
	case "HMAC_SHA512_HEX":
		h = sha512.New
	case "HMAC_SHA1_HEX":
		h = sha1.New
	default:
		return Verdict{}, ErrWebhookSignature
	}
	given, err := hex.DecodeString(header.Get("X-Payload-Digest"))
	if err != nil || s.WebhookSecret == "" {
		return Verdict{}, ErrWebhookSignature
	}
	mac := hmac.New(h, []byte(s.WebhookSecret))
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), given) {
		return Verdict{}, ErrWebhookSignature
	}
	var w sumsubWebhook
	if err := json.Unmarshal(body, &w); err != nil {
		return Verdict{}, fmt.Errorf("sumsub: the webhook: %w", err)
	}
	v := Verdict{Ref: w.ExternalUserID, ProviderRef: w.ApplicantID}
	switch w.Type {
	case "applicantPending":
		v.Outcome = OutcomePending
	case "applicantReviewed":
		switch {
		case w.ReviewResult.ReviewAnswer == "GREEN":
			v.Outcome = OutcomeApproved
		case w.ReviewResult.ReviewAnswer == "RED" && w.ReviewResult.ReviewRejectType == "RETRY":
			v.Outcome, v.Labels = OutcomeRetry, w.ReviewResult.RejectLabels
		case w.ReviewResult.ReviewAnswer == "RED":
			v.Outcome, v.Labels = OutcomeRejected, w.ReviewResult.RejectLabels
		}
	}
	return v, nil
}
