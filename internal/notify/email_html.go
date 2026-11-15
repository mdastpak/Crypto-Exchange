package notify

import (
	"bytes"
	"fmt"
	htmltemplate "html/template"
	"io"
	"io/fs"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"os"

	files "github.com/mdastpak/crypto-exchange/config"
)

// The values of notifications.email.html_templates that are not a
// directory.
const (
	HTMLTemplatesNone = "none"
	HTMLTemplatesEN   = "en"
	HTMLTemplatesFA   = "fa"
)

// htmlTemplates are the HTML parts of the emails: of a sign-in code, of
// a code that confirms a withdrawal (#161), of the reminder to change
// the anti-phishing code (#375), and of the notice of a "this was not me"
// link that was used (#374), and of the notices of a delisting (#216).
// Nil is text only.
type htmlTemplates struct {
	otp, withdrawal, reminder, denied, delisting *htmltemplate.Template
}

// loadHTMLTemplates loads the HTML templates that setting names: the
// embedded ones of a language, a directory, or none. html/template escapes
// what the templates print, so a detail of a withdrawal can never add
// markup.
func loadHTMLTemplates(setting string) (*htmlTemplates, error) {
	var dir fs.FS
	switch setting {
	case HTMLTemplatesNone:
		return nil, nil
	case "", HTMLTemplatesEN, HTMLTemplatesFA:
		if setting == "" {
			setting = HTMLTemplatesEN
		}
		sub, err := fs.Sub(files.EmailTemplates, "email/"+setting)
		if err != nil {
			return nil, err
		}
		dir = sub
	default:
		info, err := os.Stat(setting)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("html_templates %q is not en, fa, none, or a directory", setting)
		}
		dir = os.DirFS(setting)
	}
	t := &htmlTemplates{}
	// In a fixed order, so that a directory that lacks several templates
	// always names the same one first.
	for _, tmpl := range []struct {
		purpose string
		into    **htmltemplate.Template
	}{
		{"", &t.otp}, {PurposeWithdrawal, &t.withdrawal},
		{PurposeAntiPhishingReminder, &t.reminder}, {PurposeDenied, &t.denied},
		{PurposeDelistingNotice, &t.delisting},
	} {
		name, into := htmlTemplateName(tmpl.purpose), tmpl.into
		text, err := fs.ReadFile(dir, name)
		if err != nil {
			return nil, fmt.Errorf("html_templates: %w", err)
		}
		parsed, err := htmltemplate.New(name).Option("missingkey=error").Parse(string(text))
		if err != nil {
			return nil, fmt.Errorf("html_templates: invalid %s: %w", name, err)
		}
		*into = parsed
	}
	return t, nil
}

// htmlTemplateName is the file of the HTML template of a purpose.
func htmlTemplateName(purpose string) string {
	switch purpose {
	case PurposeWithdrawal:
		return "withdrawal.html"
	case PurposeAntiPhishingReminder:
		return "anti_phishing_reminder.html"
	case PurposeDenied:
		return "denied.html"
	case PurposeDelistingNotice:
		return "delisting_notice.html"
	}
	return "otp.html"
}

// render renders the HTML part of msg, or "" without HTML templates.
func (t *htmlTemplates) render(msg OTPMessage, data emailData) (string, error) {
	if t == nil {
		return "", nil
	}
	tmpl := t.otp
	switch msg.Purpose {
	case PurposeWithdrawal:
		tmpl = t.withdrawal
	case PurposeAntiPhishingReminder:
		tmpl = t.reminder
	case PurposeDenied:
		tmpl = t.denied
	case PurposeDelistingNotice:
		tmpl = t.delisting
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render %s: %w", tmpl.Name(), err)
	}
	return buf.String(), nil
}

// writeBody writes the body of the message: the text alone, or the text and
// the HTML as alternatives, each quoted-printable. It returns the
// Content-Type of the message.
func writeBody(w io.Writer, text, html string) (string, error) {
	if html == "" {
		if err := writeQuotedPrintable(w, text); err != nil {
			return "", err
		}
		return `text/plain; charset="utf-8"`, nil
	}
	parts := multipart.NewWriter(w)
	for _, part := range []struct{ contentType, body string }{
		{`text/plain; charset="utf-8"`, text},
		{`text/html; charset="utf-8"`, html},
	} {
		header := textproto.MIMEHeader{}
		header.Set("Content-Type", part.contentType)
		header.Set("Content-Transfer-Encoding", "quoted-printable")
		pw, err := parts.CreatePart(header)
		if err != nil {
			return "", err
		}
		if err := writeQuotedPrintable(pw, part.body); err != nil {
			return "", err
		}
	}
	if err := parts.Close(); err != nil {
		return "", err
	}
	return `multipart/alternative; boundary="` + parts.Boundary() + `"`, nil
}

func writeQuotedPrintable(w io.Writer, s string) error {
	qp := quotedprintable.NewWriter(w)
	if _, err := qp.Write([]byte(s)); err != nil {
		return err
	}
	return qp.Close()
}
