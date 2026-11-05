package notify

import (
	"context"
	"encoding/base64"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
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
	assert.Equal(t, `text/plain; charset="utf-8"`, msg.Header.Get("Content-Type"))
	assert.NotEmpty(t, msg.Header.Get("Message-ID"))
	assert.NotEmpty(t, msg.Header.Get("Date"))

	body, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
	require.NoError(t, err)
	assert.Contains(t, string(body), "Your sign-in code is 482913. It expires in 90 seconds.")

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
