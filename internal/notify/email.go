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
	// deniedSubject and deniedBody render the notice of a "this was not
	// me" link that was used (#374).
	deniedSubject *template.Template
	deniedBody    *template.Template
	// delistingSubject and delistingBody render the notices of a
	// delisting (#216).
	delistingSubject *template.Template
	delistingBody    *template.Template
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
	// DenyLink is the "this was not me" link of a withdrawal code (#374),
	// or "" when the links are off.
	DenyLink string
	// Cancelled, InProgress, and HoldHours are, in the notice of a denial,
	// the withdrawals that were cancelled, those that were being paid
	// already, and the length of the security hold.
	Cancelled, InProgress, HoldHours int
	// The fields of a notice of a delisting; see DelistingNotice.
	Currency, Stage, Deadline      string
	DaysLeft                       int
	Holder                         bool
	Settlement, SettlementCurrency string
	ClaimDays                      int
}

// antiPhishingLabel introduces the anti-phishing code that the text part of
// an email gains when its template does not show it.
const antiPhishingLabel = "Anti-phishing code / کد ضدفیشینگ: "

// denyLinkLabel introduces the "this was not me" link that the text part of
// an email gains when its template does not show it (#374).
const denyLinkLabel = "Not you? This link cancels it and secures your account / این من نبودم: "

// sampleDenyLink is the link of the sample that each template renders at
// startup: an HTML template of a code must show it.
const sampleDenyLink = "https://example.invalid/d/SAMPLE000000"

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
	d := &EmailDeliverer{cfg: cfg, from: from, codeTTL: codeTTL, now: time.Now, plaintextAllowed: isLocalHost}
	// In a fixed order, so that several invalid templates always name the
	// same one first.
	for _, tmpl := range []struct {
		name, text string
		into       **template.Template
	}{
		{"otp_subject", cfg.OTPSubject, &d.subject},
		{"otp_body", cfg.OTPBody, &d.body},
		{"withdrawal_subject", orDefault(cfg.WithdrawalSubject, config.DefaultWithdrawalEmailSubject), &d.withdrawalSubject},
		{"withdrawal_body", orDefault(cfg.WithdrawalBody, config.DefaultWithdrawalEmailBody), &d.withdrawalBody},
		{"anti_phishing_reminder_subject", orDefault(cfg.AntiPhishingReminderSubject, config.DefaultAntiPhishingReminderSubject), &d.reminderSubject},
		{"anti_phishing_reminder_body", orDefault(cfg.AntiPhishingReminderBody, config.DefaultAntiPhishingReminderBody), &d.reminderBody},
		{"denied_subject", orDefault(cfg.DeniedSubject, config.DefaultDeniedEmailSubject), &d.deniedSubject},
		{"denied_body", orDefault(cfg.DeniedBody, config.DefaultDeniedEmailBody), &d.deniedBody},
		{"delisting_notice_subject", orDefault(cfg.DelistingNoticeSubject, config.DefaultDelistingNoticeSubject), &d.delistingSubject},
		{"delisting_notice_body", orDefault(cfg.DelistingNoticeBody, config.DefaultDelistingNoticeBody), &d.delistingBody},
	} {
		t, err := template.New(tmpl.name).Parse(tmpl.text)
		if err != nil {
			return nil, fmt.Errorf("invalid %s: %w", tmpl.name, err)
		}
		*tmpl.into = t
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
	for _, purpose := range emailPurposes {
		sample := OTPMessage{Code: "000000", Purpose: purpose, Details: "1 USDT to T...0000", Amount: "1 USDT", Destination: "T...0000",
			AntiPhishingCode: "sample-phrase-7q", Uses: 200, AgeDays: 180}
		switch purpose {
		case PurposeWithdrawal:
			sample.DenyToken, sample.DenyLink = "SAMPLE000000", sampleDenyLink
		case PurposeAntiPhishingReminder:
			sample.Code, sample.Details, sample.Amount, sample.Destination = "", "", "", ""
		case PurposeDenied:
			sample.Code, sample.Cancelled, sample.InProgress, sample.HoldHours = "", 1, 1, 24
		case PurposeDelistingNotice:
			sample.Code, sample.Details, sample.Amount, sample.Destination = "", "", "", ""
		}
		_, _, html, err := d.render(sample)
		if err != nil {
			return nil, err
		}
		// Each stage of a delisting renders, as a holder and not.
		if purpose == PurposeDelistingNotice {
			for _, stage := range sampleDelistingStages {
				for _, holder := range []bool{true, false} {
					sample.Delisting = DelistingNotice{Currency: "ABC", Stage: stage, Deadline: "2026-12-01 12:00 UTC", DaysLeft: 7,
						Holder: holder, Settlement: "convert", SettlementCurrency: "USDT", ClaimDays: 180}
					if _, _, _, err := d.render(sample); err != nil {
						return nil, err
					}
				}
			}
		}
		// An HTML template must show the anti-phishing code itself (#375),
		// and the HTML of a code its "this was not me" link (#374): a place
		// in the HTML cannot be guessed, as one in the text can.
		if html != "" && !strings.Contains(html, sample.AntiPhishingCode) {
			return nil, fmt.Errorf("html_templates: %s must show {{.AntiPhishingCode}}", htmlTemplateName(purpose))
		}
		if html != "" && sample.DenyLink != "" && !strings.Contains(html, sample.DenyLink) {
			return nil, fmt.Errorf("html_templates: %s must show {{.DenyLink}}", htmlTemplateName(purpose))
		}
	}
	return d, nil
}

