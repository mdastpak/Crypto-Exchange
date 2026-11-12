package logging

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
)

// sensitiveKeys are the attribute keys whose values are never written,
// whatever they hold.
var sensitiveKeys = map[string]bool{
	"password": true, "passwd": true, "secret": true, "token": true, "access_token": true,
	"refresh_token": true, "authorization": true, "cookie": true, "api_key": true,
	"private_key": true, "otp": true, "otp_code": true, "totp_code": true, "recovery_code": true,
	"national_id": true,
}

// The patterns that redactText hides, in order: credentials of a URL before
// its email-like user@host, then tokens, secrets named in the text, email
// addresses, and mobile numbers.
var redactions = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`), "${1}[redacted]@"},
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), "[redacted-token]"},
	{regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`), "Bearer [redacted]"},
	{regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api[_-]?key|otp|totp_code|recovery_code)(\s*[=:]\s*)\S+`), "${1}${2}[redacted]"},
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "[redacted-email]"},
	{regexp.MustCompile(`(?:\+98|0098|\b0)9\d{9}\b|\+\d{10,15}\b`), "[redacted-phone]"},
}

// redactText hides the secrets, tokens, and contact details in s.
func redactText(s string) string {
	for _, r := range redactions {
		s = r.pattern.ReplaceAllString(s, r.replacement)
	}
	return s
}

// redactAttr hides an attribute whose key is sensitive, and the secrets in
// the text of any other. It is the ReplaceAttr of the handlers.
func redactAttr(_ []string, a slog.Attr) slog.Attr {
	return redact(a)
}

func redact(a slog.Attr) slog.Attr {
	if sensitiveKeys[strings.ToLower(a.Key)] {
		return slog.String(a.Key, "[redacted]")
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, redactText(a.Value.String()))
	case slog.KindGroup:
		attrs := a.Value.Group()
		out := make([]any, len(attrs))
		for i, g := range attrs {
			out[i] = redact(g)
		}
		return slog.Group(a.Key, out...)
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, redactText(err.Error()))
		}
	}
	return a
}

// redactingHandler hides the secrets in the message of every record, which
// ReplaceAttr does not see, and in its attributes.
type redactingHandler struct {
	next slog.Handler
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, redactText(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redact(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		redacted[i] = redact(a)
	}
	return &redactingHandler{next: h.next.WithAttrs(redacted)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{next: h.next.WithGroup(name)}
}
