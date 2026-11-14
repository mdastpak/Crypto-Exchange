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
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
)

// EmailDeliverer sends sign-in codes by SMTP.
type EmailDeliverer struct {
	cfg     config.EmailConfig
	from    *mail.Address
	subject *template.Template
	body    *template.Template
	// withdrawalSubject and withdrawalBody render the codes that confirm a
	// withdrawal (#161).
	withdrawalSubject *template.Template
	withdrawalBody    *template.Template
	// reminderSubject and reminderBody render the reminder to change the
	// anti-phishing code (#375).
	reminderSubject *template.Template
	reminderBody    *template.Template
	// html renders the HTML part beside the text; nil is text only.
	html    *htmlTemplates
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
	// Details is what a withdrawal code confirms; empty for a sign-in.
	// Amount and Destination are its two parts.
	Details, Amount, Destination string
	// Sender is the name of the sender address, such as the exchange's.
	Sender string
	// AntiPhishingCode is the user's anti-phishing phrase (#375), or "".
	AntiPhishingCode string
	// Uses and AgeDays are, in a reminder to change the anti-phishing
	// code, the emails that have shown it and its age in days.
	Uses    int64
	AgeDays int
}

// antiPhishingLabel introduces the anti-phishing code that the text part of
// an email gains when its template does not show it.
const antiPhishingLabel = "Anti-phishing code / کد ضدفیشینگ: "

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
	templates := map[string]**template.Template{}
	d := &EmailDeliverer{cfg: cfg, from: from, codeTTL: codeTTL, now: time.Now, plaintextAllowed: isLocalHost}
	templates["otp_subject"], templates["otp_body"] = &d.subject, &d.body
	templates["withdrawal_subject"], templates["withdrawal_body"] = &d.withdrawalSubject, &d.withdrawalBody
	templates["anti_phishing_reminder_subject"], templates["anti_phishing_reminder_body"] = &d.reminderSubject, &d.reminderBody
	for name, text := range map[string]string{
		"otp_subject": cfg.OTPSubject, "otp_body": cfg.OTPBody,
		"withdrawal_subject":             orDefault(cfg.WithdrawalSubject, config.DefaultWithdrawalEmailSubject),
		"withdrawal_body":                orDefault(cfg.WithdrawalBody, config.DefaultWithdrawalEmailBody),
		"anti_phishing_reminder_subject": orDefault(cfg.AntiPhishingReminderSubject, config.DefaultAntiPhishingReminderSubject),
		"anti_phishing_reminder_body":    orDefault(cfg.AntiPhishingReminderBody, config.DefaultAntiPhishingReminderBody),
	} {
		t, err := template.New(name).Parse(text)
		if err != nil {
			return nil, fmt.Errorf("invalid %s: %w", name, err)
		}
		*templates[name] = t
	}

	d.timeout = time.Duration(cfg.Timeout) * time.Second
	if d.timeout <= 0 {
		d.timeout = 10 * time.Second
	}
	if d.html, err = loadHTMLTemplates(cfg.HTMLTemplates); err != nil {
		return nil, err
	}
	// Render once, so that a template that fails, for example by using an
	// unknown field, stops the server at startup instead of every delivery.
	for _, purpose := range []string{"", PurposeWithdrawal, PurposeAntiPhishingReminder} {
		sample := OTPMessage{Code: "000000", Purpose: purpose, Details: "1 USDT to T...0000", Amount: "1 USDT", Destination: "T...0000",
			AntiPhishingCode: "sample-phrase-7q", Uses: 200, AgeDays: 180}
		if purpose == PurposeAntiPhishingReminder {
			sample.Code, sample.Details, sample.Amount, sample.Destination = "", "", "", ""
		}
		_, _, html, err := d.render(sample)
		if err != nil {
			return nil, err
		}
		// An HTML template must show the anti-phishing code itself (#375):
		// a place in the HTML cannot be guessed, as the text's first line
		// can.
		if html != "" && !strings.Contains(html, sample.AntiPhishingCode) {
			return nil, fmt.Errorf("html_templates: %s must show {{.AntiPhishingCode}}", htmlTemplateName(purpose))
		}
	}
	return d, nil
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// render returns the subject, the text, and the HTML of msg; the HTML is
// empty without HTML templates.
func (d *EmailDeliverer) render(msg OTPMessage) (subject, text, html string, err error) {
	sender := d.from.Name
	if sender == "" {
		sender = d.from.Address
	}
	data := emailData{Code: msg.Code, TTLSeconds: int(d.codeTTL / time.Second), Details: msg.Details,
		Amount: msg.Amount, Destination: msg.Destination, Sender: sender, AntiPhishingCode: msg.AntiPhishingCode,
		Uses: msg.Uses, AgeDays: msg.AgeDays}
	subjectT, bodyT, names := d.subject, d.body, [2]string{"otp_subject", "otp_body"}
	switch msg.Purpose {
	case PurposeWithdrawal:
		subjectT, bodyT, names = d.withdrawalSubject, d.withdrawalBody, [2]string{"withdrawal_subject", "withdrawal_body"}
	case PurposeAntiPhishingReminder:
		subjectT, bodyT = d.reminderSubject, d.reminderBody
		names = [2]string{"anti_phishing_reminder_subject", "anti_phishing_reminder_body"}
	}
	var buf bytes.Buffer
	if err := subjectT.Execute(&buf, data); err != nil {
		return "", "", "", fmt.Errorf("render %s: %w", names[0], err)
	}
	subject = strings.TrimSpace(buf.String())
	if strings.ContainsAny(subject, "\r\n") {
		return "", "", "", fmt.Errorf("%s must render to a single line", names[0])
	}
	buf.Reset()
	if err := bodyT.Execute(&buf, data); err != nil {
		return "", "", "", fmt.Errorf("render %s: %w", names[1], err)
	}
	text = buf.String()
	// The anti-phishing code is in every email (#375): a text template of
	// the operator's that does not show it gets it on its first line.
	if code := msg.AntiPhishingCode; code != "" && !strings.Contains(text, code) {
		text = antiPhishingLabel + code + "\n\n" + text
	}
	if html, err = d.html.render(msg, data); err != nil {
		return "", "", "", err
	}
	return subject, text, html, nil
}

// Deliver sends the code to the email address in msg.Contact.
func (d *EmailDeliverer) Deliver(ctx context.Context, msg OTPMessage) error {
	to, err := mail.ParseAddress(msg.Contact)
	if err != nil {
		// The parse error repeats the address, which must not reach the log.
		return permanent(errors.New("invalid recipient address"))
	}
	subject, body, html, err := d.render(msg)
	if err != nil {
		return permanent(err)
	}
	message, err := d.buildMessage(to, subject, body, html)
	if err != nil {
		return permanent(err)
	}
	return d.send(ctx, to.Address, message)
}

// buildMessage returns a plain-text UTF-8 message. The subject is encoded as
// an RFC 2047 encoded word and the body as quoted-printable, so templates can
// use any language.
// buildMessage builds the message: the text alone, or the text and the HTML
// as alternatives, so that a client without HTML shows the text.
func (d *EmailDeliverer) buildMessage(to *mail.Address, subject, text, html string) ([]byte, error) {
	messageID, err := newMessageID(d.from.Address)
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	contentType, err := writeBody(&body, text, html)
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
	header("Content-Type", contentType)
	if html == "" {
		header("Content-Transfer-Encoding", "quoted-printable")
	}
	buf.WriteString("\r\n")
	buf.Write(body.Bytes())
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
