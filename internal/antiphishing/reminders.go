package antiphishing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// RetryDelay is how long a reminder that could not be sent waits before it
// is tried again, so that one user's failure does not hold up the others.
const RetryDelay = time.Hour

// Reminders reminds the users whose phrases are due for a change (#375),
// by email, which shows the phrase like every email, and by SMS, which
// never does. Every instance may run it at once: each claims a due row
// with FOR UPDATE SKIP LOCKED, so that two never remind the same user. A
// reminder is sent at least once: one whose transaction fails to commit
// after the send goes out again at the next run.
type Reminders struct {
	Store  Store
	Sender notify.OTPSender
	// SMS reports that the SMS provider has a template for the reminders.
	// Without one, they go by email only.
	SMS bool
}

// Run reminds the users whose phrases are due, one at a time, until none
// is due or ctx ends, and returns how many it reminded. It stops at the
// first reminder that cannot be sent, which is tried again after
// RetryDelay.
func (r Reminders) Run(ctx context.Context) (int, error) {
	reminded := 0
	for ctx.Err() == nil {
		done, sent, err := r.next(ctx)
		if sent {
			reminded++
		}
		if err != nil || done {
			return reminded, err
		}
	}
	return reminded, ctx.Err()
}

// next handles the row that is due first. It reports done when no row is
// due, and sent when it sent a reminder.
func (r Reminders) next(ctx context.Context) (done, sent bool, err error) {
	tx, err := r.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback()

	var userHK string
	var sealed []byte
	var setAt, now time.Time
	var uses int64
	var deleted bool
	row := rowseal.AntiPhishingCodes.NewRow()
	err = tx.QueryRowContext(ctx, `SELECT c.user_hk::text, c.phrase_enc, c.set_at, c.shown_count, NOW(), u.deleted_at IS NOT NULL, `+
		rowseal.AntiPhishingCodes.Fields("c")+`
		FROM user_anti_phishing_codes c JOIN users u ON u.hk = c.user_hk
		WHERE c.removed_at IS NULL AND c.next_reminder_at <= NOW()
		ORDER BY c.next_reminder_at LIMIT 1 FOR UPDATE OF c SKIP LOCKED`).
		Scan(append([]any{&userHK, &sealed, &setAt, &uses, &now, &deleted}, row.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return true, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("read the anti-phishing codes that are due: %w", err)
	}

	rotation := r.Store.Rotation
	reason := rotation.Due(setAt, uses, now)
	switch {
	case deleted:
		// A deleted account is never reminded.
		return false, false, r.schedule(ctx, tx, userHK, nil, "infinity")
	case reason == "":
		// The bounds changed since the row was scheduled: it is due when
		// it comes of age, or when a use reaches MaxUses.
		next := "infinity"
		if rotation.MaxAge > 0 {
			next = setAt.Add(rotation.MaxAge).Format(time.RFC3339Nano)
		}
		return false, false, r.schedule(ctx, tx, userHK, nil, next)
	}
	nextReminder := now.Add(rotation.Interval).Format(time.RFC3339Nano)
	phrase, err := open(ctx, userHK, sealed, row)
	if errors.Is(err, rowseal.ErrUnverified) {
		// A phrase changed in SQL is not shown, nor reminded of; its
		// finding is recorded.
		return false, false, r.schedule(ctx, tx, userHK, nil, nextReminder)
	}
	if err != nil {
		return false, false, err
	}
	contacts, err := verifiedContacts(ctx, tx, userHK)
	if err != nil {
		return false, false, err
	}
	msg := notify.OTPMessage{Purpose: notify.PurposeAntiPhishingReminder, Uses: uses,
		AgeDays: int(now.Sub(setAt) / (24 * time.Hour))}
	for _, c := range contacts {
		if !r.Sender.Supports(c.typ) || (c.typ == notify.ContactMobile && !r.SMS) {
			continue
		}
		m := msg
		m.ContactType, m.Contact, m.MaskedContact = c.typ, c.value, notify.MaskContact(c.typ, c.value)
		if c.typ == notify.ContactEmail {
			m.AntiPhishingCode = phrase
		}
		if err := r.Sender.SendOTP(ctx, m); err != nil {
			return false, false, r.retryLater(ctx, tx, userHK, now,
				fmt.Errorf("send the reminder to change the anti-phishing code to %s: %w", m.MaskedContact, err))
		}
		sent = true
	}
	// A user without a contact that the reminder can reach is looked at
	// again after the interval, not reminded.
	var remindedAt *time.Time
	if sent {
		remindedAt = &now
	}
	return false, sent, r.schedule(ctx, tx, userHK, remindedAt, nextReminder)
}

// retryLater rolls tx back, which releases the row, and moves its reminder
// RetryDelay on, so that the next run starts with the other users. A
// contact that already has the reminder gets it again then. It returns
// sendErr.
func (r Reminders) retryLater(ctx context.Context, tx *sql.Tx, userHK string, now time.Time, sendErr error) error {
	_ = tx.Rollback()
	// The send may have failed because ctx ended: the delay is still
	// recorded, within a bound of its own.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := r.Store.DB.ExecContext(ctx, `UPDATE user_anti_phishing_codes SET next_reminder_at = $2
		WHERE user_hk = $1 AND removed_at IS NULL`, userHK, now.Add(RetryDelay)); err != nil {
		return errors.Join(sendErr, fmt.Errorf("delay the reminder: %w", err))
	}
	return sendErr
}

// schedule sets when the row of userHK is next looked at, and when it was
// reminded if remindedAt is not nil, and commits tx.
func (r Reminders) schedule(ctx context.Context, tx *sql.Tx, userHK string, remindedAt *time.Time, next string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE user_anti_phishing_codes SET next_reminder_at = $2::timestamptz,
		reminded_at = COALESCE($3, reminded_at) WHERE user_hk = $1`, userHK, next, remindedAt); err != nil {
		return fmt.Errorf("schedule the next reminder: %w", err)
	}
	return tx.Commit()
}

// contact is a verified contact of a user.
type contact struct{ typ, value string }

// verifiedContacts returns the primary verified email and mobile contacts
// of a user. A contact that does not match its seal, such as an address
// swapped in SQL for an attacker's, is left out (#139).
func verifiedContacts(ctx context.Context, q queryer, userHK string) ([]contact, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, type::text, value, value_enc, `+rowseal.UserContacts.Fields("c")+`
		FROM user_contacts c WHERE user_hk = $1 AND is_primary AND verified_at IS NOT NULL AND removed_at IS NULL
			AND type IN ('email', 'mobile') ORDER BY type`, userHK)
	if err != nil {
		return nil, fmt.Errorf("read the contacts: %w", err)
	}
	type read struct {
		id    int64
		typ   string
		plain *string
		enc   []byte
		row   *rowseal.Row
	}
	var all []read
	for rows.Next() {
		r := read{row: rowseal.UserContacts.NewRow()}
		if err := rows.Scan(append([]any{&r.id, &r.typ, &r.plain, &r.enc}, r.row.Dest()...)...); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read a contact: %w", err)
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the contacts: %w", err)
	}
	var out []contact
	for _, r := range all {
		if err := rowseal.Check(ctx, rowseal.UserContacts, r.row); err != nil {
			if errors.Is(err, rowseal.ErrUnverified) {
				continue
			}
			return nil, err
		}
		value, err := db.OpenContact(r.id, r.plain, r.enc)
		if err != nil {
			return nil, err
		}
		out = append(out, contact{typ: r.typ, value: value})
	}
	return out, nil
}

// queryer is *sql.DB or *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
