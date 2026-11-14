package antiphishing

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const day = 24 * time.Hour

func TestNormalize(t *testing.T) {
	zwnj := string(rune(0x200c))
	for in, want := range map[string]string{
		"blue falcon":           "blue falcon",
		"  blue   falcon 7  ":   "blue falcon 7",
		"sky-42":                "sky-42",
		"گل" + zwnj + "های سرخ": "گل" + zwnj + "های سرخ",
		"۱۲۳۴":                  "۱۲۳۴",
		"tab\there":             "tab here",
	} {
		got, err := Normalize(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got)
	}
	for _, in := range []string{"abc", "a b", "twenty-one characters", "blue<b>", "http://x.io", "a_b_c", "😀😀😀😀"} {
		_, err := Normalize(in)
		assert.ErrorIs(t, err, ErrInvalid, in)
	}
}

func TestHint(t *testing.T) {
	assert.Equal(t, "bl***", Hint("blue falcon"))
	assert.Equal(t, "گل***", Hint("گلهای سرخ"), "runes, not bytes")
}

func TestRotation_Due(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	r := Rotation{MaxAge: 180 * day, MaxUses: 200, Interval: 30 * day}
	assert.Equal(t, "", r.Due(now.Add(-179*day), 199, now))
	assert.Equal(t, ReasonAge, r.Due(now.Add(-180*day), 0, now))
	assert.Equal(t, ReasonUses, r.Due(now, 200, now))
	assert.Equal(t, "", Rotation{}.Due(now.Add(-1000*day), 1e6, now), "the zero rotation never suggests a change")
	assert.Equal(t, ReasonUses, Rotation{MaxUses: 5}.Due(now.Add(-1000*day), 5, now), "age off")
}

// fixture is a store on a migrated database, with sealed rows and data
// keys.
type fixture struct {
	Store
	t *testing.T
}

func newFixture(t *testing.T, rotation Rotation) fixture {
	t.Helper()
	conn := pgtest.Migrated(t, "testdb_antiphishing")
	sealtest.Use(t, conn, rowseal.ModeEnforce)
	return fixture{Store: Store{DB: conn, Rotation: rotation}, t: t}
}

// user creates a user with verified contacts, sealed.
func (f fixture) user(u usertest.User) string {
	f.t.Helper()
	hk := usertest.Insert(f.t, f.DB, u)
	ctx := context.Background()
	require.NoError(f.t, rowseal.Reseal(ctx, f.DB, rowseal.Users, "hk = $1", hk))
	require.NoError(f.t, rowseal.Reseal(ctx, f.DB, rowseal.UserContacts, "user_hk = $1", hk))
	return hk
}

// age makes the code of userHK days old and due for a look, as time would,
// and seals it again.
func (f fixture) age(userHK string, days int) {
	f.t.Helper()
	_, err := f.DB.Exec(`UPDATE user_anti_phishing_codes SET set_at = NOW() - make_interval(days => $2), next_reminder_at = NOW() - INTERVAL '1 minute'
		WHERE user_hk = $1`, userHK, days)
	require.NoError(f.t, err)
	require.NoError(f.t, rowseal.Reseal(context.Background(), f.DB, rowseal.AntiPhishingCodes, "user_hk = $1", userHK))
}

func (f fixture) column(userHK, column string) sql.NullTime {
	f.t.Helper()
	var at sql.NullTime
	require.NoError(f.t, f.DB.QueryRow(`SELECT CASE WHEN `+column+` = 'infinity' THEN NULL ELSE `+column+` END FROM user_anti_phishing_codes WHERE user_hk = $1`, userHK).Scan(&at))
	return at
}

func (f fixture) nextReminderIsNever(userHK string) bool {
	f.t.Helper()
	var never bool
	require.NoError(f.t, f.DB.QueryRow(`SELECT next_reminder_at = 'infinity' FROM user_anti_phishing_codes WHERE user_hk = $1`, userHK).Scan(&never))
	return never
}

