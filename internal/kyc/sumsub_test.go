package kyc

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/verification"
)

const (
	appToken      = "sbx:app-token"
	secretKey     = "secret-key"
	webhookSecret = "webhook-secret"
)

// fakeSumsub checks the signature of every request as Sumsub does, and
// answers as its documentation shows.
type fakeSumsub struct {
	mu         sync.Mutex
	t          *testing.T
	applicants map[string]string // externalUserId -> applicant id
	requests   []string
}

func (f *fakeSumsub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	mac := hmac.New(sha256.New, []byte(secretKey))
	mac.Write([]byte(r.Header.Get("X-App-Access-Ts") + r.Method + r.URL.RequestURI()))
	mac.Write(body)
	if r.Header.Get("X-App-Token") != appToken || r.Header.Get("X-App-Access-Sig") != hex.EncodeToString(mac.Sum(nil)) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"description":"Request signature mismatch","code":401}`))
		return
	}
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/resources/applicants":
		assert.Equal(f.t, "basic-kyc-level", r.URL.Query().Get("levelName"))
		var in struct {
			ExternalUserID string `json:"externalUserId"`
		}
		_ = json.Unmarshal(body, &in)
		if _, ok := f.applicants[in.ExternalUserID]; ok {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"description":"Applicant with external user id already exists","code":409}`))
			return
		}
		f.applicants[in.ExternalUserID] = "6650d1e7b1" + strconv.Itoa(len(f.applicants))
		_, _ = w.Write([]byte(`{"id":"` + f.applicants[in.ExternalUserID] + `","externalUserId":"` + in.ExternalUserID + `"}`))
	case r.Method == http.MethodGet && len(r.URL.Path) > len("/resources/applicants/-;externalUserId="):
		ext := r.URL.Path[len("/resources/applicants/-;externalUserId=") : len(r.URL.Path)-len("/one")]
		_, _ = w.Write([]byte(`{"id":"` + f.applicants[ext] + `"}`))
	case r.Method == http.MethodPost && r.URL.Path == "/resources/accessTokens":
		_, _ = w.Write([]byte(`{"token":"_act-` + r.URL.Query().Get("userId") + `","userId":"` + r.URL.Query().Get("userId") + `"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newSumsub(t *testing.T) (*Sumsub, *fakeSumsub) {
	t.Helper()
	fake := &fakeSumsub{t: t, applicants: map[string]string{}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return &Sumsub{BaseURL: server.URL, AppToken: appToken, SecretKey: secretKey, WebhookSecret: webhookSecret,
		Level: "basic-kyc-level", Client: server.Client()}, fake
}

// signed returns the headers of a webhook signed by Sumsub.
func signed(body []byte, alg string) http.Header {
	h := http.Header{}
	var mac = hmac.New(sha256.New, []byte(webhookSecret))
	if alg == "HMAC_SHA512_HEX" {
		mac = hmac.New(sha512.New, []byte(webhookSecret))
	}
	mac.Write(body)
	h.Set("X-Payload-Digest", hex.EncodeToString(mac.Sum(nil)))
	if alg != "" {
		h.Set("X-Payload-Digest-Alg", alg)
	}
	return h
}

func TestSumsub_StartAndSession(t *testing.T) {
	ctx := context.Background()
	s, fake := newSumsub(t)
	id, err := s.Start(ctx, "app-1")
	require.NoError(t, err)
	assert.Equal(t, fake.applicants["app-1"], id)
	again, err := s.Start(ctx, "app-1")
	require.NoError(t, err)
	assert.Equal(t, id, again, "a retry finds the applicant it created")

	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	session, err := s.Session(ctx, "app-1")
	require.NoError(t, err)
	assert.Equal(t, "_act-app-1", session.Token)
	assert.Equal(t, now.Add(10*time.Minute), session.ExpiresAt)

	s.SecretKey = "wrong"
	_, err = s.Session(ctx, "app-1")
	assert.ErrorContains(t, err, "status 401", "the requests are signed")
}

func TestSumsub_Verify(t *testing.T) {
	s, _ := newSumsub(t)
	reviewed := func(answer, kind string) []byte {
		return []byte(`{"applicantId":"66a","externalUserId":"app-1","type":"applicantReviewed","reviewStatus":"completed",` +
			`"reviewResult":{"reviewAnswer":"` + answer + `","rejectLabels":["FORGERY"],"reviewRejectType":"` + kind + `"}}`)
	}
	for name, c := range map[string]struct {
		body []byte
		want Outcome
	}{
		"pending":  {[]byte(`{"applicantId":"66a","externalUserId":"app-1","type":"applicantPending"}`), OutcomePending},
		"approved": {reviewed("GREEN", ""), OutcomeApproved},
		"final":    {reviewed("RED", "FINAL"), OutcomeRejected},
		"retry":    {reviewed("RED", "RETRY"), OutcomeRetry},
		"other":    {[]byte(`{"applicantId":"66a","externalUserId":"app-1","type":"applicantCreated"}`), OutcomeNone},
	} {
		t.Run(name, func(t *testing.T) {
			v, err := s.Verify(signed(c.body, ""), c.body)
			require.NoError(t, err)
			assert.Equal(t, c.want, v.Outcome)
			assert.Equal(t, "app-1", v.Ref)
			assert.Equal(t, "66a", v.ProviderRef)
		})
	}

	body := reviewed("RED", "FINAL")
	v, err := s.Verify(signed(body, "HMAC_SHA512_HEX"), body)
	require.NoError(t, err, "by the algorithm that the header names")
	assert.Equal(t, []string{"FORGERY"}, v.Labels)

	h := signed(body, "")
	_, err = s.Verify(h, reviewed("GREEN", ""))
	assert.ErrorIs(t, err, ErrWebhookSignature, "a body changed after it was signed")
	_, err = s.Verify(http.Header{}, body)
	assert.ErrorIs(t, err, ErrWebhookSignature, "no signature")
	h.Set("X-Payload-Digest-Alg", "MD5")
	_, err = s.Verify(h, body)
	assert.ErrorIs(t, err, ErrWebhookSignature, "an unknown algorithm")
	s.WebhookSecret = ""
	_, err = s.Verify(signed(body, ""), body)
	assert.ErrorIs(t, err, ErrWebhookSignature, "without a secret, nothing is verified")
}

// An application verified by Sumsub: the documents go to its SDK, and its
// signed webhooks move the application, until an approval grants level 2
// (#72).
func TestProviderFlow(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	provider, fake := newSumsub(t)
	f.Provider = provider

	a, err := f.Apply(ctx, f.user, form, userOrigin)
	require.NoError(t, err)
	assert.Equal(t, ProviderSumsub, a.Provider)
	applicant := fake.applicants[a.ID]
	require.NotEmpty(t, applicant, "the applicant is named by the application")

	_, err = f.Upload(ctx, f.user, a.ID, DocSelfie, jpeg, userOrigin)
	assert.ErrorIs(t, err, ErrProviderFlow, "the documents go to the provider")
	_, err = f.Submit(ctx, f.user, a.ID, userOrigin)
	assert.ErrorIs(t, err, ErrProviderFlow)
	session, err := f.Session(ctx, f.user)
	require.NoError(t, err)
	assert.Equal(t, "_act-"+a.ID, session.Token)

	hook := func(kind, answer, reject, applicantID string) error {
		body := []byte(`{"applicantId":"` + applicantID + `","externalUserId":"` + a.ID + `","type":"` + kind + `",` +
			`"reviewResult":{"reviewAnswer":"` + answer + `","rejectLabels":["BAD_SELFIE"],"reviewRejectType":"` + reject + `"}}`)
		return f.Webhook(ctx, signed(body, ""), body, audit.Event{IP: "203.0.113.200"})
	}
	status := func() Status {
		got, err := f.read(ctx, f.DB, a.ID)
		require.NoError(t, err)
		return got.Status
	}

	body := []byte(`{"applicantId":"` + applicant + `","externalUserId":"` + a.ID + `","type":"applicantReviewed","reviewResult":{"reviewAnswer":"GREEN"}}`)
	assert.ErrorIs(t, f.Webhook(ctx, http.Header{"X-Payload-Digest": {"00"}}, body, audit.Event{}), ErrWebhookSignature)
	assert.Equal(t, StatusPending, status(), "an unsigned webhook changes nothing")
	require.NoError(t, hook("applicantReviewed", "GREEN", "", "another-applicant"))
	assert.Equal(t, StatusPending, status(), "a webhook of another applicant changes nothing")

	require.NoError(t, hook("applicantPending", "", "", applicant))
	assert.Equal(t, StatusUnderReview, status())
	require.NoError(t, hook("applicantReviewed", "RED", "RETRY", applicant))
	assert.Equal(t, StatusPending, status(), "the user may send new documents")
	require.NoError(t, hook("applicantPending", "", "", applicant))
	require.NoError(t, hook("applicantReviewed", "GREEN", "", applicant))
	assert.Equal(t, StatusApproved, status())
	assert.Equal(t, verification.Level2, f.level(f.user))
	require.NoError(t, hook("applicantReviewed", "RED", "FINAL", applicant), "a webhook of a decided application is acknowledged")
	assert.Equal(t, StatusApproved, status(), "and changes nothing")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM audit_events WHERE action = $1 AND actor_type = 'system'
		AND changes->'after'->>'level' = '2'`, audit.ActionUserVerificationSet), "level 2, granted by the system")
	assert.Equal(t, 4, f.count(`SELECT COUNT(*) FROM audit_events WHERE action = $1`, audit.ActionKYCProviderVerdict))
	var reviewer sql.NullString
	require.NoError(t, f.DB.QueryRow(`SELECT reviewed_by::text FROM kyc_applications WHERE id = $1`, a.ID).Scan(&reviewer))
	assert.False(t, reviewer.Valid, "decided by the provider, not an administrator")

	// A final rejection: the user reads a general reason, the labels are
	// for the administrators.
	other := f.newUser("other@example.com", verification.Level1)
	b, err := f.Apply(ctx, other, form, userOrigin)
	require.NoError(t, err)
	a = b
	require.NoError(t, hook("applicantReviewed", "RED", "FINAL", fake.applicants[b.ID]))
	rejected, err := f.Current(ctx, other)
	require.NoError(t, err)
	assert.Equal(t, StatusRejected, rejected.Status)
	assert.Equal(t, rejectedByProvider, rejected.Reason)
	review, err := f.Detail(ctx, b.ID, f.adminOrigin())
	require.NoError(t, err)
	assert.Equal(t, "BAD_SELFIE", review.ReviewNotes)
	assert.Equal(t, verification.Level1, f.level(other))
}
