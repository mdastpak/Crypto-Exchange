package notify

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSMTPServer is a minimal SMTP server on the loopback interface that
// records the messages it receives. It offers AUTH PLAIN but not STARTTLS.
type fakeSMTPServer struct {
	host string
	port int
	// rcptReply, when set, is the reply to every RCPT TO command.
	rcptReply string

	mu       sync.Mutex
	auth     []string
	messages []receivedMail
}

type receivedMail struct {
	from, to, data string
}

func startFakeSMTPServer(t *testing.T, rcptReply string) *fakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	addr := ln.Addr().(*net.TCPAddr)
	s := &fakeSMTPServer{host: addr.IP.String(), port: addr.Port, rcptReply: rcptReply}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *fakeSMTPServer) serve(conn net.Conn) {
	defer conn.Close()
	tp := textproto.NewConn(conn)
	_ = tp.PrintfLine("220 localhost fake SMTP server")
	var current receivedMail
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		command := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(command, "EHLO"):
			_ = tp.PrintfLine("250-localhost")
			_ = tp.PrintfLine("250 AUTH PLAIN")
		case strings.HasPrefix(command, "AUTH"):
			s.mu.Lock()
			s.auth = append(s.auth, line)
			s.mu.Unlock()
			_ = tp.PrintfLine("235 2.7.0 Authentication successful")
		case strings.HasPrefix(command, "MAIL FROM:"):
			current = receivedMail{from: angleAddress(line)}
			_ = tp.PrintfLine("250 2.1.0 OK")
		case strings.HasPrefix(command, "RCPT TO:"):
			if s.rcptReply != "" {
				_ = tp.PrintfLine("%s", s.rcptReply)
				continue
			}
			current.to = angleAddress(line)
			_ = tp.PrintfLine("250 2.1.5 OK")
		case command == "DATA":
			_ = tp.PrintfLine("354 End data with <CR><LF>.<CR><LF>")
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			current.data = string(data)
			s.mu.Lock()
			s.messages = append(s.messages, current)
			s.mu.Unlock()
			_ = tp.PrintfLine("250 2.0.0 OK")
		case command == "QUIT":
			_ = tp.PrintfLine("221 2.0.0 Bye")
			return
		default:
			_ = tp.PrintfLine("250 OK")
		}
	}
}

func angleAddress(line string) string {
	start, end := strings.Index(line, "<"), strings.LastIndex(line, ">")
	if start < 0 || end < start {
		return ""
	}
	return line[start+1 : end]
}

func (s *fakeSMTPServer) received() []receivedMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]receivedMail(nil), s.messages...)
}

func (s *fakeSMTPServer) emailConfig() config.EmailConfig {
	return config.EmailConfig{
		Host:       s.host,
		Port:       s.port,
		From:       "Exchange <no-reply@example.com>",
		Timeout:    5,
		OTPSubject: config.DefaultOTPEmailSubject,
		OTPBody:    config.DefaultOTPEmailBody,
	}
}

func newTestEmailDeliverer(t *testing.T, cfg config.EmailConfig) *EmailDeliverer {
	t.Helper()
	d, err := NewEmailDeliverer(cfg, 90*time.Second)
	require.NoError(t, err)
	return d
}

var testEmailMessage = OTPMessage{
	ContactType:   ContactEmail,
	Contact:       "alice@example.com",
	MaskedContact: "ali*****@example.com",
	Code:          "482913",
}

func TestEmailDeliverer_SendsTheCode(t *testing.T) {
	server := startFakeSMTPServer(t, "")
	cfg := server.emailConfig()
	cfg.Username = "mailer"
	cfg.Password = "secret"
	cfg.OTPSubject = "Sign-in code — {{.Code}} ✓"
	d := newTestEmailDeliverer(t, cfg)

	require.NoError(t, d.Deliver(context.Background(), testEmailMessage))

	received := server.received()
	require.Len(t, received, 1)
	assert.Equal(t, "no-reply@example.com", received[0].from)
	assert.Equal(t, "alice@example.com", received[0].to)

	msg, err := mail.ReadMessage(strings.NewReader(received[0].data))
	require.NoError(t, err)
	subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	require.NoError(t, err)
	assert.Equal(t, "Sign-in code — 482913 ✓", subject, "non-ASCII subjects are encoded")
	assert.NotEmpty(t, msg.Header.Get("Message-ID"))
	assert.NotEmpty(t, msg.Header.Get("Date"))

	text, html := emailParts(t, msg)
	assert.Contains(t, text, "Your sign-in code is 482913. It expires in 90 seconds.")
	assert.Contains(t, html, "482913", "the HTML part carries the code too")
	assert.Contains(t, html, `lang="en"`)

	server.mu.Lock()
	defer server.mu.Unlock()
	require.Len(t, server.auth, 1)
	credentials := base64.StdEncoding.EncodeToString([]byte("\x00mailer\x00secret"))
	assert.Equal(t, "AUTH PLAIN "+credentials, server.auth[0])
}