func TestStore_ShowsTheCodeOnlyToVerifiedEmails(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Rotation{MaxAge: 180 * day, MaxUses: 200, Interval: 30 * day})
	alice := f.user(usertest.User{Email: "alice@example.com", Mobile: "09121234567"})
	origin := audit.Event{IP: "203.0.113.7"}

	st, err := f.Status(ctx, alice)
	require.NoError(t, err)
	assert.Equal(t, Status{}, st, "no code yet")
	phrase, err := f.ShowTo(ctx, alice, notify.ContactEmail, "alice@example.com")
	require.NoError(t, err)
	assert.Empty(t, phrase)

	assert.ErrorIs(t, f.Set(ctx, alice, "a<b>c", origin), ErrInvalid)
	require.NoError(t, f.Set(ctx, alice, "  blue   falcon ", origin))
	assert.ErrorIs(t, f.Set(ctx, alice, "blue falcon", origin), ErrSame, "the same code would only reset the reminders")

	// Only the verified email shows it, whatever its case; each email counts.
	phrase, err = f.ShowTo(ctx, alice, notify.ContactEmail, "Alice@Example.com")
	require.NoError(t, err)
	assert.Equal(t, "blue falcon", phrase)
	for _, other := range []struct{ typ, address string }{
		{notify.ContactMobile, "09121234567"},
		{notify.ContactEmail, "attacker@example.com"},
	} {
		phrase, err = f.ShowTo(ctx, alice, other.typ, other.address)
		require.NoError(t, err)
		assert.Empty(t, phrase, "%s %s", other.typ, other.address)
	}
	st, err = f.Status(ctx, alice)
	require.NoError(t, err)
	assert.True(t, st.Set)
	assert.Equal(t, "bl***", st.Hint, "the API shows only the start of the code")
	assert.Equal(t, int64(1), st.ShownCount)
	assert.False(t, st.ChangeSuggested)
	require.NotNil(t, st.SetAt)
	assert.WithinDuration(t, time.Now(), *st.SetAt, time.Minute)

	// Stored encrypted only, and audited without it.
	var sealed []byte
	require.NoError(t, f.DB.QueryRow(`SELECT phrase_enc FROM user_anti_phishing_codes WHERE user_hk = $1`, alice).Scan(&sealed))
	assert.NotContains(t, string(sealed), "falcon")
	var events int
	require.NoError(t, f.DB.QueryRow(`SELECT count(*) FROM audit_events WHERE target_id = $1 AND action = $2 AND ip = $3`,
		alice, audit.ActionUserAntiPhishingCodeSet, origin.IP).Scan(&events))
	assert.Equal(t, 1, events)
	var leaked int
	require.NoError(t, f.DB.QueryRow(`SELECT count(*) FROM audit_events WHERE row_to_json(audit_events)::text LIKE '%falcon%'`).Scan(&leaked))
	assert.Zero(t, leaked, "the audit never holds the code")

	// A new code starts again.
	require.NoError(t, f.Set(ctx, alice, "red-river-9", origin))
	st, err = f.Status(ctx, alice)
	require.NoError(t, err)
	assert.Equal(t, "re***", st.Hint)
	assert.Zero(t, st.ShownCount)

	require.NoError(t, f.Remove(ctx, alice, origin))
	assert.ErrorIs(t, f.Remove(ctx, alice, origin), ErrNotSet)
	phrase, err = f.ShowTo(ctx, alice, notify.ContactEmail, "alice@example.com")
	require.NoError(t, err)
	assert.Empty(t, phrase, "a removed code is not shown")
	st, err = f.Status(ctx, alice)
	require.NoError(t, err)
	assert.False(t, st.Set)
}

// TestStore_ACodeChangedInSQLIsNotShown checks the seal (#139): a code
// copied from another user, or an age moved to stop the reminders, is not
// shown, and a contact swapped in SQL gets no code.
func TestStore_ACodeChangedInSQLIsNotShown(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Rotation{MaxAge: 180 * day, Interval: 30 * day})
	alice := f.user(usertest.User{Email: "alice@example.com"})
	mallory := f.user(usertest.User{Email: "mallory@example.com"})
	require.NoError(t, f.Set(ctx, alice, "blue falcon", audit.Event{}))
	require.NoError(t, f.Set(ctx, mallory, "mallory code", audit.Event{}))

	_, err := f.DB.Exec(`UPDATE user_anti_phishing_codes SET phrase_enc = (SELECT phrase_enc FROM user_anti_phishing_codes WHERE user_hk = $2)
		WHERE user_hk = $1`, alice, mallory)
	require.NoError(t, err)
	phrase, err := f.ShowTo(ctx, alice, notify.ContactEmail, "alice@example.com")
	require.NoError(t, err)
	assert.Empty(t, phrase, "a code copied in SQL is not shown")

	require.NoError(t, f.Set(ctx, alice, "blue falcon", audit.Event{}), "a new code replaces the one that fails its seal")
	_, err = f.DB.Exec(`UPDATE user_anti_phishing_codes SET set_at = NOW() + INTERVAL '10 years' WHERE user_hk = $1`, alice)
	require.NoError(t, err)
	st, err := f.Status(ctx, alice)
	require.NoError(t, err)
	assert.False(t, st.Set, "an age moved in SQL fails the seal")

	require.NoError(t, f.Set(ctx, mallory, "mallory two", audit.Event{}))
	_, err = f.DB.Exec(`UPDATE user_contacts SET value = 'evil@example.com', value_enc = NULL, value_bidx = NULL WHERE user_hk = $1`, mallory)
	require.NoError(t, err)
	phrase, err = f.ShowTo(ctx, mallory, notify.ContactEmail, "evil@example.com")
	require.NoError(t, err)
	assert.Empty(t, phrase, "a contact swapped in SQL gets no code")
}

