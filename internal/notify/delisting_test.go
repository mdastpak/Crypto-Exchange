package notify

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// delistingNotice is the notice that a holder gets when deposits close.
var delistingNotice = OTPMessage{ContactType: ContactEmail, Contact: "alice@example.com", MaskedContact: "ali*****@example.com",
	Purpose: PurposeDelistingNotice, AntiPhishingCode: "blue-falcon",
	Delisting: DelistingNotice{Currency: "XYZ", Stage: "deposits_disabled", Deadline: "2026-12-01 12:00 UTC", DaysLeft: 16,
		Holder: true, Settlement: "convert", SettlementCurrency: "USDT"}}

// TestEmailDeliverer_SendsTheDelistingNotices checks the email of each
// stage of a delisting (#216), in the text and in both HTML templates: the
// currency, the deadline, the settlement, the anti-phishing code, and no
// link.
func TestEmailDeliverer_SendsTheDelistingNotices(t *testing.T) {
	server := startFakeSMTPServer(t, "")
	for lang, title := range map[string]string{HTMLTemplatesEN: "XYZ is being delisted", HTMLTemplatesFA: "در حال حذف از صرافی است"} {
		cfg := server.emailConfig()
		cfg.HTMLTemplates = lang
		d := newTestEmailDeliverer(t, cfg)
		require.NoError(t, d.Deliver(context.Background(), delistingNotice))
		received := server.received()
		parsed, err := mail.ReadMessage(strings.NewReader(received[len(received)-1].data))
		require.NoError(t, err)
		subject, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
		require.NoError(t, err)
		assert.Equal(t, "XYZ is being delisted", subject)
		text, html := emailParts(t, parsed)
		assert.Contains(t, text, "Deposits of XYZ are now closed.")
		assert.Contains(t, text, "It is frozen on 2026-12-01 12:00 UTC, in 16 day(s).")
		assert.Contains(t, text, "converted into USDT")
		assert.Contains(t, text, "You hold XYZ")
		assert.Contains(t, text, "blue-falcon", "every email shows the anti-phishing code")
		assert.Contains(t, html, title)
		assert.Contains(t, html, "<b>blue-falcon</b>")
		assert.Contains(t, html, "2026-12-01 12:00 UTC")
		assert.Contains(t, html, "USDT")
		assert.NotContains(t, html, "href", "%s: a notice has no link", lang)
	}

	// The cancel says the currency stays, with no deadline or holding.
	cancelled := delistingNotice
	cancelled.Delisting = DelistingNotice{Currency: "XYZ", Stage: DelistingStageCancelled}
	d := newTestEmailDeliverer(t, server.emailConfig())
	require.NoError(t, d.Deliver(context.Background(), cancelled))
	received := server.received()
	parsed, err := mail.ReadMessage(strings.NewReader(received[len(received)-1].data))
	require.NoError(t, err)
	assert.Equal(t, "XYZ stays listed", parsed.Header.Get("Subject"))
	text, _ := emailParts(t, parsed)
	assert.Contains(t, text, "The delisting of XYZ is cancelled")
	assert.NotContains(t, text, "frozen on")

	// A reminder counts the days, and another user is not told they hold it.
	reminder := delistingNotice
	reminder.Delisting.Stage, reminder.Delisting.DaysLeft, reminder.Delisting.Holder = DelistingStageReminder, 7, false
	require.NoError(t, d.Deliver(context.Background(), reminder))
	received = server.received()
	parsed, err = mail.ReadMessage(strings.NewReader(received[len(received)-1].data))
	require.NoError(t, err)
	text, _ = emailParts(t, parsed)
	assert.Contains(t, text, "7 day(s) are left before XYZ is frozen.")
	assert.NotContains(t, text, "You hold")
}