func TestEmailDeliverer_RecipientRejection(t *testing.T) {
	cases := map[string]bool{
		"550 5.1.1 <alice@example.com>: Recipient address rejected": true,
		"451 4.7.1 Greylisted, try again later":                     false,
	}
	for reply, wantPermanent := range cases {
		t.Run(reply, func(t *testing.T) {
			server := startFakeSMTPServer(t, reply)
			d := newTestEmailDeliverer(t, server.emailConfig())

			err := d.Deliver(context.Background(), testEmailMessage)

			require.Error(t, err)
			assert.Equal(t, wantPermanent, IsPermanent(err), "5xx replies are permanent, 4xx are retried")
			assert.Empty(t, server.received())
		})
	}
}

func TestEmailDeliverer_RefusesPlaintextToARemoteServer(t *testing.T) {
	server := startFakeSMTPServer(t, "")
	d := newTestEmailDeliverer(t, server.emailConfig())
	d.plaintextAllowed = func(string) bool { return false }

	err := d.Deliver(context.Background(), testEmailMessage)

	require.Error(t, err)
	assert.True(t, IsPermanent(err))
	assert.Contains(t, err.Error(), "STARTTLS")
	assert.Empty(t, server.received(), "the code must not be sent without TLS")
}

func TestEmailDeliverer_RejectsHeaderInjection(t *testing.T) {
	server := startFakeSMTPServer(t, "")
	d := newTestEmailDeliverer(t, server.emailConfig())
	msg := testEmailMessage
	msg.Contact = "alice@example.com\r\nBcc: mallory@example.com"

	err := d.Deliver(context.Background(), msg)

	require.Error(t, err)
	assert.True(t, IsPermanent(err))
	assert.NotContains(t, err.Error(), "mallory", "the error must not repeat the address")
	assert.Empty(t, server.received())
}

func TestEmailDeliverer_UnreachableServerIsRetried(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	cfg := (&fakeSMTPServer{host: "127.0.0.1", port: port}).emailConfig()
	d := newTestEmailDeliverer(t, cfg)

	err = d.Deliver(context.Background(), testEmailMessage)

	require.Error(t, err)
	assert.False(t, IsPermanent(err))
}

func TestEmailDeliverer_StopsWhenTheContextEnds(t *testing.T) {
	// The server accepts the connection but never greets the client.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()

	cfg := (&fakeSMTPServer{host: "127.0.0.1", port: ln.Addr().(*net.TCPAddr).Port}).emailConfig()
	cfg.Timeout = 30
	d := newTestEmailDeliverer(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = d.Deliver(ctx, testEmailMessage)

	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "the context deadline must bound the SMTP session")
}

func TestIsLocalHost(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "127.0.0.53", "::1"} {
		assert.True(t, isLocalHost(host), host)
	}
	for _, host := range []string{"smtp.example.com", "10.0.0.5", "localhost.example.com", ""} {
		assert.False(t, isLocalHost(host), host)
	}
}

// TestEmailDeliverer_WithdrawalCodesHaveTheirOwnText checks that a code
// that confirms a withdrawal is sent with the withdrawal text and what it
// confirms (#161), not with the sign-in text.
func TestEmailDeliverer_WithdrawalCodesHaveTheirOwnText(t *testing.T) {
	server := startFakeSMTPServer(t, "")
	d := newTestEmailDeliverer(t, server.emailConfig())
	withdrawal := testEmailMessage
	withdrawal.Purpose, withdrawal.Details = PurposeWithdrawal, "100 USDT to TR7NHq...gjLj6t"
	withdrawal.Amount, withdrawal.Destination = "100 USDT", "TR7NHq...gjLj6t"
	require.NoError(t, d.Deliver(context.Background(), withdrawal))

	received := server.received()
	require.Len(t, received, 1)
	msg, err := mail.ReadMessage(strings.NewReader(received[0].data))
	require.NoError(t, err)
	assert.Equal(t, "Confirm your withdrawal", msg.Header.Get("Subject"))
	text, html := emailParts(t, msg)
	assert.Contains(t, text, "Your code to confirm the withdrawal of 100 USDT to TR7NHq...gjLj6t is 482913.")
	assert.NotContains(t, text, "sign-in")
	assert.Contains(t, html, "Confirm your withdrawal")
	assert.Contains(t, html, "100 USDT")
	assert.Contains(t, html, "TR7NHq...gjLj6t")
	assert.NotContains(t, html, "sign-in code")

	cfg := server.emailConfig()
	cfg.WithdrawalBody = "{{.Unknown}}"
	_, err = NewEmailDeliverer(cfg, time.Minute)
	assert.Error(t, err, "a withdrawal template that fails stops the server at startup")
}

