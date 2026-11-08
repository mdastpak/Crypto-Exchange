package identity

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Jibit checks mobile ownership with the identity API of Jibit
// (Identicator), which #158 chose for identity checks.
//
// The client follows the documented shape of the API: an access token from
// POST {endpoint}/v1/tokens/generate with the API key and secret, and the
// matching service GET {endpoint}/v1/services/matching with mobileNumber
// and nationalCode, which answers {"matched": true|false}. It must be
// checked against the contracted version of the API, and its sandbox,
// before it is used in production.
//
// Each instance keeps its own access token, so instances need no shared
// state. A token is reused until shortly before it expires, and fetched
// again when the API refuses it.
type Jibit struct {
	endpoint          string
	apiKey, secretKey string
	client            *http.Client
	// retryDelay is the wait before the first retry; it doubles after.
	retryDelay time.Duration

	mu      sync.Mutex
	token   string
	expires time.Time
}

// jibitAttempts is how many times a request is sent while the provider is
// unavailable: a timeout, a network error, a 5xx, or a 429.
const jibitAttempts = 3

// NewJibit returns a Jibit client. timeout bounds each HTTP request.
func NewJibit(endpoint, apiKey, secretKey string, timeout time.Duration) *Jibit {
	return &Jibit{
		endpoint: strings.TrimRight(endpoint, "/"), apiKey: apiKey, secretKey: secretKey,
		client:     &http.Client{Timeout: timeout},
		retryDelay: 200 * time.Millisecond,
	}
}

// Provider names Jibit.
func (j *Jibit) Provider() string { return "jibit" }

// Match asks Jibit whether mobile belongs to nationalID.
func (j *Jibit) Match(ctx context.Context, nationalID, mobile string) (Match, error) {
	ref := uuid.NewString()
	query := url.Values{"mobileNumber": {mobile}, "nationalCode": {nationalID}}
	var answer struct {
		Matched *bool `json:"matched"`
	}
	if err := j.authorized(ctx, http.MethodGet, "/v1/services/matching?"+query.Encode(), ref, &answer); err != nil {
		return Match{}, err
	}
	if answer.Matched == nil {
		return Match{}, fmt.Errorf("%w: the answer has no matched field", ErrUnavailable)
	}
	return Match{Matched: *answer.Matched, Ref: ref}, nil
}

// authorized sends a request with the access token, and fetches a new
// token once when the API refuses the one it had.
func (j *Jibit) authorized(ctx context.Context, method, path, ref string, out any) error {
	for renewed := false; ; renewed = true {
		token, err := j.accessToken(ctx, renewed)
		if err != nil {
			return err
		}
		status, err := j.send(ctx, method, path, ref, token, nil, out)
		if status == http.StatusUnauthorized && !renewed {
			continue
		}
		return err
	}
}

// accessToken returns the cached token, or a new one when it is missing,
// about to expire, or renew is set.
func (j *Jibit) accessToken(ctx context.Context, renew bool) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !renew && j.token != "" && time.Now().Before(j.expires) {
		return j.token, nil
	}
	body, err := json.Marshal(map[string]string{"apiKey": j.apiKey, "secretKey": j.secretKey})
	if err != nil {
		return "", err
	}
	var answer struct {
		AccessToken string `json:"accessToken"`
	}
	if _, err := j.send(ctx, http.MethodPost, "/v1/tokens/generate", uuid.NewString(), "", body, &answer); err != nil {
		return "", err
	}
	if answer.AccessToken == "" {
		return "", fmt.Errorf("%w: the token answer has no accessToken", ErrUnavailable)
	}
	j.token, j.expires = answer.AccessToken, tokenExpiry(answer.AccessToken)
	return j.token, nil
}

// tokenExpiry returns a minute before the exp claim of a JWT access token,
// or an hour from now when the token does not say.
func tokenExpiry(token string) time.Time {
	fallback := time.Now().Add(time.Hour)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fallback
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fallback
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return fallback
	}
	return time.Unix(claims.Exp, 0).Add(-time.Minute)
}

// errorAnswer is the body of an error of the API. Its message is not kept:
// it may repeat the request's personal data.
type errorAnswer struct {
	Code string `json:"code"`
}

// send sends one request, retrying while the provider is unavailable, and
// decodes a 200 answer into out. It returns the last HTTP status. Errors
// name the path and the provider's error code, never the query, which holds
// personal data.
func (j *Jibit) send(ctx context.Context, method, path, ref, token string, body []byte, out any) (int, error) {
	name := path
	if i := strings.IndexByte(name, '?'); i >= 0 {
		name = name[:i]
	}
	delay := j.retryDelay
	var last error
	status := 0
	for attempt := 1; attempt <= jibitAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return status, fmt.Errorf("%w: %s: %w", ErrUnavailable, name, ctx.Err())
			case <-time.After(delay):
			}
			delay *= 2
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, j.endpoint+path, reader)
		if err != nil {
			// The error holds the URL, and so the query: it is not kept.
			return 0, fmt.Errorf("jibit %s: the request cannot be built", name)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Request-Id", ref)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := j.client.Do(req)
		if err != nil {
			// The error of the client holds the URL, and so the query:
			// only its kind is kept.
			last = fmt.Errorf("%w: %s: the request failed", ErrUnavailable, name)
			if ctx.Err() != nil {
				return 0, last
			}
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		status = resp.StatusCode
		switch {
		case status == http.StatusOK && readErr == nil:
			if err := json.Unmarshal(data, out); err != nil {
				return status, fmt.Errorf("%w: %s: an answer that cannot be read", ErrUnavailable, name)
			}
			return status, nil
		case status == http.StatusTooManyRequests || status >= 500 || readErr != nil:
			last = fmt.Errorf("%w: %s answered %d", ErrUnavailable, name, status)
			continue
		default:
			var e errorAnswer
			_ = json.Unmarshal(data, &e)
			return status, fmt.Errorf("jibit %s answered %d %s", name, status, e.Code)
		}
	}
	return status, last
}
