package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testKavenegarKey = "4F6B2D7A9C1E3B5D"

type kavenegarRequest struct {
	method, path, contentType string
	form                      url.Values
}

// fakeKavenegar answers every request with status and body, and records the
// requests.
func fakeKavenegar(t *testing.T, status int, body string) (*httptest.Server, func() []kavenegarRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []kavenegarRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		mu.Lock()
		requests = append(requests, kavenegarRequest{
			method:      r.Method,
			path:        r.URL.Path,
			contentType: r.Header.Get("Content-Type"),
			form:        r.PostForm,
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, func() []kavenegarRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]kavenegarRequest(nil), requests...)
	}
}

func newTestKavenegar(t *testing.T, baseURL string) *KavenegarDeliverer {
	t.Helper()
	k, err := NewKavenegarDeliverer(config.KavenegarConfig{
		APIKey:      testKavenegarKey,
		OTPTemplate: "signin",
		BaseURL:     baseURL,
		Timeout:     5,
	})
	require.NoError(t, err)
	return k
}

var testSMSMessage = OTPMessage{
	ContactType:   ContactMobile,
	Contact:       "09121234567",
	MaskedContact: "091*****567",
	Code:          "482913",
}

func TestKavenegarDeliverer_SendsTheCode(t *testing.T) {
	server, requests := fakeKavenegar(t, http.StatusOK,
		`{"return":{"status":200,"message":"OK"},"entries":[{"messageid":8792343,"status":5}]}`)
	k := newTestKavenegar(t, server.URL+"/")

	require.NoError(t, k.Deliver(context.Background(), testSMSMessage))

	got := requests()
	require.Len(t, got, 1)
	assert.Equal(t, http.MethodPost, got[0].method)
	assert.Equal(t, "/v1/"+testKavenegarKey+"/verify/lookup.json", got[0].path)
	assert.Equal(t, "application/x-www-form-urlencoded", got[0].contentType)
	assert.Equal(t, "09121234567", got[0].form.Get("receptor"))
	assert.Equal(t, "482913", got[0].form.Get("token"))
	assert.Equal(t, "signin", got[0].form.Get("template"))
}

func TestKavenegarDeliverer_ErrorStatuses(t *testing.T) {
	cases := []struct {
		name          string
		httpStatus    int
		body          string
		wantPermanent bool
		wantText      string
	}{
		{"insufficient credit", 418, `{"return":{"status":418,"message":"credit"},"entries":null}`, true, "insufficient account credit"},
		{"invalid recipient", 411, `{"return":{"status":411,"message":"receptor"},"entries":null}`, true, "invalid recipient"},
		{"unknown template", 424, `{"return":{"status":424,"message":"template"},"entries":null}`, true, "not approved"},
		{"server error", 500, `{"return":{"status":500,"message":"error"},"entries":null}`, false, "status 500"},
		{"rate limited", 429, `{"return":{"status":429,"message":"slow down"},"entries":null}`, false, "status 429"},
		{"unreadable server error", 502, `<html>Bad Gateway</html>`, false, "HTTP status 502"},
		{"unreadable client error", 404, `<html>Not Found</html>`, true, "HTTP status 404"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := fakeKavenegar(t, tc.httpStatus, tc.body)
			k := newTestKavenegar(t, server.URL)

			err := k.Deliver(context.Background(), testSMSMessage)

			require.Error(t, err)
			assert.Equal(t, tc.wantPermanent, IsPermanent(err))
			assert.Contains(t, err.Error(), tc.wantText)
			assert.NotContains(t, err.Error(), testKavenegarKey)
		})
	}
}

