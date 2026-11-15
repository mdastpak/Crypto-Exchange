package delisting

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/antiphishing"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/notify"
)

// The notices of a delisting (#216) tell the users of each stage that it
// reaches, remind those who hold the currency of the freeze, and tell them
// of its cancel. Users who hold the currency are told by every channel
// (email and SMS; push and the inbox of #170 when they exist), and first;
// the others by the free ones (email; push later), and not reminded.
const (
	// NoticeWindow is how late a notice may still go out: a stage applied,
	// or a reminder due, longer ago, as after a long outage or before the
	// notices existed, is not told.
	NoticeWindow = 48 * time.Hour
	// NoticeRetryDelay is how long a notice that could not be sent waits,
	// times its attempts, before it is tried again.
	NoticeRetryDelay = 15 * time.Minute
	// NoticeMaxAttempts is how many times a notice is tried before it is
	// given up as failed.
	NoticeMaxAttempts = 5
	// DefaultNoticeBudget is how long a run sends notices before it leaves
	// the rest for the next run.
	DefaultNoticeBudget = 20 * time.Second
	// noticeBatch is how many recipients a transaction claims.
	noticeBatch = 50
)

// DefaultReminderDays are the days before the freeze that holders are
// reminded on.
var DefaultReminderDays = []int{30, 14, 7, 3, 1}

// The outcomes of a recipient.
const (
	OutcomeSent      = "sent"
	OutcomeNoContact = "no_contact"
	OutcomeStale     = "stale"
	OutcomeFailed    = "failed"
)

// PhraseShower returns the anti-phishing code of a user for an email that
// shows it, and counts the use (#375): antiphishing.Store.
type PhraseShower interface {
	Show(ctx context.Context, userHK string) (string, error)
}

// Notifier sends the notices of delistings. Every instance may run it at
// once: a notice is planned once by its primary key, fanned out once by
// the instance that claims it, and each recipient is sent by the instance
// that claims it with SKIP LOCKED. A recipient is sent at least once: one
// whose transaction fails to commit after the send goes out again, but a
// channel that a retry finds recorded is not sent again.
type Notifier struct {
	Sender notify.OTPSender
	// SMS reports that the SMS provider has a template for the notices.
	// Without one, they go by email only.
	SMS bool
	// Phrases shows the anti-phishing code in the emails; nil shows none.
	Phrases PhraseShower
	// ReminderDays are the days before the freeze that holders are
	// reminded on; nil is DefaultReminderDays, and empty is none.
	ReminderDays []int
	// Budget bounds a run; 0 is DefaultNoticeBudget.
	Budget time.Duration
}

// Advance is the notifier as an advancer of the scheduler, after the
// steps: it plans the notices that are due, fans them out, and sends them
// until none is left or the budget is spent. It changes no currency.
func (n Notifier) Advance(ctx context.Context, db *sql.DB, now time.Time, _ int) ([]currencies.Change, error) {
	if n.Sender == nil {
		return nil, nil
	}
	if err := n.Plan(ctx, db, now); err != nil {
		return nil, err
	}
	for {
		prepared, err := Prepare(ctx, db, now)
		if err != nil {
			return nil, err
		}
		if !prepared {
			break
		}
	}
	budget := n.Budget
	if budget <= 0 {
		budget = DefaultNoticeBudget
	}
	until := time.Now().Add(budget)
	for time.Now().Before(until) && ctx.Err() == nil {
		claimed, err := n.Send(ctx, db, now)
		if err != nil || claimed < noticeBatch {
			return nil, err
		}
	}
	return nil, nil
}

