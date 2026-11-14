// Package antiphishing keeps the anti-phishing code of each user (#375): a
// phrase of the user's own that every email from the exchange to their
// verified contacts shows. An email without it, or with another, is not
// from the exchange.
//
// The phrase goes only to contacts that the account has verified, never to
// a contact that a change is adding: a hijacked session that adds its own
// address would otherwise learn the phrase and copy it into its phishing.
// It is stored encrypted only (#140), bound to its user, and the row is
// sealed (#139): a phrase changed or swapped in SQL is not shown.
//
// Each email that shows a phrase counts a use. Once a phrase is old, or
// many emails have shown it, the exchange suggests a new one: the status
// says so, and Reminders sends a reminder by email and SMS, again every
// interval until the phrase changes.
package antiphishing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// The bounds of a phrase, in characters.
const (
	MinLength = 4
	MaxLength = 20
)

// Errors of the store.
var (
	// ErrInvalid is a phrase that is not 4 to 20 letters, digits, spaces,
	// or hyphens.
	ErrInvalid = errors.New("an anti-phishing code is 4 to 20 letters, digits, spaces, or hyphens")
	// ErrSame is a new phrase that is the current one: a change that
	// changes nothing would reset the reminders.
	ErrSame = errors.New("the new anti-phishing code is the current one")
	// ErrNoKeys is a store without data keys, which cannot encrypt the
	// phrase.
	ErrNoKeys = errors.New("the data-protection keys are not loaded")
	// ErrNotSet is a removal of a phrase the user does not have.
	ErrNotSet = errors.New("the user has no anti-phishing code")
)

// Normalize checks a phrase and returns it with its surrounding spaces
// trimmed and its inner runs of spaces made one. Letters and digits of any
// script are allowed, Persian among them, with spaces and hyphens between.
func Normalize(phrase string) (string, error) {
	phrase = strings.Join(strings.Fields(phrase), " ")
	n := utf8.RuneCountInString(phrase)
	if n < MinLength || n > MaxLength {
		return "", ErrInvalid
	}
	for _, r := range phrase {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != ' ' && r != '-' && r != 0x200c {
			return "", ErrInvalid
		}
	}
	return phrase, nil
}

// Hint is the start of a phrase, for the settings of the account: enough
// for the user to recognize it, not enough for a stolen session to copy it
// into a phishing email. Every email shows the whole phrase.
func Hint(phrase string) string {
	runes := []rune(phrase)
	return string(runes[:min(2, len(runes))]) + "***"
}

// The reasons a phrase is due for a change.
const (
	// ReasonAge is a phrase that is MaxAge old.
	ReasonAge = "age"
	// ReasonUses is a phrase that MaxUses emails have shown.
	ReasonUses = "uses"
)

// Rotation says when a phrase is due for a change: once it is MaxAge old,
// or once MaxUses emails have shown it. A zero bound is off, and the zero
// Rotation never suggests a change. Interval is the time between two
// reminders to one user.
type Rotation struct {
	MaxAge   time.Duration
	MaxUses  int64
	Interval time.Duration
}

// NewRotation returns the rotation that cfg configures.
func NewRotation(cfg config.AntiPhishingRotation) Rotation {
	const day = 24 * time.Hour
	return Rotation{
		MaxAge:   time.Duration(max(cfg.MaxAgeDays, 0)) * day,
		MaxUses:  int64(max(cfg.MaxUses, 0)),
		Interval: time.Duration(max(cfg.ReminderIntervalDays, 1)) * day,
	}
}

// Due returns why a phrase set at setAt, and shown in uses emails, is due
// for a change at now: ReasonAge, ReasonUses, or "" when it is not.
func (r Rotation) Due(setAt time.Time, uses int64, now time.Time) string {
	switch {
	case r.MaxAge > 0 && !now.Before(setAt.Add(r.MaxAge)):
		return ReasonAge
	case r.MaxUses > 0 && uses >= r.MaxUses:
		return ReasonUses
	}
	return ""
}

// seconds is d in whole seconds, for SQL intervals.
func seconds(d time.Duration) int64 { return int64(d / time.Second) }

// place is where the phrase of a user is sealed: its associated data.
func place(userHK string) field.Place {
	return field.Place{Table: "user_anti_phishing_codes", Column: "phrase_enc", Row: userHK}
}

