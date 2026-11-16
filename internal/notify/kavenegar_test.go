package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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

// fakeKavenegarLimits are the limits of the tokens, as Kavenegar documents
// them (#459), written out apart from the deliverer's own check: spaces per
// token, and 100 characters.
var fakeKavenegarLimits = map[string]int{"token": 0, "token2": 0, "token3": 0, "token10": 5, "token20": 8}

// fakeKavenegar answers every request with status and body, and records the
// requests. Like the real API, it refuses with 431 a token over its limits.
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
		for name, spaces := range fakeKavenegarLimits {
			if v := r.PostForm.Get(name); strings.Count(v, " ") > spaces || len([]rune(v)) > 100 || strings.ContainsAny(v, "\r\n") {
				w.WriteHeader(431)
				_, _ = w.Write([]byte(`{"return":{"status":431,"message":"invalid token format"}}`))
				return
			}
		}
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

// TestKavenegarDeliverer_TheLongestWithdrawalFits sends a withdrawal code
// with the longest amount, destination, and fee that a confirmation allows,
// 25 characters each, through a Kavenegar that enforces its limits (#459):
// %token10 holds at most five spaces and 100 characters.
func TestKavenegarDeliverer_TheLongestWithdrawalFits(t *testing.T) {
	server, requests := fakeKavenegar(t, http.StatusOK, `{"return":{"status":200,"message":"OK"},"entries":[{"messageid":7}]}`)
	k, err := NewKavenegarDeliverer(config.KavenegarConfig{APIKey: testKavenegarKey, OTPTemplate: "signin",
		WithdrawalTemplate: "withdraw", BaseURL: server.URL, Timeout: 5})
	require.NoError(t, err)

	withdrawal := testSMSMessage
	withdrawal.Purpose = PurposeWithdrawal
	withdrawal.Amount = "1234567890.12345678 USDTX"
	withdrawal.Destination = "TR7NHqjeKQxGTCi8q8ZY4p..."
	withdrawal.Fee = "123456.123456789012 USDTX"
	withdrawal.Details = withdrawal.Amount + " to " + withdrawal.Destination + " (fee " + withdrawal.Fee + ")"
	withdrawal.DenyToken = "d8Kx2Qm9"
	require.Len(t, withdrawal.Amount, 25)
	require.Len(t, withdrawal.Destination, 25)
	require.Len(t, withdrawal.Fee, 25)
	require.Equal(t, 6, strings.Count(withdrawal.Details, " "), "the text of the message, which %token10 cannot take")

	require.NoError(t, k.Deliver(context.Background(), withdrawal))
	got := requests()
	require.Len(t, got, 1)
	token10 := got[0].form.Get("token10")
	assert.Equal(t, withdrawal.Amount+" to "+withdrawal.Destination+" (fee:"+withdrawal.Fee+")", token10)
	assert.Equal(t, 5, strings.Count(token10, " "))
	assert.LessOrEqual(t, len([]rune(token10)), 100)

	// A destination with spaces loses them, and a message without the fee
	// shows none.
	withdrawal.Destination, withdrawal.Fee = "IR12 0000 1234", ""
	require.NoError(t, k.Deliver(context.Background(), withdrawal))
	assert.Equal(t, withdrawal.Amount+" to IR1200001234", requests()[1].form.Get("token10"))

	// A message queued before the parts were set keeps its text, and is
	// refused here if it breaks the limits, never sent to fail there.
	withdrawal.Amount, withdrawal.Destination = "", ""
	err = k.Deliver(context.Background(), withdrawal)
	require.Error(t, err)
	assert.True(t, IsPermanent(err))
	assert.Contains(t, err.Error(), "token10 has 6 spaces")
	assert.Len(t, requests(), 2, "not sent")
}

// TestCheckKavenegarTokens checks each limit of the tokens (#459).
func TestCheckKavenegarTokens(t *testing.T) {
	for name, tc := range map[string]struct {
		token, value, want string
	}{
		"code without spaces":    {"token", "482913", ""},
		"code with a space":      {"token", "48 2913", "token has 1 spaces; Kavenegar allows 0"},
		"deny token with space":  {"token2", "a b", "token2 has 1 spaces"},
		"token3 with space":      {"token3", "a b", "token3 has 1 spaces"},
		"five spaces in token10": {"token10", "a b c d e f", ""},
		"six spaces in token10":  {"token10", "a b c d e f g", "token10 has 6 spaces; Kavenegar allows 5"},
		"eight in token20":       {"token20", "a b c d e f g h i", ""},
		"nine in token20":        {"token20", "a b c d e f g h i j", "token20 has 9 spaces"},
		"a line break":           {"token10", "a\nb", "token10 has a line break"},
		"100 characters":         {"token10", strings.Repeat("ب", 100), ""},
		"101 characters":         {"token10", strings.Repeat("ب", 101), "token10 has 101 characters; Kavenegar allows 100"},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkKavenegarTokens(url.Values{tc.token: {tc.value}, "receptor": {"0912 123 4567"}})
			if tc.want == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
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

// TestKavenegarDeliverer_ReportsDeliveryAndClassifiesFailures checks the
// message ID of a send, the delivery reports (#371), and which failures
// let another provider try.
func TestKavenegarDeliverer_ReportsDeliveryAndClassifiesFailures(t *testing.T) {
	var mu sync.Mutex
	var statusQueries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/verify/lookup.json"):
			_, _ = w.Write([]byte(`{"return":{"status":200,"message":"OK"},"entries":[{"messageid":8792343,"status":5}]}`))
		case strings.HasSuffix(r.URL.Path, "/sms/status.json"):
			mu.Lock()
			statusQueries = append(statusQueries, r.URL.Query().Get("messageid"))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"return":{"status":200,"message":"OK"},"entries":[
				{"messageid":1,"status":10},{"messageid":2,"status":11},{"messageid":3,"status":4},{"messageid":4,"status":100}]}`))
		}
	}))
	t.Cleanup(server.Close)
	k := newTestKavenegar(t, server.URL)
	ctx := context.Background()

	id, err := k.Send(ctx, testSMSMessage)
	require.NoError(t, err)
	assert.Equal(t, "8792343", id)

	states, err := k.Reports(ctx, []string{"1", "2", "3", "4"})
	require.NoError(t, err)
	assert.Equal(t, map[string]DeliveryState{"1": StateDelivered, "2": StateFailed, "3": StatePending, "4": StateUnknown}, states)
	assert.Equal(t, []string{"1,2,3,4"}, statusQueries, "one request for up to 500 IDs")

	for _, tc := range []struct {
		status            int
		provider, lasting bool
	}{
		{411, false, true}, // an invalid number: no other provider either
		{431, false, true},
		{418, true, true}, // no credit: the next provider
		{403, true, true},
		{424, true, true},
		{500, true, false},
		{429, true, false},
		{451, true, false},
	} {
		server, _ := fakeKavenegar(t, tc.status, `{"return":{"status":`+strconv.Itoa(tc.status)+`,"message":"x"},"entries":null}`)
		_, err := newTestKavenegar(t, server.URL).Send(ctx, testSMSMessage)
		require.Error(t, err)
		assert.Equal(t, tc.provider, IsProviderError(err), "status %d is a failure of the provider", tc.status)
		assert.Equal(t, tc.lasting, IsPermanent(err), "status %d is permanent", tc.status)
	}
}
