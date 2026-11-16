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
	"github.com/mdastpak/crypto-exchange/internal/metrics"
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
	// noticeBatch is how many recipients a transaction claims, and how many
	// other users a batch of a broadcast sends.
	noticeBatch = 50
	// DefaultPrepareBatch is how many holders a transaction writes as
	// recipients (#461), well within the statement timeout.
	DefaultPrepareBatch = 5000
	// staleBatch is how many other users a batch of a stale broadcast
	// counts, sending nothing.
	staleBatch = 5000
	// PrepareRetryDelay is how long a notice that failed to prepare waits,
	// times its attempts, up to an hour.
	PrepareRetryDelay = time.Minute
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
	// PrepareBatch is how many holders a transaction writes; 0 is
	// DefaultPrepareBatch.
	PrepareBatch int
}

// Advance is the notifier as an advancer of the scheduler, after the
// steps: it plans the notices that are due, prepares them, sends their
// recipients, holders first, and broadcasts them to the other users, until
// none is left or the budget is spent. It changes no currency. A notice
// that cannot be prepared, or a recipient that cannot be sent, delays only
// itself (#461): the others go on, and the errors are returned together at
// the end of the run.
func (n Notifier) Advance(ctx context.Context, db *sql.DB, now time.Time, _ int) ([]currencies.Change, error) {
	if n.Sender == nil {
		return nil, nil
	}
	budget := n.Budget
	if budget <= 0 {
		budget = DefaultNoticeBudget
	}
	until := time.Now().Add(budget)
	more := func() bool { return time.Now().Before(until) && ctx.Err() == nil }
	var errs []error
	if err := n.Plan(ctx, db, now); err != nil {
		errs = append(errs, err)
	}
	for more() {
		prepared, err := n.prepare(ctx, db, now)
		if err != nil {
			errs = append(errs, err)
		}
		if !prepared {
			break
		}
	}
	for more() {
		claimed, err := n.Send(ctx, db, now)
		if err != nil {
			errs = append(errs, err)
		}
		if claimed < noticeBatch {
			break
		}
	}
	for more() {
		sent, err := n.Broadcast(ctx, db, now)
		if err != nil {
			errs = append(errs, err)
		}
		if !sent {
			break
		}
	}
	if err := observeNotices(ctx, db, now); err != nil {
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
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

// Prepare writes one batch of the recipients of the notice that is due
// first and is not prepared yet, at DefaultPrepareBatch, and reports false
// when there is none.
func Prepare(ctx context.Context, db *sql.DB, now time.Time) (bool, error) {
	return Notifier{}.prepare(ctx, db, now)
}

// prepare writes one batch of the recipients of the notice that is due
// first and is not prepared yet, and reports false when there is none
// (#461). The recipients written are the users who hold the currency, in
// keyset batches over their balances: a batch is a transaction, well
// within the statement timeout, and the next run goes on from the cursor.
// When the last batch is written, the notice is prepared; one that goes
// to everyone then broadcasts to the other users (Broadcast). A notice
// that is stale by then, such as a step of a delisting cancelled since,
// gets none. A notice whose batch fails waits PrepareRetryDelay times its
// attempts, and the error is returned: the next call prepares another.
func (n Notifier) prepare(ctx context.Context, db *sql.DB, now time.Time) (bool, error) {
	batch := n.PrepareBatch
	if batch <= 0 {
		batch = DefaultPrepareBatch
	}
	delistingHK, kind, err := prepareBatch(ctx, db, now, batch)
	if err == nil || delistingHK == "" {
		return delistingHK != "", err
	}
	metrics.DelistingNoticeFailures.WithLabelValues("prepare").Inc()
	var attempts int
	if dbErr := db.QueryRowContext(ctx, `UPDATE currency_delisting_notices
		SET prepare_attempts = prepare_attempts + 1, prepare_error = left($3, 200),
			prepare_retry_at = $4::timestamptz + LEAST(interval '1 hour', (prepare_attempts + 1) * $5 * interval '1 second')
		WHERE delisting_hk = $1 AND kind = $2 RETURNING prepare_attempts`,
		delistingHK, kind, err.Error(), now, PrepareRetryDelay.Seconds()).Scan(&attempts); dbErr != nil {
		// The database is away: stop preparing in this run.
		return false, errors.Join(err, fmt.Errorf("delay a notice that failed to prepare: %w", dbErr))
	}
	log.Printf("The notice %s of the delisting %s failed to prepare (attempt %d), and waits: %v", kind, delistingHK, attempts, err)
	return true, err
}

// prepareBatch writes one batch of the recipients of a notice, and returns
// the notice it claimed, if any.
func prepareBatch(ctx context.Context, db *sql.DB, now time.Time, batch int) (string, string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	var delistingHK, kind, currencyHK string
	var status Status
	var deleted bool
	var cursor sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT n.delisting_hk, n.kind, d.status, d.currency_hk, c.deleted_at IS NOT NULL, n.prepare_cursor::text
		FROM currency_delisting_notices n
		JOIN currency_delistings d ON d.hk = n.delisting_hk JOIN currencies c ON c.hk = d.currency_hk
		WHERE n.prepared_at IS NULL AND n.due_at <= $1 AND (n.prepare_retry_at IS NULL OR n.prepare_retry_at <= $1)
		ORDER BY n.due_at, n.delisting_hk, n.kind
		FOR UPDATE OF n SKIP LOCKED LIMIT 1`, now).Scan(&delistingHK, &kind, &status, &currencyHK, &deleted, &cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("claim a notice: %w", err)
	}
	fail := func(err error) (string, string, error) { return delistingHK, kind, err }
	stale := deleted || (kind != notify.DelistingStageCancelled && status == StatusCancelled)
	written, last := 0, ""
	if !stale {
		var lastHK sql.NullString
		if err := tx.QueryRowContext(ctx, `
			WITH batch AS (
				SELECT DISTINCT b.user_id FROM balances b JOIN users u ON u.hk = b.user_id AND u.deleted_at IS NULL
				WHERE b.currency_hk = $3 AND ($4::uuid IS NULL OR b.user_id > $4::uuid)
				  AND b.available::numeric + b.locked::numeric + b.pending_deposit::numeric <> 0
				ORDER BY b.user_id LIMIT $6),
			written AS (
				INSERT INTO currency_delisting_notice_recipients (delisting_hk, kind, user_hk, holder, next_attempt_at)
				SELECT $1, $2, user_id, true, $5 FROM batch
				ON CONFLICT DO NOTHING)
			SELECT count(*), (SELECT user_id::text FROM batch ORDER BY user_id DESC LIMIT 1) FROM batch`,
			delistingHK, kind, currencyHK, cursor, now, batch).Scan(&written, &lastHK); err != nil {
			return fail(fmt.Errorf("write the recipients of a notice: %w", err))
		}
		last = lastHK.String
	}
	if written == batch {
		// More holders may follow: the next batch goes on from here.
		if _, err := tx.ExecContext(ctx, `UPDATE currency_delisting_notices SET prepare_cursor = $3::uuid
			WHERE delisting_hk = $1 AND kind = $2`, delistingHK, kind, last); err != nil {
			return fail(fmt.Errorf("record the cursor of a notice: %w", err))
		}
		return delistingHK, kind, tx.Commit()
	}
	everyone := !stale && !strings.HasPrefix(kind, "reminder:")
	if _, err := tx.ExecContext(ctx, `
		UPDATE currency_delisting_notices n SET prepared_at = $3, recipients = c.total, holders = c.total, broadcast = $4,
			prepare_retry_at = NULL, prepare_error = NULL
		FROM (SELECT count(*) AS total FROM currency_delisting_notice_recipients WHERE delisting_hk = $1 AND kind = $2) c
		WHERE n.delisting_hk = $1 AND n.kind = $2`, delistingHK, kind, now, everyone); err != nil {
		return fail(fmt.Errorf("mark a notice prepared: %w", err))
	}
	return delistingHK, kind, tx.Commit()
}

// recipient is a recipient that a run claimed.
type recipient struct {
	delistingHK, kind, userHK string
	holder, deleted           bool
	attempts                  int
	sent                      []string
}

// Send sends the recipients that are due, holders first, at most a batch
// of them, and returns how many it claimed. A recipient that cannot be
// sent is tried again after NoticeRetryDelay times its attempts, and the
// batch goes on with the next one (#461); the errors are returned
// together.
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
	var errs []error
	for _, r := range claimed {
		key := [2]string{r.delistingHK, r.kind}
		nt, ok := notices[key]
		if !ok {
			if nt, err = readNotice(ctx, tx, r.delistingHK, r.kind, now); err != nil {
				return 0, err
			}
			notices[key] = nt
		}
		if err := n.sendOne(ctx, tx, nt, r, now); err != nil {
			var failed *sendFailure
			if !errors.As(err, &failed) {
				// The database failed: what this transaction recorded is
				// lost, and the batch is tried again.
				return 0, err
			}
			metrics.DelistingNoticeFailures.WithLabelValues("send").Inc()
			errs = append(errs, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("record the notices sent: %w", err)
	}
	return len(claimed), errors.Join(errs...)
}

// sendFailure is a send of a notice that failed, recorded as an attempt;
// any other error of sendOne is one of the database.
type sendFailure struct{ err error }

func (f *sendFailure) Error() string { return f.err.Error() }
func (f *sendFailure) Unwrap() error { return f.err }

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

// deliver sends the notice nt to the user of r by each channel that it
// goes by and that r.sent does not hold, and returns the channels sent,
// those of r.sent included. It stops at the first send that fails, and
// returns its error with the channels sent before it.
func (n Notifier) deliver(ctx context.Context, q *sql.Tx, nt *notice, r recipient) ([]string, error) {
	contacts, err := antiphishing.VerifiedContacts(ctx, q, r.userHK)
	if err != nil {
		return r.sent, err
	}
	msg := notify.OTPMessage{Purpose: notify.PurposeDelistingNotice, Delisting: nt.msg}
	msg.Delisting.Holder = r.holder
	sent := slices.Clone(r.sent)
	for _, c := range contacts {
		if !n.channel(c.Type, r.holder, nt.msg.Stage) || slices.Contains(sent, c.Type) {
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
			return sent, &sendFailure{fmt.Errorf("send the notice of a delisting to %s: %w", m.MaskedContact, err)}
		}
		sent = append(sent, c.Type)
	}
	return sent, nil
}

// sendOne sends the notice nt to the recipient r and records the outcome
// in tx. A send that failed is recorded as an attempt, and returned as a
// *sendFailure; any other error is of the database.
func (n Notifier) sendOne(ctx context.Context, tx *sql.Tx, nt *notice, r recipient, now time.Time) error {
	if nt.stale || r.deleted {
		return finish(ctx, tx, r, OutcomeStale, now)
	}
	sent, sendErr := n.deliver(ctx, tx, nt, r)
	var failed *sendFailure
	if sendErr != nil && !errors.As(sendErr, &failed) {
		return sendErr
	}
	if len(sent) > len(r.sent) {
		if _, err := tx.ExecContext(ctx, `UPDATE currency_delisting_notice_recipients SET sent_channels = $4
			WHERE delisting_hk = $1 AND kind = $2 AND user_hk = $3`, r.delistingHK, r.kind, r.userHK, "{"+strings.Join(sent, ",")+"}"); err != nil {
			return fmt.Errorf("record a notice sent: %w", err)
		}
		r.sent = sent
	}
	if sendErr != nil {
		r.attempts++
		if r.attempts >= NoticeMaxAttempts {
			log.Printf("Gave up the notice %s of the delisting %s to user %s after %d attempts: %v",
				r.kind, r.delistingHK, r.userHK, r.attempts, sendErr)
			if err := finish(ctx, tx, r, OutcomeFailed, now); err != nil {
				return err
			}
			return sendErr
		}
		if _, err := tx.ExecContext(ctx, `UPDATE currency_delisting_notice_recipients SET attempts = $4, next_attempt_at = $5
			WHERE delisting_hk = $1 AND kind = $2 AND user_hk = $3`, r.delistingHK, r.kind, r.userHK, r.attempts,
			now.Add(time.Duration(r.attempts)*NoticeRetryDelay)); err != nil {
			return fmt.Errorf("delay a notice: %w", err)
		}
		return sendErr
	}
	if len(r.sent) == 0 {
		return finish(ctx, tx, r, OutcomeNoContact, now)
	}
	return finish(ctx, tx, r, OutcomeSent, now)
}

// Broadcast sends one batch of the notice that goes to everyone to the
// users who are not its recipients, the users who held nothing when it was
// prepared, and reports false when no notice has one left (#461). They
// are not written as recipients: the notice counts them, and keeps a
// cursor over the users, so that a notice to every user does not store a
// row per user. A user whose send fails is written as a recipient, to be
// tried again as one. The batch locks its notice, so that instances share
// the notices, and a user is told once, but for a commit that fails after
// its sends. A notice that turned stale counts the rest as stale.
func (n Notifier) Broadcast(ctx context.Context, db *sql.DB, now time.Time) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var delistingHK, kind string
	var cursor sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT delisting_hk, kind, broadcast_cursor::text FROM currency_delisting_notices
		WHERE broadcast AND broadcast_done_at IS NULL AND prepared_at IS NOT NULL
		ORDER BY due_at, delisting_hk, kind
		FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&delistingHK, &kind, &cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim a broadcast: %w", err)
	}
	nt, err := readNotice(ctx, tx, delistingHK, kind, now)
	if err != nil {
		return false, err
	}
	batch := noticeBatch
	if nt.stale {
		batch = staleBatch
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT u.hk::text FROM users u
		WHERE u.deleted_at IS NULL AND ($3::uuid IS NULL OR u.hk > $3::uuid)
		  AND NOT EXISTS (SELECT 1 FROM currency_delisting_notice_recipients r
		    WHERE r.delisting_hk = $1 AND r.kind = $2 AND r.user_hk = u.hk)
		ORDER BY u.hk LIMIT $4`, delistingHK, kind, cursor, batch)
	if err != nil {
		return false, fmt.Errorf("read the users of a broadcast: %w", err)
	}
	var users []string
	for rows.Next() {
		var hk string
		if err := rows.Scan(&hk); err != nil {
			rows.Close()
			return false, fmt.Errorf("read a user of a broadcast: %w", err)
		}
		users = append(users, hk)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read the users of a broadcast: %w", err)
	}
	var sent, noContact, stale int
	var errs []error
	for _, userHK := range users {
		if nt.stale {
			stale++
			continue
		}
		r := recipient{delistingHK: delistingHK, kind: kind, userHK: userHK}
		channels, err := n.deliver(ctx, tx, nt, r)
		var failed *sendFailure
		switch {
		case err != nil && !errors.As(err, &failed):
			return false, err
		case err != nil:
			// It is tried again as a recipient, without the channels sent.
			metrics.DelistingNoticeFailures.WithLabelValues("send").Inc()
			errs = append(errs, err)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO currency_delisting_notice_recipients (delisting_hk, kind, user_hk, holder, sent_channels, attempts, next_attempt_at)
				VALUES ($1, $2, $3, false, $4, 1, $5)`, delistingHK, kind, userHK, "{"+strings.Join(channels, ",")+"}",
				now.Add(NoticeRetryDelay)); err != nil {
				return false, fmt.Errorf("write a recipient whose notice failed: %w", err)
			}
		case len(channels) == 0:
			noContact++
		default:
			sent++
		}
	}
	var last any
	if len(users) > 0 {
		last = users[len(users)-1]
	}
	done := len(users) < batch
	if _, err := tx.ExecContext(ctx, `UPDATE currency_delisting_notices
		SET broadcast_cursor = COALESCE($3::uuid, broadcast_cursor), broadcast_sent = broadcast_sent + $4,
			broadcast_no_contact = broadcast_no_contact + $5, broadcast_stale = broadcast_stale + $6,
			broadcast_done_at = CASE WHEN $7 THEN $8::timestamptz END
		WHERE delisting_hk = $1 AND kind = $2`, delistingHK, kind, last, sent, noContact, stale, done, now); err != nil {
		return false, fmt.Errorf("record a batch of a broadcast: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("record a batch of a broadcast: %w", err)
	}
	return true, errors.Join(errs...)
}

// observeNotices sets the gauges of the notices: those waiting to be
// prepared, the broadcasts left, and the recipients waiting, by holder,
// with the age of the oldest due one, from the notice it belongs to.
func observeNotices(ctx context.Context, db *sql.DB, now time.Time) error {
	var unprepared, broadcasts int
	if err := db.QueryRowContext(ctx, `SELECT
		count(*) FILTER (WHERE prepared_at IS NULL AND due_at <= $1),
		count(*) FILTER (WHERE broadcast AND broadcast_done_at IS NULL)
		FROM currency_delisting_notices`, now).Scan(&unprepared, &broadcasts); err != nil {
		return fmt.Errorf("observe the notices: %w", err)
	}
	metrics.DelistingNoticesUnprepared.Set(float64(unprepared))
	metrics.DelistingNoticeBroadcasts.Set(float64(broadcasts))
	for _, holder := range []bool{true, false} {
		var pending int
		var oldest sql.NullTime
		if err := db.QueryRowContext(ctx, `SELECT count(*), min(n.due_at) FILTER (WHERE r.next_attempt_at <= $2)
			FROM currency_delisting_notice_recipients r
			JOIN currency_delisting_notices n ON n.delisting_hk = r.delisting_hk AND n.kind = r.kind
			WHERE r.done_at IS NULL AND r.holder = $1`, holder, now).Scan(&pending, &oldest); err != nil {
			return fmt.Errorf("observe the recipients of the notices: %w", err)
		}
		label := fmt.Sprint(holder)
		metrics.DelistingNoticeRecipientsPending.WithLabelValues(label).Set(float64(pending))
		age := 0.0
		if oldest.Valid {
			age = max(0, now.Sub(oldest.Time).Seconds())
		}
		metrics.DelistingNoticeOldestPending.WithLabelValues(label).Set(age)
	}
	return nil
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
	// PreparedAt is when the holders were written as recipients; nil until
	// then. PrepareAttempts counts the batches that failed meanwhile.
	PreparedAt      *time.Time `json:"prepared_at,omitempty"`
	PrepareAttempts int        `json:"prepare_attempts,omitempty"`
	// Broadcasting is a notice to everyone whose other users are still
	// being sent, in batches (#461).
	Broadcasting bool `json:"broadcasting,omitempty"`
	// Recipients are the users that it went to so far, and Holders those
	// of them who held the currency.
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
		SELECT n.kind, n.stage, n.due_at, n.prepared_at,
			count(r.user_hk) + n.broadcast_sent + n.broadcast_no_contact + n.broadcast_stale, COALESCE(n.holders, 0),
			count(*) FILTER (WHERE r.outcome = 'sent') + n.broadcast_sent,
			count(*) FILTER (WHERE r.outcome = 'no_contact') + n.broadcast_no_contact,
			count(*) FILTER (WHERE r.outcome = 'stale') + n.broadcast_stale, count(*) FILTER (WHERE r.outcome = 'failed'),
			count(*) FILTER (WHERE r.user_hk IS NOT NULL AND r.done_at IS NULL),
			n.broadcast AND n.broadcast_done_at IS NULL, n.prepare_attempts
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
			&r.Sent, &r.NoContact, &r.Stale, &r.Failed, &r.Pending, &r.Broadcasting, &r.PrepareAttempts); err != nil {
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
