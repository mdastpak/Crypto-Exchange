package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testNationalID = "0012345679"
	testMobile     = "09121234567"
)

// fakeJibit is a fake of the Jibit identity API. Its fields set how it
// answers; it counts the requests.
type fakeJibit struct {
	mu            sync.Mutex
	owners        map[string]string // mobile → national ID
	tokens        int
	matches       int
	failMatches   int // answer 503 to this many matching requests first
	expireTokens  bool
	matchBody     string // answer this body to matching instead
	matchStatus   int
	issuedToken   string
	rejectedToken string
}

func (f *fakeJibit) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/tokens/generate", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		if body["apiKey"] != "key" || body["secretKey"] != "secret" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"code":"invalid.credentials"}`)
			return
		}
		f.tokens++
		exp := time.Now().Add(24 * time.Hour).Unix()
		payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp)))
		f.issuedToken = fmt.Sprintf("h.%s.%d", payload, f.tokens)
		_, _ = fmt.Fprintf(w, `{"accessToken":%q,"refreshToken":"r"}`, f.issuedToken)
	})
	mux.HandleFunc("GET /v1/services/matching", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.matches++
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token != f.issuedToken || token == f.rejectedToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if f.expireTokens {
			f.rejectedToken, f.expireTokens = token, false
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if f.failMatches > 0 {
			f.failMatches--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if f.matchStatus != 0 {
			w.WriteHeader(f.matchStatus)
		}
		if f.matchBody != "" {
			_, _ = fmt.Fprint(w, f.matchBody)
			return
		}
		q := r.URL.Query()
		matched := f.owners[q.Get("mobileNumber")] == q.Get("nationalCode")
		_, _ = fmt.Fprintf(w, `{"matched":%t}`, matched)
	})
	return mux
}

func newFakeJibit(t *testing.T) (*fakeJibit, *Jibit) {
	f := &fakeJibit{owners: map[string]string{testMobile: testNationalID}}
	server := httptest.NewServer(f.handler(t))
	t.Cleanup(server.Close)
	j := NewJibit(server.URL+"/", "key", "secret", 2*time.Second)
	j.retryDelay = time.Millisecond
	return f, j
}

func TestJibit_MatchAndMismatch(t *testing.T) {
	f, j := newFakeJibit(t)
	m, err := j.Match(context.Background(), testNationalID, testMobile)
	require.NoError(t, err)
	assert.True(t, m.Matched)
	assert.NotEmpty(t, m.Ref)
	m, err = j.Match(context.Background(), "9876543210", testMobile)
	require.NoError(t, err)
	assert.False(t, m.Matched)
	assert.Equal(t, 1, f.tokens, "the token is reused")
}

func TestJibit_RetriesWhileUnavailable(t *testing.T) {
	f, j := newFakeJibit(t)
	f.failMatches = 2
	m, err := j.Match(context.Background(), testNationalID, testMobile)
	require.NoError(t, err)
	assert.True(t, m.Matched)
	assert.Equal(t, 3, f.matches)

	f.failMatches = 5
	_, err = j.Match(context.Background(), testNationalID, testMobile)
	assert.ErrorIs(t, err, ErrUnavailable)
	assert.Equal(t, 6, f.matches, "three attempts, then it gives up")
}

func TestJibit_RenewsARefusedToken(t *testing.T) {
	f, j := newFakeJibit(t)
	_, err := j.Match(context.Background(), testNationalID, testMobile)
	require.NoError(t, err)
	f.expireTokens = true
	m, err := j.Match(context.Background(), testNationalID, testMobile)
	require.NoError(t, err)
	assert.True(t, m.Matched)
	assert.Equal(t, 2, f.tokens)
}

func TestJibit_FailuresNeverDecideAndNeverLeakData(t *testing.T) {
	for name, set := range map[string]func(f *fakeJibit){
		"malformed":  func(f *fakeJibit) { f.matchBody = `not json` },
		"no matched": func(f *fakeJibit) { f.matchBody = `{"result":true}` },
		"a client error": func(f *fakeJibit) {
			f.matchStatus, f.matchBody = http.StatusBadRequest, `{"code":"nationalCode.not_valid","message":"0012345679"}`
		},
		"a rate limit":    func(f *fakeJibit) { f.matchStatus = http.StatusTooManyRequests },
		"a server outage": func(f *fakeJibit) { f.failMatches = 10 },
	} {
		f, j := newFakeJibit(t)
		set(f)
		_, err := j.Match(context.Background(), testNationalID, testMobile)
		require.Error(t, err, name)
		assert.NotContains(t, err.Error(), testNationalID, name)
		assert.NotContains(t, err.Error(), testMobile, name)
	}

	j := NewJibit("http://127.0.0.1:1", "key", "secret", time.Second)
	j.retryDelay = time.Millisecond
	_, err := j.Match(context.Background(), testNationalID, testMobile)
	assert.ErrorIs(t, err, ErrUnavailable, "a network error")

	_, j = newFakeJibit(t)
	j.secretKey = "wrong"
	_, err = j.Match(context.Background(), testNationalID, testMobile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid.credentials")
	assert.NotContains(t, err.Error(), "wrong")
}

func TestJibit_StopsWithItsContext(t *testing.T) {
	f, j := newFakeJibit(t)
	f.failMatches = 10
	j.retryDelay = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := j.Match(ctx, testNationalID, testMobile)
	assert.ErrorIs(t, err, ErrUnavailable)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestTokenExpiry(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	assert.True(t, tokenExpiry("h."+payload+".s").Equal(exp.Add(-time.Minute)))
	assert.WithinDuration(t, time.Now().Add(time.Hour), tokenExpiry("opaque"), time.Minute)
}