// Plan writes the notices that are due at now and not written yet: the
// stage that each delisting reached last, a reminder of the freeze for
// each of ReminderDays that came, and the cancel of a delisting that told
// its users anything.
func (n Notifier) Plan(ctx context.Context, db *sql.DB, now time.Time) error {
	since := now.Add(-NoticeWindow)
	// Of the steps applied at once, as by an emergency or after a pause,
	// only the last is told.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO currency_delisting_notices (delisting_hk, kind, stage, due_at)
		SELECT s.delisting_hk, 'step:' || s.state, s.state, s.applied_at
		FROM currency_delisting_steps s JOIN currency_delistings d ON d.hk = s.delisting_hk
		WHERE s.applied_at IS NOT NULL AND s.applied_at >= $1 AND s.applied_at <= $2 AND d.status <> 'cancelled'
		  AND NOT EXISTS (SELECT 1 FROM currency_delisting_steps l WHERE l.delisting_hk = s.delisting_hk
		    AND l.applied_at IS NOT NULL AND (l.applied_at, l.scheduled_at) > (s.applied_at, s.scheduled_at))
		ON CONFLICT DO NOTHING`, since, now); err != nil {
		return fmt.Errorf("plan the notices of the steps: %w", err)
	}
	days := n.ReminderDays
	if days == nil {
		days = DefaultReminderDays
	}
	if len(days) > 0 {
		// A reminder whose day came before the delisting was scheduled, or
		// before it told anything, is left out: its first notice says when
		// the freeze is.
		list := make([]string, len(days))
		for i, d := range days {
			list[i] = fmt.Sprint(d)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO currency_delisting_notices (delisting_hk, kind, stage, due_at)
			SELECT d.hk, 'reminder:' || r.days || 'd', 'reminder', s.scheduled_at - r.days * interval '1 day'
			FROM currency_delistings d
			JOIN currency_delisting_steps s ON s.delisting_hk = d.hk AND s.state = 'frozen' AND s.applied_at IS NULL
			CROSS JOIN unnest(string_to_array($3, ',')::int[]) AS r(days)
			WHERE d.status = 'scheduled' AND s.scheduled_at > $2
			  AND s.scheduled_at - r.days * interval '1 day' BETWEEN $1 AND $2
			  AND s.scheduled_at - r.days * interval '1 day' >= d.created_at
			  AND EXISTS (SELECT 1 FROM currency_delisting_notices f WHERE f.delisting_hk = d.hk
			    AND f.kind LIKE 'step:%' AND f.due_at < s.scheduled_at - r.days * interval '1 day')
			ON CONFLICT DO NOTHING`, since, now, strings.Join(list, ",")); err != nil {
			return fmt.Errorf("plan the reminders of the freezes: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO currency_delisting_notices (delisting_hk, kind, stage, due_at)
		SELECT d.hk, 'cancelled', 'cancelled', d.cancelled_at
		FROM currency_delistings d JOIN currencies c ON c.hk = d.currency_hk
		WHERE d.status = 'cancelled' AND d.cancelled_at BETWEEN $1 AND $2 AND c.deleted_at IS NULL
		  AND EXISTS (SELECT 1 FROM currency_delisting_notices f WHERE f.delisting_hk = d.hk)
		ON CONFLICT DO NOTHING`, since, now); err != nil {
		return fmt.Errorf("plan the notices of the cancels: %w", err)
	}
	return nil
}

// Prepare fans out the notice that is due first and has no recipients yet,
// and reports false when there is none. The recipients are every user who
// holds the currency, and, but for a reminder, every other user. A notice
// that is stale by then, such as a step of a delisting cancelled since,
// gets none.
func Prepare(ctx context.Context, db *sql.DB, now time.Time) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var delistingHK, kind, currencyHK string
	var status Status
	var deleted bool
	err = tx.QueryRowContext(ctx, `
		SELECT n.delisting_hk, n.kind, d.status, d.currency_hk, c.deleted_at IS NOT NULL
		FROM currency_delisting_notices n
		JOIN currency_delistings d ON d.hk = n.delisting_hk JOIN currencies c ON c.hk = d.currency_hk
		WHERE n.prepared_at IS NULL AND n.due_at <= $1
		ORDER BY n.due_at, n.delisting_hk, n.kind
		FOR UPDATE OF n SKIP LOCKED LIMIT 1`, now).Scan(&delistingHK, &kind, &status, &currencyHK, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim a notice: %w", err)
	}
	if !deleted && (kind == notify.DelistingStageCancelled || status != StatusCancelled) {
		everyone := !strings.HasPrefix(kind, "reminder:")
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO currency_delisting_notice_recipients (delisting_hk, kind, user_hk, holder, next_attempt_at)
			SELECT $1, $2, u.hk, h.holder, $5
			FROM users u CROSS JOIN LATERAL (SELECT EXISTS (SELECT 1 FROM balances b WHERE b.user_id = u.hk
			  AND b.currency_hk = $3 AND b.available::numeric + b.locked::numeric + b.pending_deposit::numeric <> 0) AS holder) h
			WHERE u.deleted_at IS NULL AND ($4 OR h.holder)
			ON CONFLICT DO NOTHING`, delistingHK, kind, currencyHK, everyone, now); err != nil {
			return false, fmt.Errorf("write the recipients of a notice: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE currency_delisting_notices n SET prepared_at = $3, recipients = c.total, holders = c.holders
		FROM (SELECT count(*) AS total, count(*) FILTER (WHERE holder) AS holders FROM currency_delisting_notice_recipients
		  WHERE delisting_hk = $1 AND kind = $2) c
		WHERE n.delisting_hk = $1 AND n.kind = $2`, delistingHK, kind, now); err != nil {
		return false, fmt.Errorf("mark a notice prepared: %w", err)
	}
	return true, tx.Commit()
}

// recipient is a recipient that a run claimed.
type recipient struct {
	delistingHK, kind, userHK string
	holder, deleted           bool
	attempts                  int
	sent                      []string
}

// Send sends the recipients that are due, holders first, at most a batch
// of them, and returns how many it claimed. It stops at the first that
// cannot be sent, which is tried again after NoticeRetryDelay times its
// attempts, and returns the error.
func (n Notifier) Send(ctx context.Context, db *sql.DB, now time.Time) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT r.delisting_hk, r.kind, r.user_hk, r.holder, r.attempts, array_to_string(r.sent_channels, ','),
			u.deleted_at IS NOT NULL
		FROM currency_delisting_notice_recipients r JOIN users u ON u.hk = r.user_hk
		WHERE r.done_at IS NULL AND r.next_attempt_at <= $1
		ORDER BY r.holder DESC, r.next_attempt_at, r.delisting_hk, r.kind, r.user_hk
		LIMIT $2 FOR UPDATE OF r SKIP LOCKED`, now, noticeBatch)
	if err != nil {
		return 0, fmt.Errorf("claim the recipients of the notices: %w", err)
	}
	var claimed []recipient
	for rows.Next() {
		var r recipient
		var sent string
		if err := rows.Scan(&r.delistingHK, &r.kind, &r.userHK, &r.holder, &r.attempts, &sent, &r.deleted); err != nil {
			rows.Close()
			return 0, fmt.Errorf("read a recipient: %w", err)
		}
		if sent != "" {
			r.sent = strings.Split(sent, ",")
		}
		claimed = append(claimed, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read the recipients: %w", err)
	}
	notices := map[[2]string]*notice{}
	var sendErr error
	for _, r := range claimed {
		key := [2]string{r.delistingHK, r.kind}
		nt, ok := notices[key]
		if !ok {
			if nt, err = readNotice(ctx, tx, r.delistingHK, r.kind, now); err != nil {
				return 0, err
			}
			notices[key] = nt
		}
		if sendErr = n.sendOne(ctx, tx, nt, r, now); sendErr != nil {
			break
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("record the notices sent: %w", err)
	}
	return len(claimed), sendErr
}

// notice is what a notice says, as its recipients are sent.
type notice struct {
	stale bool
	msg   notify.DelistingNotice
}

// readNotice reads the notice of a delisting of kind, at now.
func readNotice(ctx context.Context, tx *sql.Tx, delistingHK, kind string, now time.Time) (*notice, error) {
	var nt notice
	var status Status
	var deleted bool
	var frozenAt sql.NullTime
	err := tx.QueryRowContext(ctx, `
		SELECT n.stage, d.status, c.deleted_at IS NOT NULL, COALESCE(c.info->>'symbol', ''), d.settlement,
			COALESCE(t.info->>'symbol', ''), COALESCE(d.claim_days, 0),
			(SELECT s.scheduled_at FROM currency_delisting_steps s WHERE s.delisting_hk = d.hk AND s.state = 'frozen' AND s.applied_at IS NULL)
		FROM currency_delisting_notices n
		JOIN currency_delistings d ON d.hk = n.delisting_hk JOIN currencies c ON c.hk = d.currency_hk
		LEFT JOIN currencies t ON t.hk = d.settlement_currency_hk
		WHERE n.delisting_hk = $1 AND n.kind = $2`, delistingHK, kind).
		Scan(&nt.msg.Stage, &status, &deleted, &nt.msg.Currency, &nt.msg.Settlement, &nt.msg.SettlementCurrency,
			&nt.msg.ClaimDays, &frozenAt)
	if err != nil {
		return nil, fmt.Errorf("read a notice: %w", err)
	}
	cancelled := nt.msg.Stage == notify.DelistingStageCancelled
	nt.stale = deleted || (!cancelled && status == StatusCancelled)
	switch {
	case cancelled, nt.msg.Stage == string(currencies.LifecycleDelisted):
		// Nothing is left to do: no deadline, and no settlement.
		nt.msg.Settlement, nt.msg.SettlementCurrency, nt.msg.ClaimDays = "", "", 0
	case frozenAt.Valid && frozenAt.Time.After(now):
		nt.msg.Deadline = frozenAt.Time.UTC().Format("2006-01-02 15:04 UTC")
		nt.msg.DaysLeft = int(math.Ceil(frozenAt.Time.Sub(now).Hours() / 24))
	}
	if nt.msg.Settlement != string(SettlementClaim) {
		nt.msg.ClaimDays = 0
	}
	return &nt, nil
}

// sendOne sends the notice nt to the recipient r and records the outcome
// in tx. It returns the error of a send that failed, which is recorded
// as an attempt.
func (n Notifier) sendOne(ctx context.Context, tx *sql.Tx, nt *notice, r recipient, now time.Time) error {
	if nt.stale || r.deleted {
		return finish(ctx, tx, r, OutcomeStale, now)
	}
	contacts, err := antiphishing.VerifiedContacts(ctx, tx, r.userHK)
	if err != nil {
		return err
	}
	msg := notify.OTPMessage{Purpose: notify.PurposeDelistingNotice, Delisting: nt.msg}
	msg.Delisting.Holder = r.holder
	var sendErr error
	for _, c := range contacts {
		if !n.channel(c.Type, r.holder, nt.msg.Stage) || slices.Contains(r.sent, c.Type) {
			continue
		}
		m := msg
		m.ContactType, m.Contact, m.MaskedContact = c.Type, c.Value, notify.MaskContact(c.Type, c.Value)
		if c.Type == notify.ContactEmail && n.Phrases != nil {
			if m.AntiPhishingCode, err = n.Phrases.Show(ctx, r.userHK); err != nil {
				log.Printf("Failed to read the anti-phishing code of user %s for a delisting notice: %v", r.userHK, err)
				m.AntiPhishingCode = ""
			}
		}
		if err := n.Sender.SendOTP(ctx, m); err != nil {
			sendErr = fmt.Errorf("send the notice of a delisting to %s: %w", m.MaskedContact, err)
			break
		}
		r.sent = append(r.sent, c.Type)
		if _, err := tx.ExecContext(ctx, `UPDATE currency_delisting_notice_recipients SET sent_channels = array_append(sent_channels, $4)
			WHERE delisting_hk = $1 AND kind = $2 AND user_hk = $3`, r.delistingHK, r.kind, r.userHK, c.Type); err != nil {
			return fmt.Errorf("record a notice sent: %w", err)
		}
	}
	if sendErr != nil {
		r.attempts++
		if r.attempts >= NoticeMaxAttempts {
			log.Printf("Gave up the notice %s of the delisting %s to user %s after %d attempts: %v",
				r.kind, r.delistingHK, r.userHK, r.attempts, sendErr)
			return errors.Join(sendErr, finish(ctx, tx, r, OutcomeFailed, now))
		}
		if _, err := tx.ExecContext(ctx, `UPDATE currency_delisting_notice_recipients SET attempts = $4, next_attempt_at = $5
			WHERE delisting_hk = $1 AND kind = $2 AND user_hk = $3`, r.delistingHK, r.kind, r.userHK, r.attempts,
			now.Add(time.Duration(r.attempts)*NoticeRetryDelay)); err != nil {
			return errors.Join(sendErr, fmt.Errorf("delay a notice: %w", err))
		}
		return sendErr
	}
	if len(r.sent) == 0 {
		return finish(ctx, tx, r, OutcomeNoContact, now)
	}
	return finish(ctx, tx, r, OutcomeSent, now)
}

// channel reports whether a notice of stage goes by a contact of
// contactType to a recipient: email to every user, and SMS to holders
// only, when the provider has a template for it. The cancel goes by email
// only: the SMS template tells of a delisting.
func (n Notifier) channel(contactType string, holder bool, stage string) bool {
	if !n.Sender.Supports(contactType) {
		return false
	}
	switch contactType {
	case notify.ContactEmail:
		return true
	case notify.ContactMobile:
		return holder && n.SMS && stage != notify.DelistingStageCancelled
	}
	return false
}

// finish records the outcome of a recipient.
func finish(ctx context.Context, tx *sql.Tx, r recipient, outcome string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE currency_delisting_notice_recipients SET outcome = $4, done_at = $5, attempts = $6
		WHERE delisting_hk = $1 AND kind = $2 AND user_hk = $3`, r.delistingHK, r.kind, r.userHK, outcome, now, r.attempts); err != nil {
		return fmt.Errorf("record the outcome of a notice: %w", err)
	}
	return nil
}

// NoticeReport is a notice of a delisting, as administrators see it: what
// it told, when, to how many, and how its sends went.
type NoticeReport struct {
	// Kind is step:<state>, reminder:<days>d, or cancelled.
	Kind  string    `json:"kind" example:"step:deposits_disabled"`
	Stage string    `json:"stage" example:"deposits_disabled"`
	DueAt time.Time `json:"due_at"`
	// PreparedAt is when the recipients were written; nil until then.
	PreparedAt *time.Time `json:"prepared_at,omitempty"`
	// Recipients are the users that it goes to, and Holders those of them
	// who held the currency.
	Recipients int `json:"recipients"`
	Holders    int `json:"holders"`
	// The recipients by outcome, and those still to send.
	Sent      int `json:"sent"`
	NoContact int `json:"no_contact"`
	Stale     int `json:"stale"`
	Failed    int `json:"failed"`
	Pending   int `json:"pending"`
}

// Notices returns the notices of a delisting of a currency, in the order
// they were due.
func Notices(ctx context.Context, db *sql.DB, currencyHK, hk string) ([]NoticeReport, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM currency_delistings WHERE hk = $1 AND currency_hk = $2)`,
		hk, currencyHK).Scan(&exists); err != nil {
		return nil, fmt.Errorf("read the delisting: %w", err)
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := db.QueryContext(ctx, `
		SELECT n.kind, n.stage, n.due_at, n.prepared_at, COALESCE(n.recipients, 0), COALESCE(n.holders, 0),
			count(*) FILTER (WHERE r.outcome = 'sent'), count(*) FILTER (WHERE r.outcome = 'no_contact'),
			count(*) FILTER (WHERE r.outcome = 'stale'), count(*) FILTER (WHERE r.outcome = 'failed'),
			count(*) FILTER (WHERE r.user_hk IS NOT NULL AND r.done_at IS NULL)
		FROM currency_delisting_notices n
		LEFT JOIN currency_delisting_notice_recipients r ON r.delisting_hk = n.delisting_hk AND r.kind = n.kind
		WHERE n.delisting_hk = $1
		GROUP BY n.delisting_hk, n.kind
		ORDER BY n.due_at, n.kind`, hk)
	if err != nil {
		return nil, fmt.Errorf("read the notices: %w", err)
	}
	defer rows.Close()
	out := []NoticeReport{}
	for rows.Next() {
		var r NoticeReport
		var prepared sql.NullTime
		if err := rows.Scan(&r.Kind, &r.Stage, &r.DueAt, &prepared, &r.Recipients, &r.Holders,
			&r.Sent, &r.NoContact, &r.Stale, &r.Failed, &r.Pending); err != nil {
			return nil, fmt.Errorf("read a notice: %w", err)
		}
		r.DueAt = r.DueAt.UTC()
		if prepared.Valid {
			t := prepared.Time.UTC()
			r.PreparedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