func TestStore_UsesBringTheReminderForward(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Rotation{MaxAge: 180 * day, MaxUses: 3, Interval: 30 * day})
	alice := f.user(usertest.User{Email: "alice@example.com"})
	require.NoError(t, f.Set(ctx, alice, "blue falcon", audit.Event{}))
	next := f.column(alice, "next_reminder_at")
	require.True(t, next.Valid)
	assert.WithinDuration(t, time.Now().Add(180*day), next.Time, time.Minute, "the first reminder is when the code comes of age")

	for range 2 {
		_, err := f.ShowTo(ctx, alice, notify.ContactEmail, "alice@example.com")
		require.NoError(t, err)
	}
	assert.Equal(t, next.Time, f.column(alice, "next_reminder_at").Time)
	_, err := f.ShowTo(ctx, alice, notify.ContactEmail, "alice@example.com")
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), f.column(alice, "next_reminder_at").Time, time.Minute, "the third use makes it due")
	st, err := f.Status(ctx, alice)
	require.NoError(t, err)
	assert.True(t, st.ChangeSuggested)
	assert.Equal(t, ReasonUses, st.ChangeReason)

	// After a reminder, more uses wait for the interval.
	_, err = f.DB.Exec(`UPDATE user_anti_phishing_codes SET reminded_at = NOW(), next_reminder_at = NOW() + INTERVAL '30 days' WHERE user_hk = $1`, alice)
	require.NoError(t, err)
	_, err = f.ShowTo(ctx, alice, notify.ContactEmail, "alice@example.com")
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(30*day), f.column(alice, "next_reminder_at").Time, time.Minute)

	// Without an age bound, a new code waits for its uses.
	f.Rotation.MaxAge = 0
	require.NoError(t, f.Set(ctx, alice, "red river", audit.Event{}))
	assert.True(t, f.nextReminderIsNever(alice))
}

// recorder keeps the messages it is asked to send, and fails for the
// addresses in fail.
type recorder struct {
	mu   sync.Mutex
	sent []notify.OTPMessage
	fail map[string]bool
}

func (r *recorder) Supports(contactType string) bool {
	return contactType == notify.ContactEmail || contactType == notify.ContactMobile
}

func (r *recorder) SendOTP(_ context.Context, msg notify.OTPMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail[msg.Contact] {
		return errors.New("the broker is away")
	}
	r.sent = append(r.sent, msg)
	return nil
}

func (r *recorder) to(contact string) []notify.OTPMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []notify.OTPMessage
	for _, m := range r.sent {
		if m.Contact == contact {
			out = append(out, m)
		}
	}
	return out
}

func TestReminders_RemindTheUsersWhoseCodesAreDue(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Rotation{MaxAge: 180 * day, MaxUses: 2, Interval: 30 * day})
	sender := &recorder{}
	reminders := Reminders{Store: f.Store, Sender: sender, SMS: true}

	alice := f.user(usertest.User{Email: "alice@example.com", Mobile: "09121234567"})
	require.NoError(t, f.Set(ctx, alice, "blue falcon", audit.Event{}))
	f.age(alice, 200)
	bob := f.user(usertest.User{Email: "bob@example.com"})
	require.NoError(t, f.Set(ctx, bob, "bob code", audit.Event{}))
	carol := f.user(usertest.User{Email: "carol@example.com"})
	require.NoError(t, f.Set(ctx, carol, "carol code", audit.Event{}))
	for range 2 {
		_, err := f.ShowTo(ctx, carol, notify.ContactEmail, "carol@example.com")
		require.NoError(t, err)
	}
	gone := f.user(usertest.User{Email: "gone@example.com"})
	require.NoError(t, f.Set(ctx, gone, "gone code", audit.Event{}))
	f.age(gone, 200)
	_, err := f.DB.Exec(`UPDATE users SET deleted_at = NOW() WHERE hk = $1`, gone)
	require.NoError(t, err)

	n, err := reminders.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "alice for age, carol for uses")

	email := sender.to("alice@example.com")
	require.Len(t, email, 1)
	assert.Equal(t, notify.PurposeAntiPhishingReminder, email[0].Purpose)
	assert.Equal(t, "blue falcon", email[0].AntiPhishingCode, "the email shows the code, like every email")
	assert.Equal(t, 200, email[0].AgeDays)
	assert.Empty(t, email[0].Code)
	assert.Equal(t, "ali*****@example.com", email[0].MaskedContact)
	sms := sender.to("09121234567")
	require.Len(t, sms, 1)
	assert.Empty(t, sms[0].AntiPhishingCode, "an SMS never carries the code")
	carolEmail := sender.to("carol@example.com")
	require.Len(t, carolEmail, 1)
	assert.Equal(t, int64(2), carolEmail[0].Uses)
	assert.Empty(t, sender.to("bob@example.com"), "bob's code is new and little used")
	assert.Empty(t, sender.to("gone@example.com"), "a deleted account is never reminded")
	assert.True(t, f.nextReminderIsNever(gone))

	reminded := f.column(alice, "reminded_at")
	require.True(t, reminded.Valid)
	assert.WithinDuration(t, time.Now().Add(30*day), f.column(alice, "next_reminder_at").Time, time.Minute)

	n, err = reminders.Run(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "the next reminder waits for the interval")

	// A new code ends the reminders.
	require.NoError(t, f.Set(ctx, carol, "carol new code", audit.Event{}))
	st, err := f.Status(ctx, carol)
	require.NoError(t, err)
	assert.False(t, st.ChangeSuggested)
}