// emailParts returns the text and the HTML of a message: its body when it
// is text only, or its two alternatives.
func emailParts(t *testing.T, msg *mail.Message) (text, html string) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	require.NoError(t, err)
	if mediaType == "text/plain" {
		body, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
		require.NoError(t, err)
		return string(body), ""
	}
	require.Equal(t, "multipart/alternative", mediaType)
	reader := multipart.NewReader(msg.Body, params["boundary"])
	for {
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		assert.Equal(t, "quoted-printable", part.Header.Get("Content-Transfer-Encoding"))
		body, err := io.ReadAll(quotedprintable.NewReader(part))
		require.NoError(t, err)
		switch part.Header.Get("Content-Type") {
		case `text/plain; charset="utf-8"`:
			text = string(body)
		case `text/html; charset="utf-8"`:
			html = string(body)
		}
	}
	require.NotEmpty(t, text, "a client without HTML has the text")
	return text, html
}

// TestEmailDeliverer_HTMLTemplates checks the HTML templates of each
// language, that they escape what they print, and that "none" sends text
// only.
func TestEmailDeliverer_HTMLTemplates(t *testing.T) {
	server := startFakeSMTPServer(t, "")
	send := func(setting string, msg OTPMessage) (string, string) {
		t.Helper()
		cfg := server.emailConfig()
		cfg.HTMLTemplates = setting
		d := newTestEmailDeliverer(t, cfg)
		require.NoError(t, d.Deliver(context.Background(), msg))
		received := server.received()
		parsed, err := mail.ReadMessage(strings.NewReader(received[len(received)-1].data))
		require.NoError(t, err)
		return emailParts(t, parsed)
	}
	withdrawal := testEmailMessage
	withdrawal.Purpose, withdrawal.Amount, withdrawal.Destination = PurposeWithdrawal, "100 USDT", "<b>TR7</b>"
	withdrawal.Details = withdrawal.Amount + " to " + withdrawal.Destination

	_, html := send(HTMLTemplatesFA, withdrawal)
	assert.Contains(t, html, `dir="rtl"`)
	assert.Contains(t, html, "تأیید برداشت")
	assert.Contains(t, html, "&lt;b&gt;TR7&lt;/b&gt;", "what the template prints is escaped")
	assert.NotContains(t, html, "<b>TR7</b>")
	_, html = send(HTMLTemplatesFA, testEmailMessage)
	assert.Contains(t, html, "کد ورود شما")

	text, html := send(HTMLTemplatesNone, testEmailMessage)
	assert.Empty(t, html, "none sends text only")
	assert.Contains(t, text, "482913")

	// A directory of the operator's own templates.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "otp.html"), []byte(`<p>{{.Sender}}: {{.Code}} {{.AntiPhishingCode}}</p>`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "withdrawal.html"), []byte(`<p>{{.Amount}} {{.Code}} {{.AntiPhishingCode}}</p>`), 0o600))
	// Without the reminder's template, the directory is refused (#375).
	cfg := server.emailConfig()
	cfg.HTMLTemplates = dir
	_, err := NewEmailDeliverer(cfg, time.Minute)
	require.ErrorContains(t, err, "anti_phishing_reminder.html")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "anti_phishing_reminder.html"), []byte(`<p>{{.Uses}} {{.AntiPhishingCode}}</p>`), 0o600))
	_, html = send(dir, testEmailMessage)
	assert.Contains(t, html, "<p>Exchange: 482913 </p>", "the sender is the name of the from address")

	cfg.HTMLTemplates = filepath.Join(dir, "missing")
	_, err = NewEmailDeliverer(cfg, time.Minute)
	require.Error(t, err)
	// An HTML template that does not show the anti-phishing code (#375).
	require.NoError(t, os.WriteFile(filepath.Join(dir, "withdrawal.html"), []byte(`<p>{{.Amount}} {{.Code}}</p>`), 0o600))
	cfg.HTMLTemplates = dir
	_, err = NewEmailDeliverer(cfg, time.Minute)
	require.ErrorContains(t, err, "withdrawal.html must show {{.AntiPhishingCode}}")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "otp.html"), []byte(`<p>{{.Nope}}</p>`), 0o600))
	cfg.HTMLTemplates = dir
	_, err = NewEmailDeliverer(cfg, time.Minute)
	require.Error(t, err, "a template that fails stops the server at startup")
}