// emailPurposes are the purposes of the emails, in the order that their
// templates are checked at startup.
var emailPurposes = []string{"", PurposeWithdrawal, PurposeAntiPhishingReminder, PurposeDenied, PurposeDelistingNotice}

// sampleDelistingStages are the stages of a notice of a delisting.
var sampleDelistingStages = []string{"delist_announced", "deposits_disabled", "sell_only", "trading_halted",
	"withdrawal_only", "frozen", "delisted", DelistingStageReminder, DelistingStageCancelled}

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
		Uses: msg.Uses, AgeDays: msg.AgeDays, DenyLink: msg.DenyLink,
		Cancelled: msg.Cancelled, InProgress: msg.InProgress, HoldHours: msg.HoldHours,
		Currency: msg.Delisting.Currency, Stage: msg.Delisting.Stage, Deadline: msg.Delisting.Deadline,
		DaysLeft: msg.Delisting.DaysLeft, Holder: msg.Delisting.Holder, Settlement: msg.Delisting.Settlement,
		SettlementCurrency: msg.Delisting.SettlementCurrency, ClaimDays: msg.Delisting.ClaimDays}
	subjectT, bodyT, names := d.subject, d.body, [2]string{"otp_subject", "otp_body"}
	switch msg.Purpose {
	case PurposeWithdrawal:
		subjectT, bodyT, names = d.withdrawalSubject, d.withdrawalBody, [2]string{"withdrawal_subject", "withdrawal_body"}
	case PurposeAntiPhishingReminder:
		subjectT, bodyT = d.reminderSubject, d.reminderBody
		names = [2]string{"anti_phishing_reminder_subject", "anti_phishing_reminder_body"}
	case PurposeDenied:
		subjectT, bodyT, names = d.deniedSubject, d.deniedBody, [2]string{"denied_subject", "denied_body"}
	case PurposeDelistingNotice:
		subjectT, bodyT = d.delistingSubject, d.delistingBody
		names = [2]string{"delisting_notice_subject", "delisting_notice_body"}
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
	// So is the link of a code (#374): a text template that does not show
	// it gets it at the end.
	if link := msg.DenyLink; link != "" && !strings.Contains(text, link) {
		text = strings.TrimRight(text, "\n") + "\n\n" + denyLinkLabel + link + "\n"
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