func TestReminders_WithoutSMSTemplateGoByEmailOnly(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Rotation{MaxAge: 180 * day, Interval: 30 * day})
	sender := &recorder{}
	alice := f.user(usertest.User{Email: "alice@example.com", Mobile: "09121234567"})
	require.NoError(t, f.Set(ctx, alice, "blue falcon", audit.Event{}))
	f.age(alice, 200)
	n, err := Reminders{Store: f.Store, Sender: sender}.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Len(t, sender.to("alice@example.com"), 1)
	assert.Empty(t, sender.to("09121234567"))
}

// TestReminders_ARowNoLongerDueIsRescheduled checks a row that a looser
// configuration no longer makes due: it waits until the code comes of age.
func TestReminders_ARowNoLongerDueIsRescheduled(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Rotation{MaxAge: 180 * day, Interval: 30 * day})
	alice := f.user(usertest.User{Email: "alice@example.com"})
	require.NoError(t, f.Set(ctx, alice, "blue falcon", audit.Event{}))
	f.age(alice, 100)
	sender := &recorder{}
	n, err := Reminders{Store: f.Store, Sender: sender}.Run(ctx)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.WithinDuration(t, time.Now().Add(80*day), f.column(alice, "next_reminder_at").Time, time.Minute)
	assert.False(t, f.column(alice, "reminded_at").Valid)
}

// TestReminders_AFailedSendIsRetriedLater checks that a reminder that
// cannot be sent waits RetryDelay, so that it does not hold up the others.
func TestReminders_AFailedSendIsRetriedLater(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Rotation{MaxAge: 180 * day, Interval: 30 * day})
	alice := f.user(usertest.User{Email: "alice@example.com"})
	require.NoError(t, f.Set(ctx, alice, "blue falcon", audit.Event{}))
	f.age(alice, 200)
	sender := &recorder{fail: map[string]bool{"alice@example.com": true}}
	n, err := Reminders{Store: f.Store, Sender: sender}.Run(ctx)
	require.Error(t, err)
	assert.Zero(t, n)
	assert.WithinDuration(t, time.Now().Add(RetryDelay), f.column(alice, "next_reminder_at").Time, time.Minute)
	assert.False(t, f.column(alice, "reminded_at").Valid)
}

// TestReminders_InstancesRemindEachUserOnce runs the job on several
// instances at once (multi-node): each due user gets one reminder.
func TestReminders_InstancesRemindEachUserOnce(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Rotation{MaxAge: 180 * day, Interval: 30 * day})
	const users = 12
	for i := range users {
		email := string(rune('a'+i)) + "user@example.com"
		hk := f.user(usertest.User{Email: email})
		require.NoError(t, f.Set(ctx, hk, "code number "+string(rune('a'+i)), audit.Event{}))
		f.age(hk, 200)
	}
	sender := &recorder{}
	var wg sync.WaitGroup
	counts := make([]int, 4)
	for i := range counts {
		wg.Go(func() {
			n, err := Reminders{Store: f.Store, Sender: sender}.Run(ctx)
			assert.NoError(t, err)
			counts[i] = n
		})
	}
	wg.Wait()
	total := 0
	for _, n := range counts {
		total += n
	}
	assert.Equal(t, users, total)
	assert.Len(t, sender.sent, users, "no user is reminded twice")
}
