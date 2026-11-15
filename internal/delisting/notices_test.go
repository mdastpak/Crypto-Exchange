package delisting_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/delisting"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recorder is a sender that records each message, and fails those that
// fail reports.
type recorder struct {
	mu   sync.Mutex
	sent []notify.OTPMessage
	fail func(notify.OTPMessage) bool
}

func (r *recorder) Supports(contactType string) bool {
	return contactType == notify.ContactEmail || contactType == notify.ContactMobile
}

func (r *recorder) SendOTP(_ context.Context, msg notify.OTPMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil && r.fail(msg) {
		return errors.New("the broker is away")
	}
	r.sent = append(r.sent, msg)
	return nil
}

// to returns the messages to contact.
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

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = nil
}

func accountOf(t *testing.T, userHK string) ledger.Account {
	t.Helper()
	hk, err := shared.UUIDFromString(userHK)
	require.NoError(t, err)
	return ledger.Account{UserHK: hk}
}

// fixedPhrase shows every user the same anti-phishing code.
type fixedPhrase string

func (p fixedPhrase) Show(context.Context, string) (string, error) { return string(p), nil }

// noticeWorld is a currency being delisted, with a holder, a user who
// holds nothing, one without contacts, and a deleted account.
type noticeWorld struct {
	db       *sql.DB
	currency string
	start    time.Time
	d        delisting.Delisting
	sender   *recorder
	n        delisting.Notifier
}

func newNoticeWorld(t *testing.T, p func(start time.Time) delisting.Plan) noticeWorld {
	t.Helper()
	db := pgtest.Migrated(t, testDBName)
	b := books{t: t, db: db}
	xyz := insertCurrency(t, db, "XYZ", "approved")
	alice := usertest.Insert(t, db, usertest.User{Email: "alice@example.com", Mobile: "+989121111111"})
	usertest.Insert(t, db, usertest.User{Email: "bob@example.com", Mobile: "+989122222222"})
	usertest.Insert(t, db, usertest.User{})
	usertest.Insert(t, db, usertest.User{Email: "gone@example.com", Deleted: true})
	b.fund(accountOf(t, alice), xyz, "25")
	start := time.Now().Truncate(time.Second)
	d, _, err := delisting.Schedule(ctx, db, xyz, p(start), origin())
	require.NoError(t, err)
	sender := &recorder{}
	return noticeWorld{db: db, currency: xyz, start: start, d: d, sender: sender,
		n: delisting.Notifier{Sender: sender, SMS: true, Phrases: fixedPhrase("blue-falcon")}}
}

// at applies the steps that are due at now and sends the notices.
func (w noticeWorld) at(t *testing.T, now time.Time) {
	t.Helper()
	_, err := delisting.Advance(ctx, w.db, now, 100)
	require.NoError(t, err)
	_, err = w.n.Advance(ctx, w.db, now, 100)
	require.NoError(t, err)
}

func (w noticeWorld) notices(t *testing.T) map[string]delisting.NoticeReport {
	t.Helper()
	list, err := delisting.Notices(ctx, w.db, w.currency, w.d.ID)
	require.NoError(t, err)
	out := map[string]delisting.NoticeReport{}
	for _, n := range list {
		out[n.Kind] = n
	}
	return out
}

// tenDays is a standard delisting announced at start, with deposits
// closed a day later, frozen ten days after start, and delisted after.
func tenDays(start time.Time) delisting.Plan {
	p := plan(start, currencies.LifecycleDelistAnnounced)
	p.Steps = append(p.Steps,
		delisting.Step{State: currencies.LifecycleDepositsDisabled, At: start.Add(24 * time.Hour)},
		delisting.Step{State: currencies.LifecycleFrozen, At: start.Add(10 * 24 * time.Hour)},
		delisting.Step{State: currencies.LifecycleDelisted, At: start.Add(11 * 24 * time.Hour)})
	return p
}