func TestKavenegarDeliverer_NetworkErrorsHideTheAPIKey(t *testing.T) {
	server, _ := fakeKavenegar(t, http.StatusOK, `{}`)
	baseURL := server.URL
	server.Close()
	k := newTestKavenegar(t, baseURL)

	err := k.Deliver(context.Background(), testSMSMessage)

	require.Error(t, err)
	assert.False(t, IsPermanent(err), "network errors are retried")
	assert.NotContains(t, err.Error(), testKavenegarKey, "the API key is part of the URL and must not be logged")
	assert.NotContains(t, err.Error(), "verify/lookup")
}

func TestKavenegarDeliverer_HonorsTheContext(t *testing.T) {
	server, requests := fakeKavenegar(t, http.StatusOK, `{"return":{"status":200}}`)
	k := newTestKavenegar(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := k.Deliver(ctx, testSMSMessage)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), testKavenegarKey)
	assert.Empty(t, requests())
}

// TestKavenegarDeliverer_WithdrawalCodesHaveTheirOwnTemplate checks that a
// code that confirms a withdrawal goes out with its own template and what
// it confirms (#161), and never with the sign-in template.
func TestKavenegarDeliverer_WithdrawalCodesHaveTheirOwnTemplate(t *testing.T) {
	server, requests := fakeKavenegar(t, http.StatusOK, `{"return":{"status":200,"message":"OK"}}`)
	withdrawal := testSMSMessage
	withdrawal.Purpose, withdrawal.Details = PurposeWithdrawal, "100 USDT to TR7NHq...gjLj6t"

	k := newTestKavenegar(t, server.URL)
	err := k.Deliver(context.Background(), withdrawal)
	require.Error(t, err, "without a withdrawal template, the code is not sent with the sign-in one")
	assert.True(t, IsPermanent(err))
	assert.Empty(t, requests())

	k, err = NewKavenegarDeliverer(config.KavenegarConfig{APIKey: testKavenegarKey, OTPTemplate: "signin",
		WithdrawalTemplate: "withdraw", BaseURL: server.URL, Timeout: 5})
	require.NoError(t, err)
	require.NoError(t, k.Deliver(context.Background(), withdrawal))
	got := requests()
	require.Len(t, got, 1)
	assert.Equal(t, "withdraw", got[0].form.Get("template"))
	assert.Equal(t, "482913", got[0].form.Get("token"))
	assert.Equal(t, "100 USDT to TR7NHq...gjLj6t", got[0].form.Get("token10"))
}

// TestKavenegarDeliverer_RemindersHaveTheirOwnTemplate checks that the
// reminder to change the anti-phishing code (#375) goes out with its own
// template and the number of emails that showed the code, never the code.
func TestKavenegarDeliverer_RemindersHaveTheirOwnTemplate(t *testing.T) {
	server, requests := fakeKavenegar(t, http.StatusOK, `{"return":{"status":200,"message":"OK"}}`)
	reminder := OTPMessage{ContactType: ContactMobile, Contact: testSMSMessage.Contact, MaskedContact: testSMSMessage.MaskedContact,
		Purpose: PurposeAntiPhishingReminder, AntiPhishingCode: "blue-falcon", Uses: 212, AgeDays: 190}

	k := newTestKavenegar(t, server.URL)
	err := k.Deliver(context.Background(), reminder)
	require.Error(t, err, "without a reminder template, the reminder is not sent")
	assert.True(t, IsPermanent(err))
	assert.Empty(t, requests())

	k, err = NewKavenegarDeliverer(config.KavenegarConfig{APIKey: testKavenegarKey, OTPTemplate: "signin",
		AntiPhishingReminderTemplate: "antiphishing", BaseURL: server.URL, Timeout: 5})
	require.NoError(t, err)
	require.NoError(t, k.Deliver(context.Background(), reminder))
	got := requests()
	require.Len(t, got, 1)
	assert.Equal(t, "antiphishing", got[0].form.Get("template"))
	assert.Equal(t, "212", got[0].form.Get("token"))
	for key, values := range got[0].form {
		assert.NotContains(t, strings.Join(values, ","), "blue-falcon", "an SMS never carries the code: %s", key)
	}
}