// open checks the seal of the row of userHK and opens its phrase. A row
// that does not match its seal returns rowseal.ErrUnverified in enforce
// mode, and its finding is recorded.
func open(ctx context.Context, userHK string, sealed []byte, row *rowseal.Row) (string, error) {
	if err := rowseal.Check(ctx, rowseal.AntiPhishingCodes, row); err != nil {
		return "", err
	}
	cipher := field.Active()
	if cipher == nil {
		return "", ErrNoKeys
	}
	phrase, err := cipher.Open(place(userHK), sealed)
	if err != nil {
		return "", fmt.Errorf("open the anti-phishing code: %w", err)
	}
	return phrase, nil
}

// Store keeps the phrases.
type Store struct {
	DB *sql.DB
	// Rotation says when a phrase is due for a change.
	Rotation Rotation
}

// Status is the anti-phishing code of a user, as their settings show it.
type Status struct {
	// Set is whether the user has a code.
	Set bool `json:"set"`
	// Hint is the start of the code; the whole code is in every email.
	Hint string `json:"hint,omitempty" example:"bl***"`
	// SetAt is when the code was set.
	SetAt *time.Time `json:"set_at,omitempty"`
	// ShownCount is the number of emails that have shown the code.
	ShownCount int64 `json:"shown_count"`
	// ChangeSuggested is whether the exchange suggests a new code, for
	// ChangeReason: "age", the code is old, or "uses", many emails have
	// shown it.
	ChangeSuggested bool   `json:"change_suggested"`
	ChangeReason    string `json:"change_reason,omitempty" enums:"age,uses"`
}

// Status returns the user's code as their settings show it. A code that
// does not match its seal is shown as none in enforce mode.
func (s Store) Status(ctx context.Context, userHK string) (Status, error) {
	var sealed []byte
	var setAt, now time.Time
	var uses int64
	row := rowseal.AntiPhishingCodes.NewRow()
	err := s.DB.QueryRowContext(ctx, `SELECT phrase_enc, set_at, shown_count, NOW(), `+rowseal.AntiPhishingCodes.Fields("")+`
		FROM user_anti_phishing_codes WHERE user_hk = $1 AND removed_at IS NULL`, userHK).
		Scan(append([]any{&sealed, &setAt, &uses, &now}, row.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, fmt.Errorf("read the anti-phishing code: %w", err)
	}
	phrase, err := open(ctx, userHK, sealed, row)
	if errors.Is(err, rowseal.ErrUnverified) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, err
	}
	reason := s.Rotation.Due(setAt, uses, now)
	setAt = setAt.UTC()
	return Status{Set: true, Hint: Hint(phrase), SetAt: &setAt, ShownCount: uses,
		ChangeSuggested: reason != "", ChangeReason: reason}, nil
}