// TestEmailDeliverer_RefusesADelistingTemplateThatFails checks that a
// template of the notices that fails at one stage stops the server at
// startup.
func TestEmailDeliverer_RefusesADelistingTemplateThatFails(t *testing.T) {
	server := startFakeSMTPServer(t, "")
	cfg := server.emailConfig()
	cfg.DelistingNoticeBody = `{{if eq .Stage "frozen"}}{{.Unknown}}{{end}}`
	_, err := NewEmailDeliverer(cfg, 90*time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delisting_notice_body")
}

// TestSMS_DelistingNoticesHaveTheirOwnTemplate checks that the notice of a
// delisting goes by SMS only with its own template, with the symbol and
// the date of the freeze.
func TestSMS_DelistingNoticesHaveTheirOwnTemplate(t *testing.T) {
	notice := delistingNotice
	notice.ContactType, notice.Contact, notice.MaskedContact = ContactMobile, testSMSMessage.Contact, testSMSMessage.MaskedContact

	server, requests := fakeKavenegar(t, http.StatusOK, `{"return":{"status":200,"message":"OK"}}`)
	k := newTestKavenegar(t, server.URL)
	assert.False(t, k.Supports(PurposeDelistingNotice))
	err := k.Deliver(context.Background(), notice)
	require.Error(t, err)
	assert.True(t, IsPermanent(err), "never with the sign-in text")
	k, err = NewKavenegarDeliverer(config.KavenegarConfig{APIKey: testKavenegarKey, OTPTemplate: "signin",
		DelistingNoticeTemplate: "delisting", BaseURL: server.URL, Timeout: 5})
	require.NoError(t, err)
	assert.True(t, k.Supports(PurposeDelistingNotice))
	require.NoError(t, k.Deliver(context.Background(), notice))
	got := requests()
	require.Len(t, got, 1)
	assert.Equal(t, "delisting", got[0].form.Get("template"))
	assert.Equal(t, "XYZ", got[0].form.Get("token"))
	assert.Equal(t, "2026-12-01", got[0].form.Get("token2"))
	for key, values := range got[0].form {
		assert.NotContains(t, strings.Join(values, ","), "blue-falcon", "an SMS never carries the code: %s", key)
	}

	ir, irRequests := fakeSMSIR(t, http.StatusOK, smsirOK, nil)
	s := newTestSMSIR(t, ir.URL+"/")
	assert.False(t, s.Supports(PurposeDelistingNotice))
	s, err = NewSMSIRDeliverer(config.SMSIRConfig{APIKey: testSMSIRKey, OTPTemplateID: 1, DelistingNoticeTemplateID: 444,
		BaseURL: ir.URL + "/", Timeout: 5})
	require.NoError(t, err)
	_, err = s.Send(context.Background(), notice)
	require.NoError(t, err)
	sent := irRequests()
	require.Len(t, sent, 1)
	assert.EqualValues(t, 444, sent[0].body["templateId"])
	assert.Equal(t, []any{map[string]any{"name": "CURRENCY", "value": "XYZ"}, map[string]any{"name": "DATE", "value": "2026-12-01"}},
		sent[0].body["parameters"])

	notice.Delisting.Deadline = ""
	assert.Equal(t, "-", deadlineDate(notice.Delisting), "a parameter is never empty")
}

// TestQueueSender_DelistingNoticesReachTheWorkerWhole checks that the
// notice crosses the queue with all it says, and waits a day.
func TestQueueSender_DelistingNoticesReachTheWorkerWhole(t *testing.T) {
	publisher := &recordingPublisher{}
	sender := NewQueueSender(publisher, "notifications", emailOnlyChannels(), 90*time.Second)
	require.NoError(t, sender.SendOTP(context.Background(), delistingNotice))
	published := publisher.messages()
	require.Len(t, published, 1)
	assert.Equal(t, "86400000", published[0].msg.Expiration)
	var job otpJob
	require.NoError(t, json.Unmarshal(published[0].msg.Body, &job))
	assert.Equal(t, delistingNotice, job.message())

	// Another message carries no notice of a delisting.
	require.NoError(t, sender.SendOTP(context.Background(), OTPMessage{ContactType: ContactEmail, Contact: "a@example.com", Code: "1"}))
	published = publisher.messages()
	assert.NotContains(t, string(published[1].msg.Body), `"delisting"`)
}