func TestNotices_HoldersByEveryChannelTheOthersByEmail(t *testing.T) {
	w := newNoticeWorld(t, tenDays)
	w.at(t, w.start)

	alice := append(w.sender.to("alice@example.com"), w.sender.to("+989121111111")...)
	require.Len(t, alice, 2, "the holder by email and SMS")
	for _, m := range alice {
		assert.Equal(t, notify.PurposeDelistingNotice, m.Purpose)
		assert.Equal(t, notify.DelistingNotice{Currency: "XYZ", Stage: "delist_announced",
			Deadline: w.start.Add(10 * 24 * time.Hour).UTC().Format("2006-01-02 15:04 UTC"), DaysLeft: 10,
			Holder: true, Settlement: "claim", ClaimDays: 180}, m.Delisting)
	}
	assert.Equal(t, "blue-falcon", w.sender.to("alice@example.com")[0].AntiPhishingCode, "the email shows the code")
	assert.Empty(t, w.sender.to("+989121111111")[0].AntiPhishingCode, "an SMS never does")
	bob := w.sender.to("bob@example.com")
	require.Len(t, bob, 1, "another user by email")
	assert.False(t, bob[0].Delisting.Holder)
	assert.Empty(t, w.sender.to("+989122222222"), "and not by SMS, which costs")
	assert.Empty(t, w.sender.to("gone@example.com"), "never a deleted account")

	n := w.notices(t)["step:delist_announced"]
	assert.Equal(t, 3, n.Recipients)
	assert.Equal(t, 1, n.Holders)
	assert.Equal(t, 2, n.Sent)
	assert.Equal(t, 1, n.NoContact)
	assert.Zero(t, n.Pending)

	// Each run after tells nothing again.
	w.sender.reset()
	w.at(t, w.start.Add(time.Minute))
	assert.Empty(t, w.sender.sent)

	// The next step is told too.
	w.at(t, w.start.Add(24*time.Hour))
	assert.Len(t, w.sender.to("alice@example.com"), 1)
	assert.Equal(t, "deposits_disabled", w.sender.to("bob@example.com")[0].Delisting.Stage)
	assert.Equal(t, 9, w.sender.to("alice@example.com")[0].Delisting.DaysLeft)
}

func TestNotices_HoldersAreRemindedOfTheFreeze(t *testing.T) {
	w := newNoticeWorld(t, tenDays)
	w.at(t, w.start)
	w.at(t, w.start.Add(24*time.Hour))
	w.sender.reset()

	// Seven days before the freeze; the 30 and 14 day reminders came
	// before the delisting was scheduled.
	frozen := w.start.Add(10 * 24 * time.Hour)
	w.at(t, frozen.Add(-7*24*time.Hour))
	alice := append(w.sender.to("alice@example.com"), w.sender.to("+989121111111")...)
	require.Len(t, alice, 2)
	assert.Equal(t, notify.DelistingStageReminder, alice[0].Delisting.Stage)
	assert.Equal(t, 7, alice[0].Delisting.DaysLeft)
	assert.Empty(t, w.sender.to("bob@example.com"), "only holders are reminded")
	notices := w.notices(t)
	assert.Contains(t, notices, "reminder:7d")
	assert.NotContains(t, notices, "reminder:14d")
	assert.NotContains(t, notices, "reminder:30d")
	assert.Equal(t, 1, notices["reminder:7d"].Recipients)

	// At the freeze, nothing is left to do before it.
	w.sender.reset()
	w.at(t, frozen)
	frozenNotice := w.sender.to("alice@example.com")
	require.Len(t, frozenNotice, 1)
	assert.Equal(t, "frozen", frozenNotice[0].Delisting.Stage)
	assert.Empty(t, frozenNotice[0].Delisting.Deadline)
}

func TestNotices_ACancelIsToldAndStopsWhatWasLeft(t *testing.T) {
	w := newNoticeWorld(t, tenDays)
	w.at(t, w.start)
	w.sender.reset()

	// The step is applied and its notice written, but not sent, when the
	// delisting is cancelled.
	_, err := delisting.Advance(ctx, w.db, w.start.Add(24*time.Hour), 100)
	require.NoError(t, err)
	require.NoError(t, w.n.Plan(ctx, w.db, w.start.Add(24*time.Hour)))
	_, err = delisting.Prepare(ctx, w.db, w.start.Add(24*time.Hour))
	require.NoError(t, err)
	_, _, err = delisting.Cancel(ctx, w.db, w.currency, w.d.ID, nil, origin())
	require.NoError(t, err)

	w.at(t, w.start.Add(24*time.Hour+time.Minute))
	notices := w.notices(t)
	assert.Equal(t, 3, notices["step:deposits_disabled"].Stale, "the step is not told after the cancel")
	alice := append(w.sender.to("alice@example.com"), w.sender.to("+989121111111")...)
	require.Len(t, alice, 1, "the cancel by email only")
	assert.Equal(t, notify.DelistingStageCancelled, alice[0].Delisting.Stage)
	assert.Empty(t, alice[0].Delisting.Deadline)
	assert.Empty(t, alice[0].Delisting.Settlement)
	assert.Len(t, w.sender.to("bob@example.com"), 1)
	assert.Equal(t, 2, notices["cancelled"].Sent)
}