// Phrase returns the user's phrase, or "" when they have none, without
// counting a use. A phrase that does not match its seal is not returned in
// enforce mode, and its finding is recorded; one that cannot be read
// without the data keys is not returned either.
func (s Store) Phrase(ctx context.Context, userHK string) (string, error) {
	var sealed []byte
	row := rowseal.AntiPhishingCodes.NewRow()
	err := s.DB.QueryRowContext(ctx, `SELECT phrase_enc, `+rowseal.AntiPhishingCodes.Fields("")+`
		FROM user_anti_phishing_codes WHERE user_hk = $1 AND removed_at IS NULL`, userHK).
		Scan(append([]any{&sealed}, row.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the anti-phishing code: %w", err)
	}
	phrase, err := open(ctx, userHK, sealed, row)
	if errors.Is(err, rowseal.ErrUnverified) {
		return "", nil
	}
	return phrase, err
}

// Show returns the user's phrase for an email that is to show it, and
// counts the use, or "" when they have none. The use that reaches MaxUses
// brings the next reminder forward to now, or to the end of the interval
// after the last one.
func (s Store) Show(ctx context.Context, userHK string) (string, error) {
	var sealed []byte
	row := rowseal.AntiPhishingCodes.NewRow()
	err := s.DB.QueryRowContext(ctx, `UPDATE user_anti_phishing_codes SET shown_count = shown_count + 1,
			next_reminder_at = CASE WHEN $2::bigint > 0 AND shown_count + 1 >= $2::bigint
				THEN LEAST(next_reminder_at, GREATEST(NOW(), reminded_at + $3::bigint * INTERVAL '1 second'))
				ELSE next_reminder_at END
		WHERE user_hk = $1 AND removed_at IS NULL
		RETURNING phrase_enc, `+rowseal.AntiPhishingCodes.Fields(""), userHK, s.Rotation.MaxUses, seconds(s.Rotation.Interval)).
		Scan(append([]any{&sealed}, row.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("count a use of the anti-phishing code: %w", err)
	}
	phrase, err := open(ctx, userHK, sealed, row)
	if errors.Is(err, rowseal.ErrUnverified) {
		return "", nil
	}
	return phrase, err
}

// Set sets the user's phrase, and records origin, the audit event of the
// request, without the phrase. A new phrase starts with no uses, and its
// first reminder is when it comes of age.
func (s Store) Set(ctx context.Context, userHK, phrase string, origin audit.Event) error {
	phrase, err := Normalize(phrase)
	if err != nil {
		return err
	}
	cipher := field.Active()
	if cipher == nil {
		return ErrNoKeys
	}
	current, err := s.Phrase(ctx, userHK)
	if err != nil {
		return err
	}
	if current == phrase {
		return ErrSame
	}
	sealed, err := cipher.Seal(place(userHK), phrase)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO user_anti_phishing_codes (user_hk, phrase_enc, set_at, next_reminder_at)
		VALUES ($1, $2, NOW(), CASE WHEN $3::bigint > 0 THEN NOW() + $3::bigint * INTERVAL '1 second' ELSE 'infinity' END)
		ON CONFLICT (user_hk) DO UPDATE SET phrase_enc = EXCLUDED.phrase_enc, set_at = EXCLUDED.set_at, shown_count = 0,
			reminded_at = NULL, next_reminder_at = EXCLUDED.next_reminder_at, removed_at = NULL, updated_at = NOW()`,
		userHK, sealed, seconds(s.Rotation.MaxAge)); err != nil {
		return fmt.Errorf("record the anti-phishing code: %w", err)
	}
	return s.finish(ctx, tx, userHK, audit.ActionUserAntiPhishingCodeSet, origin)
}

// Remove removes the user's phrase, and records origin without it. Emails
// then show none, and no reminder follows.
func (s Store) Remove(ctx context.Context, userHK string, origin audit.Event) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE user_anti_phishing_codes SET phrase_enc = NULL, removed_at = NOW(), updated_at = NOW()
		WHERE user_hk = $1 AND removed_at IS NULL`, userHK)
	if err != nil {
		return fmt.Errorf("remove the anti-phishing code: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return ErrNotSet
	}
	return s.finish(ctx, tx, userHK, audit.ActionUserAntiPhishingCodeRemove, origin)
}

// finish seals the user's row in tx, records the event, and commits.
func (s Store) finish(ctx context.Context, tx *sql.Tx, userHK, action string, origin audit.Event) error {
	if err := rowseal.Reseal(ctx, tx, rowseal.AntiPhishingCodes, "user_hk = $1", userHK); err != nil {
		return err
	}
	event := origin
	event.ActorType, event.ActorID = audit.ActorUser, userHK
	event.Action, event.TargetType, event.TargetID = action, audit.TargetUser, userHK
	event.Outcome = audit.Success
	if err := audit.Record(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit()
}

// ShowTo returns the user's phrase for an email to address, and counts the
// use, when address is a verified primary email of the user that matches
// its seal. For any other address, such as one that a change is adding,
// or for a user without a phrase, it returns "".
func (s Store) ShowTo(ctx context.Context, userHK, contactType, address string) (string, error) {
	if contactType != notify.ContactEmail || userHK == "" {
		return "", nil
	}
	contacts, err := verifiedContacts(ctx, s.DB, userHK)
	if err != nil {
		return "", err
	}
	for _, c := range contacts {
		if c.typ == contactType && db.FoldContact(c.value) == db.FoldContact(address) {
			return s.Show(ctx, userHK)
		}
	}
	return "", nil
}

// Reader reads phrases for the senders of emails.
type Reader interface {
	// ShowTo returns the user's phrase for an email to address, and
	// counts the use, only when address is a verified email of the user.
	ShowTo(ctx context.Context, userHK, contactType, address string) (string, error)
}

var _ Reader = Store{}
