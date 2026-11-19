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
	testCard       = "6037991234567890"
	testIBAN       = "IR820540102680020817909002"
)

// testBirth is 1991-08-16, 1370-05-25 in the Persian calendar.
var testBirth = time.Date(1991, 8, 16, 0, 0, 0, 0, time.UTC)

// fakeJibit is a fake of the Jibit identity API. Its fields set how it
// answers; it counts the requests.
type fakeJibit struct {
	mu            sync.Mutex
	owners        map[string]string // mobile, card, or IBAN → national ID
	births        map[string]string // national ID → date of birth, YYYYMMDD in the Persian calendar
	lastQuery     string
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
		f.lastQuery = r.URL.RawQuery
		var matched bool
		switch number := q.Get("cardNumber") + q.Get("iban"); {
		case q.Get("mobileNumber") != "":
			matched = f.owners[q.Get("mobileNumber")] == q.Get("nationalCode")
		case number != "":
			matched = f.owners[number] == q.Get("nationalCode") && f.births[q.Get("nationalCode")] == q.Get("birthDate")
		default:
			matched = f.births[q.Get("nationalCode")] == q.Get("birthDate")
		}
		_, _ = fmt.Fprintf(w, `{"matched":%t}`, matched)
	})
	return mux
}

func newFakeJibit(t *testing.T) (*fakeJibit, *Jibit) {
	f := &fakeJibit{
		owners: map[string]string{testMobile: testNationalID, testCard: testNationalID, testIBAN: testNationalID},
		births: map[string]string{testNationalID: "13700525"},
	}
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

// The national ID and its date of birth, and the owner of a card and of an
// IBAN (#158): matched, not matched for another person or another date,
// and an account number, which Jibit does not check.
func TestJibit_IdentityAndBankOwnership(t *testing.T) {
	ctx := context.Background()
	f, j := newFakeJibit(t)
	person := Person{NationalID: testNationalID, BirthDate: testBirth}

	m, err := j.Identify(ctx, person)
	require.NoError(t, err)
	assert.True(t, m.Matched)
	assert.Contains(t, f.lastQuery, "birthDate=13700525", "the date of birth in the Persian calendar")
	m, err = j.Identify(ctx, Person{NationalID: testNationalID, BirthDate: testBirth.AddDate(0, 0, 1)})
	require.NoError(t, err)
	assert.False(t, m.Matched, "another date")

	for _, account := range []BankAccount{{Kind: "card", Number: testCard}, {Kind: "iban", Number: testIBAN}} {
		m, err := j.MatchBank(ctx, person, account)
		require.NoError(t, err, account.Kind)
		assert.True(t, m.Matched, account.Kind)
		assert.NotEmpty(t, m.Ref)
		m, err = j.MatchBank(ctx, Person{NationalID: "9876543210", BirthDate: testBirth}, account)
		require.NoError(t, err)
		assert.False(t, m.Matched, "%s of another person", account.Kind)
	}
	assert.Contains(t, f.lastQuery, "iban=")

	before := f.matches
	_, err = j.MatchBank(ctx, person, BankAccount{Kind: "account", Number: "0102680020817", Bank: "melli"})
	require.ErrorIs(t, err, ErrUnsupported)
	assert.NotErrorIs(t, err, ErrUnavailable, "not worth trying again")
	assert.Equal(t, before, f.matches, "not sent")
}

// An outage and an answer that cannot be read decide nothing, and no error
// carries the national ID, the date, the card, or the IBAN (#158).
func TestJibit_BankChecksNeverDecideOnFailures(t *testing.T) {
	ctx := context.Background()
	f, j := newFakeJibit(t)
	person := Person{NationalID: testNationalID, BirthDate: testBirth}
	check := func(err error) {
		t.Helper()
		require.ErrorIs(t, err, ErrUnavailable)
		for _, secret := range []string{testNationalID, testCard, "13700525", "1991"} {
			assert.NotContains(t, err.Error(), secret)
		}
	}
	f.failMatches = jibitAttempts
	_, err := j.MatchBank(ctx, person, BankAccount{Kind: "card", Number: testCard})
	check(err)
	f.matchBody = `{"matched":"maybe"}`
	_, err = j.Identify(ctx, person)
	check(err)
	f.matchBody = `{}`
	_, err = j.MatchBank(ctx, person, BankAccount{Kind: "iban", Number: testIBAN})
	check(err)
}

// The Persian calendar of known dates.
func TestJalali(t *testing.T) {
	for gregorian, persian := range map[string]string{
		"1979-02-11": "13571122", // 22 Bahman 1357
		"2000-01-01": "13781011",
		"2024-03-20": "14030101", // Nowruz 1403
		"2024-03-19": "14021229", // the last day of 1402, a leap year
		"1991-08-16": "13700525",
		"2025-03-21": "14040101", // Nowruz 1404
	} {
		d, err := time.Parse(time.DateOnly, gregorian)
		require.NoError(t, err)
		assert.Equal(t, persian, jalali(d), gregorian)
	}
}

func TestParseBirthDate(t *testing.T) {
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	d, err := ParseBirthDate("1991-08-16", now)
	require.NoError(t, err)
	assert.Equal(t, testBirth, d)
	for _, bad := range []string{"16/08/1991", "1991-02-30", "2027-01-01", "1890-01-01", ""} {
		_, err := ParseBirthDate(bad, now)
		assert.Error(t, err, bad)
	}
}
