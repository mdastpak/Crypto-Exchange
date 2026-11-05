package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/mdastpak/crypto-exchange/config"
)

// EmailDeliverer sends sign-in codes by SMTP.
type EmailDeliverer struct {
	cfg     config.EmailConfig
	from    *mail.Address
	subject *template.Template
	body    *template.Template
	codeTTL time.Duration
	timeout time.Duration
	now     func() time.Time
	// plaintextAllowed reports whether host may be reached without TLS.
	plaintextAllowed func(host string) bool
}

// emailData is the data available to the subject and body templates.
type emailData struct {
	Code       string
	TTLSeconds int
}

// NewEmailDeliverer validates cfg and returns a deliverer for it. codeTTL is
// the lifetime of sign-in codes, shown to templates as {{.TTLSeconds}}.
func NewEmailDeliverer(cfg config.EmailConfig, codeTTL time.Duration) (*EmailDeliverer, error) {
	if cfg.Host == "" {
		return nil, errors.New("host is required")
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("invalid port %d", cfg.Port)
	}
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return nil, fmt.Errorf("invalid from address: %w", err)
	}
	subject, err := template.New("otp_subject").Parse(cfg.OTPSubject)
	if err != nil {
		return nil, fmt.Errorf("invalid otp_subject: %w", err)
	}
	body, err := template.New("otp_body").Parse(cfg.OTPBody)
	if err != nil {
		return nil, fmt.Errorf("invalid otp_body: %w", err)
	}

	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	d := &EmailDeliverer{
		cfg:     cfg,
		from:    from,
		subject: subject,
		body:    body,
		codeTTL: codeTTL,
		timeout: timeout,
		now:     time.Now,

		plaintextAllowed: isLocalHost,
	}
	// Render once, so that a template that fails, for example by using an
	// unknown field, stops the server at startup instead of every delivery.
	if _, _, err := d.render("000000"); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *EmailDeliverer) render(code string) (subject, body string, err error) {
	data := emailData{Code: code, TTLSeconds: int(d.codeTTL / time.Second)}
	var buf bytes.Buffer
	if err := d.subject.Execute(&buf, data); err != nil {
		return "", "", fmt.Errorf("render otp_subject: %w", err)
	}
	subject = strings.TrimSpace(buf.String())
	if strings.ContainsAny(subject, "\r\n") {
		return "", "", errors.New("otp_subject must render to a single line")
	}
	buf.Reset()
	if err := d.body.Execute(&buf, data); err != nil {
		return "", "", fmt.Errorf("render otp_body: %w", err)
	}
	return subject, buf.String(), nil
}

// Deliver sends the code to the email address in msg.Contact.
func (d *EmailDeliverer) Deliver(ctx context.Context, msg OTPMessage) error {
	to, err := mail.ParseAddress(msg.Contact)
	if err != nil {
		// The parse error repeats the address, which must not reach the log.
		return permanent(errors.New("invalid recipient address"))
	}
	subject, body, err := d.render(msg.Code)
	if err != nil {
		return permanent(err)
	}
	message, err := d.buildMessage(to, subject, body)
	if err != nil {
		return permanent(err)
	}
	return d.send(ctx, to.Address, message)
}

// buildMessage returns a plain-text UTF-8 message. The subject is encoded as
// an RFC 2047 encoded word and the body as quoted-printable, so templates can
// use any language.
func (d *EmailDeliverer) buildMessage(to *mail.Address, subject, body string) ([]byte, error) {
	messageID, err := newMessageID(d.from.Address)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	header := func(name, value string) {
		buf.WriteString(name + ": " + value + "\r\n")
	}
	header("From", d.from.String())
	header("To", to.String())
	header("Subject", mime.QEncoding.Encode("utf-8", subject))
	header("Date", d.now().Format(time.RFC1123Z))
	header("Message-ID", messageID)
	header("MIME-Version", "1.0")
	header("Content-Type", `text/plain; charset="utf-8"`)
	header("Content-Transfer-Encoding", "quoted-printable")
	buf.WriteString("\r\n")

	qp := quotedprintable.NewWriter(&buf)
	if _, err := qp.Write([]byte(body)); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func newMessageID(from string) (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	domain := from[strings.LastIndex(from, "@")+1:]
	return "<" + hex.EncodeToString(random) + "@" + domain + ">", nil
}

// isLocalHost reports whether host is the local machine, the only place a
// plaintext SMTP connection is acceptable.
func isLocalHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// send transmits message in one SMTP session. Codes and credentials are only
// sent over TLS, unless the server is on the local machine.
func (d *EmailDeliverer) send(ctx context.Context, to string, message []byte) error {
	deadline := d.now().Add(d.timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	addr := net.JoinHostPort(d.cfg.Host, strconv.Itoa(d.cfg.Port))
	tlsConfig := &tls.Config{ServerName: d.cfg.Host, MinVersion: tls.VersionTLS12}
	dialer := &net.Dialer{Deadline: deadline}

	var conn net.Conn
	var err error
	if d.cfg.ImplicitTLS {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connect to the SMTP server: %w", err)
	}
	// The deadline bounds the whole session, and cancelling ctx ends it early.
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	client, err := smtp.NewClient(conn, d.cfg.Host)
	if err != nil {
		conn.Close()
		return smtpError("greeting", err)
	}
	defer client.Close()

	if !d.cfg.ImplicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(tlsConfig); err != nil {
				return smtpError("STARTTLS", err)
			}
		} else if !d.plaintextAllowed(d.cfg.Host) {
			return permanent(errors.New("the SMTP server does not support STARTTLS; refusing to send the code in plaintext"))
		}
	}

	if d.cfg.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", d.cfg.Username, d.cfg.Password, d.cfg.Host)); err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) {
				return fmt.Errorf("SMTP authentication: %w", err)
			}
			// Rejected credentials and missing TLS are configuration errors.
			return permanent(fmt.Errorf("SMTP authentication: %w", err))
		}
	}
	if err := client.Mail(d.from.Address); err != nil {
		return smtpError("MAIL FROM", err)
	}
	if err := client.Rcpt(to); err != nil {
		return smtpError("RCPT TO", err)
	}
	w, err := client.Data()
	if err != nil {
		return smtpError("DATA", err)
	}
	if _, err := w.Write(message); err != nil {
		return smtpError("DATA", err)
	}
	if err := w.Close(); err != nil {
		return smtpError("DATA", err)
	}
	// The server accepted the message; a failed QUIT does not change that.
	_ = client.Quit()
	return nil
}

// smtpError marks permanent SMTP replies (5xx), such as an unknown recipient,
// so that they are not retried.
func smtpError(step string, err error) error {
	err = fmt.Errorf("SMTP %s: %w", step, err)
	var reply *textproto.Error
	if errors.As(err, &reply) && reply.Code >= 500 {
		return permanent(err)
	}
	return err
}
