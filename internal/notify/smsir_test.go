package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSMSIRKey = "smsir-7Q2v9KxLm3"

type smsirRequest struct {
	method, path, key, contentType string
	body                           map[string]any
}

// fakeSMSIR answers verify requests with status and body, and reports with
// the delivery states in reports, and records the requests.
func fakeSMSIR(t *testing.T, status int, body string, reports map[string]string) (*httptest.Server, func() []smsirRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []smsirRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := smsirRequest{method: r.Method, path: r.URL.Path, key: r.Header.Get("X-API-KEY"), contentType: r.Header.Get("Content-Type")}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &req.body)
		mu.Lock()
		requests = append(requests, req)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if id, ok := strings.CutPrefix(r.URL.Path, "/v1/send/"); ok && r.Method == http.MethodGet {
			state, known := reports[id]
			if !known {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"status":111,"message":"not registered","data":null}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":1,"message":"ok","data":{"messageId":` + id + `,"deliveryState":` + state + `}}`))
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, func() []smsirRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]smsirRequest(nil), requests...)
	}
}

func newTestSMSIR(t *testing.T, baseURL string) *SMSIRDeliverer {
	t.Helper()
	s, err := NewSMSIRDeliverer(config.SMSIRConfig{APIKey: testSMSIRKey, OTPTemplateID: 123456, WithdrawalTemplateID: 222,
		AntiPhishingReminderTemplateID: 333, BaseURL: baseURL, Timeout: 5})
	require.NoError(t, err)
	return s
}

const smsirOK = `{"status":1,"message":"OK","data":{"messageId":89545112,"cost":1.0}}`

func TestSMSIRDeliverer_SendsEachTemplateWithItsParameters(t *testing.T) {
	server, requests := fakeSMSIR(t, http.StatusOK, smsirOK, nil)
	s := newTestSMSIR(t, server.URL+"/")
	ctx := context.Background()

	id, err := s.Send(ctx, testSMSMessage)
	require.NoError(t, err)
	assert.Equal(t, "89545112", id)
	got := requests()
	require.Len(t, got, 1)
	assert.Equal(t, http.MethodPost, got[0].method)
	assert.Equal(t, "/v1/send/verify", got[0].path)
	assert.Equal(t, testSMSIRKey, got[0].key)
	assert.Equal(t, "application/json", got[0].contentType)
	assert.Equal(t, "9121234567", got[0].body["mobile"], "ten digits, without the leading 0")
	assert.EqualValues(t, 123456, got[0].body["templateId"])
	assert.Equal(t, []any{map[string]any{"name": "CODE", "value": "482913"}}, got[0].body["parameters"])

	withdrawal := testSMSMessage
	withdrawal.Purpose, withdrawal.Amount, withdrawal.Destination = PurposeWithdrawal, "100 USDT", "TR7NHq...gjLj6t"
	_, err = s.Send(ctx, withdrawal)
	require.NoError(t, err)
	got = requests()
	assert.EqualValues(t, 222, got[1].body["templateId"])
	assert.Equal(t, []any{
		map[string]any{"name": "CODE", "value": "482913"},
		map[string]any{"name": "AMOUNT", "value": "100 USDT"},
		map[string]any{"name": "DEST", "value": "TR7NHq...gjLj6t"},
	}, got[1].body["parameters"])

	reminder := OTPMessage{ContactType: ContactMobile, Contact: "+989121234567", Purpose: PurposeAntiPhishingReminder,
		AntiPhishingCode: "blue falcon", Uses: 212}
	_, err = s.Send(ctx, reminder)
	require.NoError(t, err)
	got = requests()
	assert.Equal(t, "9121234567", got[2].body["mobile"])
	assert.Equal(t, []any{map[string]any{"name": "COUNT", "value": "212"}}, got[2].body["parameters"], "never the code")

	// A value over 25 characters is never sent.
	withdrawal.Destination = strings.Repeat("x", 26)
	_, err = s.Send(ctx, withdrawal)
	require.Error(t, err)
	assert.True(t, IsProviderError(err), "another provider may take it")
	assert.Len(t, requests(), 3)
}

func TestSMSIRDeliverer_ClassifiesFailures(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name              string
		http, status      int
		provider, lasting bool
	}{
		{"invalid mobile", 400, 104, false, true},
		{"blacklisted mobile", 400, 115, false, true},
		{"insufficient credit", 400, 102, true, true},
		{"invalid key", 401, 10, true, true},
		{"template not found", 400, 113, true, true},
		{"too many requests", 429, 20, true, false},
		{"system error", 500, 0, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := fakeSMSIR(t, tc.http, `{"status":`+strconv.Itoa(tc.status)+`,"message":"x","data":null}`, nil)
			_, err := newTestSMSIR(t, server.URL).Send(ctx, testSMSMessage)
			require.Error(t, err)
			assert.Equal(t, tc.provider, IsProviderError(err))
			assert.Equal(t, tc.lasting, IsPermanent(err))
			assert.NotContains(t, err.Error(), testSMSIRKey)
		})
	}
	server, _ := fakeSMSIR(t, http.StatusBadGateway, `<html>Bad Gateway</html>`, nil)
	_, err := newTestSMSIR(t, server.URL).Send(ctx, testSMSMessage)
	require.Error(t, err)
	assert.True(t, IsProviderError(err))
	assert.False(t, IsPermanent(err))
}

func TestSMSIRDeliverer_ReadsTheDeliveryReports(t *testing.T) {
	server, requests := fakeSMSIR(t, http.StatusOK, smsirOK, map[string]string{
		"1": "1", "2": "2", "3": "3", "4": "null", "6": "6", "7": "7",
	})
	s := newTestSMSIR(t, server.URL)
	states, err := s.Reports(context.Background(), []string{"1", "2", "3", "4", "5", "6", "7", "not-a-number"})
	require.NoError(t, err)
	assert.Equal(t, map[string]DeliveryState{
		"1": StateDelivered, "2": StateFailed, "3": StatePending, "4": StatePending,
		"5": StateUnknown, "6": StateFailed, "7": StateFailed,
	}, states, "5 is not recorded, as a sandbox message is not")
	for _, r := range requests() {
		assert.Equal(t, http.MethodGet, r.method)
		assert.Equal(t, testSMSIRKey, r.key)
	}
	assert.Len(t, requests(), 7, "an ID that is not one is not asked for")
}

func TestNewSMSIRDeliverer_RefusesAnIncompleteConfiguration(t *testing.T) {
	for name, cfg := range map[string]config.SMSIRConfig{
		"no key":      {OTPTemplateID: 1, BaseURL: config.DefaultSMSIRBaseURL},
		"no template": {APIKey: "k", BaseURL: config.DefaultSMSIRBaseURL},
		"relative":    {APIKey: "k", OTPTemplateID: 1, BaseURL: "api.sms.ir"},
	} {
		_, err := NewSMSIRDeliverer(cfg)
		assert.Error(t, err, name)
	}
	s, err := NewSMSIRDeliverer(config.SMSIRConfig{APIKey: "k", OTPTemplateID: 1, BaseURL: config.DefaultSMSIRBaseURL})
	require.NoError(t, err)
	assert.True(t, s.Supports(""))
	assert.False(t, s.Supports(PurposeWithdrawal), "without a withdrawal template, withdrawal codes go elsewhere")
	assert.False(t, s.Supports(PurposeAntiPhishingReminder))
}