// TestEmailDeliverer_ShowsTheAntiPhishingCode checks that every email to a
// user who has an anti-phishing code shows it (#375): in the HTML of each
// language, escaped, and in the text, on the first line when the text
// template does not show it.
func TestEmailDeliverer_ShowsTheAntiPhishingCode(t *testing.T) {
	server := startFakeSMTPServer(t, "")
	send := func(setting string, msg OTPMessage) (string, string) {
		t.Helper()
		cfg := server.emailConfig()
		cfg.HTMLTemplates = setting
		d := newTestEmailDeliverer(t, cfg)
		require.NoError(t, d.Deliver(context.Background(), msg))
		received := server.received()
		parsed, err := mail.ReadMessage(strings.NewReader(received[len(received)-1].data))
		require.NoError(t, err)
		return emailParts(t, parsed)
	}
	signIn := testEmailMessage
	signIn.AntiPhishingCode = "blue-falcon"
	withdrawal := signIn
	withdrawal.Purpose, withdrawal.Amount, withdrawal.Destination = PurposeWithdrawal, "1 USDT", "T...0000"
	withdrawal.Details = "1 USDT to T...0000"

	for _, lang := range []string{HTMLTemplatesEN, HTMLTemplatesFA} {
		for _, msg := range []OTPMessage{signIn, withdrawal} {
			text, html := send(lang, msg)
			assert.True(t, strings.HasPrefix(text, "Anti-phishing code / کد ضدفیشینگ: blue-falcon\n"), "%s %s: %q", lang, msg.Purpose, text)
			assert.Contains(t, html, "<b>blue-falcon</b>", "%s %s", lang, msg.Purpose)
		}
	}

	// Escaped in the HTML, though the phrase never holds markup.
	tricky := signIn
	tricky.AntiPhishingCode = "<i>x</i>"
	_, html := send(HTMLTemplatesEN, tricky)
	assert.Contains(t, html, "&lt;i&gt;x&lt;/i&gt;")

	// Without a code, nothing is added.
	text, html := send(HTMLTemplatesEN, testEmailMessage)
	assert.NotContains(t, text, "Anti-phishing")
	assert.NotContains(t, html, "anti-phishing")
}

// TestEmailDeliverer_SendsTheAntiPhishingReminder checks the reminder to
// change the anti-phishing code (#375): its own subject and text, with the
// code, its uses, and its age, in the HTML of each language, and no link.
func TestEmailDeliverer_SendsTheAntiPhishingReminder(t *testing.T) {
	server := startFakeSMTPServer(t, "")
	reminder := OTPMessage{ContactType: ContactEmail, Contact: "user@example.com", MaskedContact: "use*****@example.com",
		Purpose: PurposeAntiPhishingReminder, AntiPhishingCode: "blue-falcon", Uses: 212, AgeDays: 190}
	for lang, title := range map[string]string{HTMLTemplatesEN: "Time to change your anti-phishing code", HTMLTemplatesFA: "زمان تغییر کد ضدفیشینگ"} {
		cfg := server.emailConfig()
		cfg.HTMLTemplates = lang
		d := newTestEmailDeliverer(t, cfg)
		require.NoError(t, d.Deliver(context.Background(), reminder))
		received := server.received()
		parsed, err := mail.ReadMessage(strings.NewReader(received[len(received)-1].data))
		require.NoError(t, err)
		subject, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
		require.NoError(t, err)
		assert.Equal(t, "Time to change your anti-phishing code", subject)
		text, html := emailParts(t, parsed)
		assert.Contains(t, text, "Your anti-phishing code blue-falcon was set 190 days ago, and 212 emails from us have shown it.")
		assert.Contains(t, html, title)
		assert.Contains(t, html, "<b>blue-falcon</b>")
		assert.Contains(t, html, "212")
		assert.Contains(t, html, "190")
		assert.NotContains(t, html, "href", "%s: a reminder has no link", lang)
	}
}
