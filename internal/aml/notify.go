package aml

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// TextSender sends a plain-text email: *notify.EmailDeliverer.
type TextSender interface {
	SendText(ctx context.Context, to, subject, text string) error
}

// EmailNotifier emails the notices to the compliance team: the alerts and
// their rules, never the user's contacts or amounts, which the admin API
// shows to whoever may see them.
type EmailNotifier struct {
	Sender     TextSender
	Recipients []string
}

// Notify sends one message of every notice to each recipient.
func (n EmailNotifier) Notify(ctx context.Context, notices []Notice) error {
	if len(notices) == 0 {
		return nil
	}
	var b strings.Builder
	for _, x := range notices {
		what := string(x.Action)
		if x.Escalated {
			what = "escalated, " + what
		}
		fmt.Fprintf(&b, "- alert %s: rule %s, user %s (%s)\n", x.AlertID, x.RuleCode, x.UserHK, what)
	}
	subject := fmt.Sprintf("AML: %d alert(s) to review", len(notices))
	text := "These AML alerts need a review in the admin API (GET /api/v1/admin/aml/alerts):\n\n" + b.String()
	var errs []error
	for _, to := range n.Recipients {
		if err := n.Sender.SendText(ctx, to, subject, text); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", to, err))
		}
	}
	return errors.Join(errs...)
}
