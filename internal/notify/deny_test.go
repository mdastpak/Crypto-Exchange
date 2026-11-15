package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDenyLink = "https://ex.example/d/Ab3xK9q2LmPq"

func deniableWithdrawal() OTPMessage {
	msg := testEmailMessage
	msg.Purpose, msg.Details = PurposeWithdrawal, "100 USDT to TR7NHq...gjLj6t"
	msg.Amount, msg.Destination = "100 USDT", "TR7NHq...gjLj6t"
	msg.DenyToken, msg.DenyLink = "Ab3xK9q2LmPq", testDenyLink
	return msg
}

// lastEmail returns the subject, the text, and the HTML of the last email
// that server received.
func lastEmail(t *testing.T, server *fakeSMTPServer) (subject, text, html string) {
	t.Helper()
	received := server.received()
	require.NotEmpty(t, received)
	msg, err := mail.ReadMessage(strings.NewReader(received[len(received)-1].data))
	require.NoError(t, err)
	text, html = emailParts(t, msg)
	return msg.Header.Get("Subject"), text, html
}

// TestEmailDeliverer_WithdrawalCodesCarryTheDenyLink: the text and the HTML
// of each language show the "this was not me" link (#374), and a text
// template of the operator's that does not show it gets it at its end.
func TestEmailDeliverer_WithdrawalCodesCarryTheDenyLink(t *testing.T) {
	ctx := context.Background()
	server := startFakeSMTPServer(t, "")
	for _, lang := range []string{HTMLTemplatesEN, HTMLTemplatesFA} {
		cfg := server.emailConfig()
		cfg.HTMLTemplates = lang
		require.NoError(t, newTestEmailDeliverer(t, cfg).Deliver(ctx, deniableWithdrawal()))
		_, text, html := lastEmail(t, server)
		assert.Contains(t, text, "Open this link to cancel it and secure your account", lang)
		assert.Equal(t, 1, strings.Count(text, testDenyLink), "once in the text, %s", lang)
		assert.Contains(t, html, `href="`+testDenyLink+`"`, lang)
	}
	_, _, html := lastEmail(t, server)
	assert.Contains(t, html, "این من نبودم")

	cfg := server.emailConfig()
	cfg.WithdrawalBody = "Your code is {{.Code}}.\n"
	require.NoError(t, newTestEmailDeliverer(t, cfg).Deliver(ctx, deniableWithdrawal()))
	_, text, _ := lastEmail(t, server)
	assert.True(t, strings.HasSuffix(text, denyLinkLabel+testDenyLink+"\n"), "a text without the link gets it at its end: %q", text)

	// With the links off, the text keeps its advice.
	off := deniableWithdrawal()
	off.DenyToken, off.DenyLink = "", ""
	require.NoError(t, newTestEmailDeliverer(t, server.emailConfig()).Deliver(ctx, off))
	_, text, html = lastEmail(t, server)
	assert.Contains(t, text, "Sign in, change your password, and contact support.")
	assert.NotContains(t, text, "Open this link")
	assert.NotContains(t, html, "This was not me")
}

// TestEmailDeliverer_SendsTheNoticeOfADenial renders the notice of a link
// that was used (#374), in the text and in the HTML of each language.
func TestEmailDeliverer_SendsTheNoticeOfADenial(t *testing.T) {
	ctx := context.Background()
	server := startFakeSMTPServer(t, "")
	notice := OTPMessage{ContactType: ContactEmail, Contact: testEmailMessage.Contact, MaskedContact: testEmailMessage.MaskedContact,
		Purpose: PurposeDenied, Details: "100 USDT to TR7NHq...gjLj6t", Amount: "100 USDT", Destination: "TR7NHq...gjLj6t",
		Cancelled: 2, InProgress: 1, HoldHours: 24, AntiPhishingCode: "blue falcon"}
	require.NoError(t, newTestEmailDeliverer(t, server.emailConfig()).Deliver(ctx, notice))
	subject, text, html := lastEmail(t, server)
	assert.Equal(t, "We stopped a withdrawal and secured your account", subject)
	assert.Contains(t, text, "the withdrawal of 100 USDT to TR7NHq...gjLj6t was used")
	assert.Contains(t, text, "We cancelled 2 withdrawal(s)")
	assert.Contains(t, text, "held for 24 hours")
	assert.Contains(t, text, "1 withdrawal(s) were already being paid")
	assert.Contains(t, text, "blue falcon")
	assert.NotContains(t, text, "http", "the notice has no link")
	assert.Contains(t, html, "could not be stopped")
	assert.Contains(t, html, "blue falcon")

	cfg := server.emailConfig()
	cfg.HTMLTemplates = HTMLTemplatesFA
	notice.InProgress = 0
	require.NoError(t, newTestEmailDeliverer(t, cfg).Deliver(ctx, notice))
	_, text, html = lastEmail(t, server)
	assert.NotContains(t, text, "already being paid", "nothing being paid")
	assert.Contains(t, html, "برداشت متوقف شد")
	assert.NotContains(t, html, "در حال پرداخت بود", "nothing being paid")
}