func TestNotices_AFailedSendIsTriedAgainWithoutTheChannelsSent(t *testing.T) {
	w := newNoticeWorld(t, tenDays)
	w.sender.fail = func(m notify.OTPMessage) bool { return m.ContactType == notify.ContactMobile }
	_, err := delisting.Advance(ctx, w.db, w.start, 100)
	require.NoError(t, err)
	_, err = w.n.Advance(ctx, w.db, w.start, 100)
	require.Error(t, err, "the run stops at the send that failed")
	assert.Len(t, w.sender.to("alice@example.com"), 1)
	assert.Empty(t, w.sender.to("bob@example.com"), "the run stopped before the others")

	// Before the delay, nothing is tried again; after it, the SMS goes
	// alone, and the others are sent.
	w.sender.fail = nil
	_, err = w.n.Advance(ctx, w.db, w.start.Add(time.Minute), 100)
	require.NoError(t, err)
	assert.Empty(t, w.sender.to("+989121111111"))
	assert.Len(t, w.sender.to("bob@example.com"), 1)
	_, err = w.n.Advance(ctx, w.db, w.start.Add(delisting.NoticeRetryDelay), 100)
	require.NoError(t, err)
	assert.Len(t, w.sender.to("+989121111111"), 1)
	assert.Len(t, w.sender.to("alice@example.com"), 1, "the email is not sent twice")
	n := w.notices(t)["step:delist_announced"]
	assert.Equal(t, 2, n.Sent)
	assert.Zero(t, n.Pending)
}

func TestNotices_ANoticeThatNeverGoesIsGivenUp(t *testing.T) {
	w2 := newNoticeWorld(t, tenDays)
	w2.sender.fail = func(m notify.OTPMessage) bool { return m.Contact == "bob@example.com" }
	_, err := delisting.Advance(ctx, w2.db, w2.start, 100)
	require.NoError(t, err)
	now := w2.start
	for i := 0; i < delisting.NoticeMaxAttempts; i++ {
		_, _ = w2.n.Advance(ctx, w2.db, now, 100)
		now = now.Add(time.Duration(delisting.NoticeMaxAttempts) * delisting.NoticeRetryDelay)
	}
	n := w2.notices(t)["step:delist_announced"]
	assert.Equal(t, 1, n.Failed)
	assert.Equal(t, 1, n.Sent)
	assert.Equal(t, 1, n.NoContact)
	assert.Zero(t, n.Pending)
}

func TestNotices_StepsAppliedAtOnceAreToldOnce(t *testing.T) {
	w := newNoticeWorld(t, func(start time.Time) delisting.Plan {
		p := plan(start, currencies.LifecycleTradingHalted, currencies.LifecycleWithdrawalOnly)
		p.Kind = delisting.KindEmergency
		return p
	})
	w.at(t, w.start.Add(2*time.Hour))
	notices := w.notices(t)
	assert.Len(t, notices, 1)
	assert.Contains(t, notices, "step:withdrawal_only")
	assert.Len(t, w.sender.to("bob@example.com"), 1)
}

func TestNotices_AStepTooOldIsNotTold(t *testing.T) {
	w := newNoticeWorld(t, tenDays)
	_, err := delisting.Advance(ctx, w.db, w.start, 100)
	require.NoError(t, err)
	_, err = w.n.Advance(ctx, w.db, w.start.Add(delisting.NoticeWindow+time.Hour), 100)
	require.NoError(t, err)
	assert.Empty(t, w.notices(t))
	assert.Empty(t, w.sender.sent)
}

func TestNotices_OffSendNothing(t *testing.T) {
	w2 := newNoticeWorld(t, tenDays)
	w2.n.Sender = nil
	w2.at(t, w2.start)
	assert.Empty(t, w2.notices(t))
}

func TestNotices_InstancesShareTheRecipients(t *testing.T) {
	w := newNoticeWorld(t, tenDays)
	const users = 130
	for i := range users {
		usertest.Insert(t, w.db, usertest.User{Email: fmt.Sprintf("user%d@example.com", i)})
	}
	_, err := delisting.Advance(ctx, w.db, w.start, 100)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := w.n.Advance(ctx, w.db, w.start, 100)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	for i := range users {
		assert.Len(t, w.sender.to(fmt.Sprintf("user%d@example.com", i)), 1, "user %d is told once", i)
	}
	n := w.notices(t)["step:delist_announced"]
	assert.Equal(t, users+3, n.Recipients)
	assert.Zero(t, n.Pending)
	assert.Equal(t, 1, count(t, w.db, `SELECT count(*) FROM currency_delisting_notices`))
}

func TestNotices_OfAnotherCurrencyAreNotFound(t *testing.T) {
	w := newNoticeWorld(t, tenDays)
	other := insertCurrency(t, w.db, sym("OTH"), "approved")
	_, err := delisting.Notices(ctx, w.db, other, w.d.ID)
	assert.ErrorIs(t, err, delisting.ErrNotFound)
}