// TestKavenegarDeliverer_TheDenyLinkAndTheNotice: the token of a
// withdrawal's link is %token2 (#374), and a notice of a denial, which has
// no template there, never goes out with the sign-in one.
func TestKavenegarDeliverer_TheDenyLinkAndTheNotice(t *testing.T) {
	server, requests := fakeKavenegar(t, http.StatusOK, `{"return":{"status":200,"message":"OK"}}`)
	k, err := NewKavenegarDeliverer(config.KavenegarConfig{APIKey: testKavenegarKey, OTPTemplate: "signin",
		WithdrawalTemplate: "withdraw", BaseURL: server.URL, Timeout: 5})
	require.NoError(t, err)
	withdrawal := testSMSMessage
	withdrawal.Purpose, withdrawal.Details, withdrawal.DenyToken = PurposeWithdrawal, "100 USDT to TR7NHq...gjLj6t", "Ab3xK9q2LmPq"
	require.NoError(t, k.Deliver(context.Background(), withdrawal))
	got := requests()
	require.Len(t, got, 1)
	assert.Equal(t, "Ab3xK9q2LmPq", got[0].form.Get("token2"))

	assert.False(t, k.Supports(PurposeDenied))
	notice := testSMSMessage
	notice.Purpose, notice.Code = PurposeDenied, ""
	err = k.Deliver(context.Background(), notice)
	require.Error(t, err)
	assert.True(t, IsPermanent(err))
	assert.Len(t, requests(), 1, "nothing sent")
}

// TestSMSIRDeliverer_TheDenyLinkAndTheNotice: the token of a withdrawal's
// link is #DENY# (#374), only when the links are on, and a notice of a
// denial is never sent with the sign-in template.
func TestSMSIRDeliverer_TheDenyLinkAndTheNotice(t *testing.T) {
	server, requests := fakeSMSIR(t, http.StatusOK, smsirOK, nil)
	s := newTestSMSIR(t, server.URL+"/")
	ctx := context.Background()
	withdrawal := testSMSMessage
	withdrawal.Purpose, withdrawal.Amount, withdrawal.Destination = PurposeWithdrawal, "100 USDT", "TR7NHq...gjLj6t"
	withdrawal.DenyToken = "Ab3xK9q2LmPq"
	_, err := s.Send(ctx, withdrawal)
	require.NoError(t, err)
	got := requests()
	require.Len(t, got, 1)
	assert.Equal(t, []any{
		map[string]any{"name": "CODE", "value": "482913"},
		map[string]any{"name": "AMOUNT", "value": "100 USDT"},
		map[string]any{"name": "DEST", "value": "TR7NHq...gjLj6t"},
		map[string]any{"name": "DENY", "value": "Ab3xK9q2LmPq"},
	}, got[0].body["parameters"])

	assert.False(t, s.Supports(PurposeDenied))
	notice := testSMSMessage
	notice.Purpose, notice.Code = PurposeDenied, ""
	_, err = s.Send(ctx, notice)
	require.Error(t, err)
	assert.True(t, IsPermanent(err))
	assert.Len(t, requests(), 1, "nothing sent")
}

// TestRedact_RemovesTheTokenOfTheDenyLink: a provider's error that echoes
// the parameters of a withdrawal never logs the token of its link (#374).
func TestRedact_RemovesTheTokenOfTheDenyLink(t *testing.T) {
	msg := deniableWithdrawal()
	text := redact(errors.New("bad parameter DENY=Ab3xK9q2LmPq for "+testDenyLink), msg)
	assert.NotContains(t, text, "Ab3xK9q2LmPq")
	assert.Contains(t, text, "[link]")
}

// TestQueueSender_CarriesTheDenyLinkAndTheNotice: the link of a code and
// the counts of a notice (#374) reach the worker with the job.
func TestQueueSender_CarriesTheDenyLinkAndTheNotice(t *testing.T) {
	publisher := &recordingPublisher{}
	sender := NewQueueSender(publisher, "notifications", emailOnlyChannels(), 90*time.Second)
	notice := OTPMessage{ContactType: ContactEmail, Contact: "alice@example.com", Purpose: PurposeDenied,
		Cancelled: 2, InProgress: 1, HoldHours: 24}
	for _, msg := range []OTPMessage{deniableWithdrawal(), notice} {
		require.NoError(t, sender.SendOTP(context.Background(), msg))
	}
	published := publisher.messages()
	require.Len(t, published, 2)
	var code, denied otpJob
	require.NoError(t, json.Unmarshal(published[0].msg.Body, &code))
	require.NoError(t, json.Unmarshal(published[1].msg.Body, &denied))
	assert.Equal(t, "Ab3xK9q2LmPq", code.message().DenyToken)
	assert.Equal(t, testDenyLink, code.message().DenyLink)
	got := denied.message()
	assert.Equal(t, []int{2, 1, 24}, []int{got.Cancelled, got.InProgress, got.HoldHours})
}
