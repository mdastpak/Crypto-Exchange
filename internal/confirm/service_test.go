package confirm

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/passkey"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/redistest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"
	"github.com/mdastpak/crypto-exchange/internal/userauth"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sender keeps the codes it is asked to send.
type sender struct {
	mu   sync.Mutex
	sent []notify.OTPMessage
}

func (s *sender) Supports(contactType string) bool {
	return contactType == "email" || contactType == "mobile"
}

func (s *sender) SendOTP(_ context.Context, msg notify.OTPMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, msg)
	return nil
}

func (s *sender) last() notify.OTPMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent[len(s.sent)-1]
}

// authenticator is an authenticator app that accepts one code.
type authenticator struct {
	enabled bool
	code    string
	locked  bool
}

func (a *authenticator) Enabled(context.Context, string) (bool, error) { return a.enabled, nil }

func (a *authenticator) Verify(_ context.Context, _, code string) error {
	switch {
	case !a.enabled:
		return userauth.ErrNoTOTP
	case a.locked:
		return &userauth.LockedError{Until: time.Now().Add(time.Minute)}
	case code != a.code:
		return userauth.ErrWrongTOTP
	}
	return nil
}

type fixture struct {
	Service
	sender *sender
	auth   *authenticator
	t      *testing.T
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	conn := pgtest.Migrated(t, "testdb_confirm")
	sealtest.Use(t, conn, rowseal.ModeEnforce)
	client := redis.NewClient(&redis.Options{Addr: redistest.Addr, DB: redistest.Confirm})
	t.Cleanup(func() { client.Close() })
	if _, err := net.DialTimeout("tcp", redistest.Addr, time.Second); err != nil {
		t.Skipf("Redis is not reachable: %v", err)
	}
	require.NoError(t, client.FlushDB(context.Background()).Err())
	codes := otp.NewStore(client, otp.Settings{Namespace: "confirm", TTL: time.Minute, AttemptsPerCode: 3,
		ContactRequestsPerHour: 10, ContactAttemptsPerHour: 10})
	s, a := &sender{}, &authenticator{}
	return fixture{
		Service: Service{DB: conn, Codes: codes, Sender: s, SMSForWithdrawals: true, Authenticator: a, MaxAttempts: 3},
		sender:  s, auth: a, t: t,
	}
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

func (f fixture) consume(userHK, id string, digest []byte) error {
	f.t.Helper()
	ctx := context.Background()
	tx, err := f.DB.BeginTx(ctx, nil)
	require.NoError(f.t, err)
	defer tx.Rollback()
	if err := f.Consume(ctx, tx, userHK, id, PurposeWithdrawal, digest); err != nil {
		return err
	}
	return tx.Commit()
}

func withdrawal(amount, destination string) []byte {
	return Digest(PurposeWithdrawal, amount, "USDT", destination)
}

func TestService_AnSMSCodeConfirmsItsWithdrawalOnce(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user(usertest.User{Email: "alice@example.com", Mobile: "09121234567"})
	digest := withdrawal("100", "dest-1")

	st, err := f.Settings(ctx, alice)
	require.NoError(t, err)
	assert.Equal(t, []Method{MethodSMS, MethodEmail}, st.Available, "no authenticator yet")
	assert.Equal(t, MethodSMS, st.Effective)

	c, err := f.Start(ctx, alice, PurposeWithdrawal, digest, Details{Amount: "100 USDT", Destination: "TR7NHq...gjLj6t"}, "client")
	require.NoError(t, err)
	assert.Equal(t, MethodSMS, c.Method)
	assert.Equal(t, "091*****567", c.SentTo)
	sent := f.sender.last()
	assert.Equal(t, notify.PurposeWithdrawal, sent.Purpose, "the code goes out with the withdrawal text")
	assert.Equal(t, "100 USDT to TR7NHq...gjLj6t", sent.Details)
	assert.Equal(t, "100 USDT", sent.Amount, "in two parts too, for templates with short parameters")
	assert.Equal(t, "TR7NHq...gjLj6t", sent.Destination)
	assert.Equal(t, Details{Amount: "100 USDT", Destination: "TR7NHq...gjLj6t"}, c.Details)

	require.ErrorIs(t, f.consume(alice, c.ID, digest), ErrNotConfirmed, "not before the code")
	require.ErrorIs(t, f.Confirm(ctx, alice, c.ID, Proof{Code: "000000"}, audit.Event{}), ErrInvalidCode)
	require.NoError(t, f.Confirm(ctx, alice, c.ID, Proof{Code: sent.Code}, audit.Event{}))
	require.ErrorIs(t, f.Confirm(ctx, alice, c.ID, Proof{Code: sent.Code}, audit.Event{}), ErrAlreadyConfirmed)

	// Bound to its operation: another amount or destination is refused.
	require.ErrorIs(t, f.consume(alice, c.ID, withdrawal("1000", "dest-1")), ErrMismatch)
	require.ErrorIs(t, f.consume(alice, c.ID, withdrawal("100", "dest-2")), ErrMismatch)
	bob := f.user(usertest.User{Email: "bob@example.com"})
	require.ErrorIs(t, f.consume(bob, c.ID, digest), ErrNotFound, "not another user's")

	require.NoError(t, f.consume(alice, c.ID, digest))
	require.ErrorIs(t, f.consume(alice, c.ID, digest), ErrConsumed, "once")

	var successes, failures int
	require.NoError(t, f.DB.QueryRow(`SELECT COUNT(*) FILTER (WHERE outcome = 'success'), COUNT(*) FILTER (WHERE outcome = 'failure')
		FROM audit_events WHERE action = $1`, audit.ActionUserOperationConfirm).Scan(&successes, &failures))
	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, failures)
	var changes string
	require.NoError(t, f.DB.QueryRow(`SELECT changes::text FROM audit_events WHERE action = $1 LIMIT 1`, audit.ActionUserOperationConfirm).Scan(&changes))
	assert.NotContains(t, changes, sent.Code, "the events never hold the code")
}

func TestService_WrongCodesEndTheConfirmation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user(usertest.User{Email: "alice@example.com"})
	c, err := f.Start(ctx, alice, PurposeWithdrawal, withdrawal("1", "d"), Details{Amount: "1 USDT", Destination: "T..."}, "client")
	require.NoError(t, err)
	assert.Equal(t, MethodEmail, c.Method)
	code := f.sender.last().Code

	require.ErrorIs(t, f.Confirm(ctx, alice, c.ID, Proof{Code: "111111"}, audit.Event{}), ErrInvalidCode)
	require.ErrorIs(t, f.Confirm(ctx, alice, c.ID, Proof{Code: "222222"}, audit.Event{}), ErrInvalidCode)
	require.ErrorIs(t, f.Confirm(ctx, alice, c.ID, Proof{Code: "333333"}, audit.Event{}), ErrTooManyAttempts)
	require.ErrorIs(t, f.Confirm(ctx, alice, c.ID, Proof{Code: code}, audit.Event{}), ErrTooManyAttempts, "even the right code, after too many wrong ones")
	_, err = f.Resend(ctx, alice, c.ID, "client")
	require.ErrorIs(t, err, ErrTooManyAttempts)
}

func TestService_TheAuthenticatorAndTheChosenMethod(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user(usertest.User{Email: "alice@example.com", Mobile: "09121234567"})
	f.auth.enabled, f.auth.code = true, "654321"

	st, err := f.Settings(ctx, alice)
	require.NoError(t, err)
	assert.Equal(t, []Method{MethodTOTP, MethodSMS, MethodEmail}, st.Available)
	assert.Equal(t, MethodTOTP, st.Effective, "the authenticator first, when the user chose none")
	assert.Empty(t, st.Method)

	require.NoError(t, f.SetMethod(ctx, alice, MethodEmail, audit.Event{}))
	st, err = f.Settings(ctx, alice)
	require.NoError(t, err)
	assert.Equal(t, MethodEmail, st.Method)
	assert.Equal(t, MethodEmail, st.Effective)
	require.ErrorIs(t, f.SetMethod(ctx, alice, Method("pigeon"), audit.Event{}), ErrMethodUnavailable)

	require.NoError(t, f.SetMethod(ctx, alice, MethodTOTP, audit.Event{}))
	digest := withdrawal("5", "d")
	c, err := f.Start(ctx, alice, PurposeWithdrawal, digest, Details{Amount: "5 USDT", Destination: "T..."}, "client")
	require.NoError(t, err)
	assert.Equal(t, MethodTOTP, c.Method)
	assert.Empty(t, c.SentTo, "nothing is sent")
	_, err = f.Resend(ctx, alice, c.ID, "client")
	require.ErrorIs(t, err, ErrMethodUnavailable)
	require.ErrorIs(t, f.Confirm(ctx, alice, c.ID, Proof{Code: "000000"}, audit.Event{}), ErrInvalidCode)
	require.NoError(t, f.Confirm(ctx, alice, c.ID, Proof{Code: "654321"}, audit.Event{}))
	require.NoError(t, f.consume(alice, c.ID, digest))

	// A locked authenticator ends the confirmation's attempts.
	c, err = f.Start(ctx, alice, PurposeWithdrawal, digest, Details{Amount: "5 USDT", Destination: "T..."}, "client")
	require.NoError(t, err)
	f.auth.locked = true
	require.ErrorIs(t, f.Confirm(ctx, alice, c.ID, Proof{Code: "654321"}, audit.Event{}), ErrTooManyAttempts)

	// When the chosen method goes away, the next available one is used.
	f.auth.enabled, f.auth.locked = false, false
	st, err = f.Settings(ctx, alice)
	require.NoError(t, err)
	assert.Equal(t, MethodTOTP, st.Method)
	assert.Equal(t, MethodSMS, st.Effective)

	// No SMS template: SMS is not offered for withdrawals.
	f.SMSForWithdrawals = false
	st, err = f.Settings(ctx, alice)
	require.NoError(t, err)
	assert.Equal(t, []Method{MethodEmail}, st.Available)

	// A choice changed in SQL is ignored, and its finding recorded.
	require.NoError(t, f.SetMethod(ctx, alice, MethodEmail, audit.Event{}))
	_, err = f.DB.Exec(`UPDATE user_confirmation_methods SET method = 'sms' WHERE user_hk = $1`, alice)
	require.NoError(t, err)
	st, err = f.Settings(ctx, alice)
	require.NoError(t, err)
	assert.Empty(t, st.Method)
	var findings int
	require.NoError(t, f.DB.QueryRow(`SELECT COUNT(*) FROM integrity_findings WHERE table_name = 'user_confirmation_methods'`).Scan(&findings))
	assert.Equal(t, 1, findings)
}

func TestService_ExpiryAndSeals(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user(usertest.User{Email: "alice@example.com"})
	digest := withdrawal("7", "d")

	expiring, err := f.Start(ctx, alice, PurposeWithdrawal, digest, Details{Amount: "7 USDT", Destination: "T..."}, "client")
	require.NoError(t, err)
	backdate := func(id, column string) {
		t.Helper()
		_, err := f.DB.Exec(`UPDATE operation_confirmations SET `+column+` = `+column+` - interval '1 hour' WHERE id = $1`, id)
		require.NoError(t, err)
		require.NoError(t, rowseal.Reseal(ctx, f.DB, rowseal.OperationConfirmations, "id = $1", id))
	}
	backdate(expiring.ID, "expires_at")
	require.ErrorIs(t, f.Confirm(ctx, alice, expiring.ID, Proof{Code: f.sender.last().Code}, audit.Event{}), ErrExpired)

	// A new code gives it a new lifetime.
	renewed, err := f.Resend(ctx, alice, expiring.ID, "client")
	require.NoError(t, err)
	assert.True(t, renewed.ExpiresAt.After(time.Now()))
	require.NoError(t, f.Confirm(ctx, alice, expiring.ID, Proof{Code: f.sender.last().Code}, audit.Event{}))

	// Confirmed too long ago to be consumed.
	backdate(expiring.ID, "confirmed_at")
	require.ErrorIs(t, f.consume(alice, expiring.ID, digest), ErrExpired)

	// Marked confirmed in SQL: refused, and the finding is recorded.
	forged, err := f.Start(ctx, alice, PurposeWithdrawal, digest, Details{Amount: "7 USDT", Destination: "T..."}, "client")
	require.NoError(t, err)
	_, err = f.DB.Exec(`UPDATE operation_confirmations SET confirmed_at = NOW() WHERE id = $1`, forged.ID)
	require.NoError(t, err)
	require.ErrorIs(t, f.consume(alice, forged.ID, digest), ErrUnverified)
	var findings int
	require.NoError(t, f.DB.QueryRow(`SELECT COUNT(*) FROM integrity_findings WHERE table_name = 'operation_confirmations' AND row_key = $1`,
		forged.ID).Scan(&findings))
	assert.Equal(t, 1, findings)
}

func TestService_AUserWithoutAMethod(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	carol := f.user(usertest.User{Username: "carol"})
	_, err := f.Start(ctx, carol, PurposeWithdrawal, withdrawal("1", "d"), Details{Amount: "1 USDT", Destination: "T..."}, "client")
	require.ErrorIs(t, err, ErrNoMethod)
	_, err = f.Start(ctx, carol, PurposeWithdrawal, []byte("short"), Details{}, "client")
	require.Error(t, err)

	// A contact swapped in SQL receives no code.
	dave := f.user(usertest.User{Email: "dave@example.com"})
	_, err = f.DB.Exec(`UPDATE user_contacts SET verified_at = verified_at - interval '1 second' WHERE user_hk = $1`, dave)
	require.NoError(t, err)
	_, err = f.Start(ctx, dave, PurposeWithdrawal, withdrawal("1", "d"), Details{Amount: "1 USDT", Destination: "T..."}, "client")
	require.ErrorIs(t, err, ErrNoMethod)
}

func TestDigest(t *testing.T) {
	assert.Len(t, Digest(PurposeWithdrawal, "1"), 32)
	assert.Equal(t, Digest(PurposeWithdrawal, "1", "USDT"), Digest(PurposeWithdrawal, "1", "USDT"))
	assert.NotEqual(t, Digest(PurposeWithdrawal, "1", "USDT"), Digest(PurposeWithdrawal, "1USDT"), "fields are separated")
	assert.NotEqual(t, Digest(PurposeWithdrawal, "ab", "c"), Digest(PurposeWithdrawal, "a", "bc"))
}

func TestDetails_FitShortTemplateParameters(t *testing.T) {
	assert.True(t, Details{Amount: "100 USDT", Destination: "TR7NHq...gjLj6t"}.valid())
	assert.False(t, Details{Amount: "100 USDT"}.valid(), "a destination is required")
	assert.False(t, Details{Amount: "123456789012345678901 USDT", Destination: "T..."}.valid(), "at most 25 characters")
	assert.Equal(t, "1 BTC to bc1q...xyz", Details{Amount: "1 BTC", Destination: "bc1q...xyz"}.String())
}

// passkeys keeps one ceremony per scope, as the passkey service does, and
// accepts one answer.
type passkeys struct {
	count  int
	accept string
	begun  map[string]bool
}

func (p *passkeys) Enabled() bool                              { return true }
func (p *passkeys) Count(context.Context, string) (int, error) { return p.count, nil }
func (p *passkeys) BeginAssertion(_ context.Context, _, scope string) (*protocol.CredentialAssertion, error) {
	if p.count == 0 {
		return nil, passkey.ErrNoPasskey
	}
	p.begun[scope] = true
	return &protocol.CredentialAssertion{Response: protocol.PublicKeyCredentialRequestOptions{Challenge: []byte(scope)}}, nil
}

func (p *passkeys) FinishAssertion(_ context.Context, _, scope string, response []byte) error {
	if !p.begun[scope] {
		return passkey.ErrNoCeremony
	}
	delete(p.begun, scope)
	if string(response) != p.accept {
		return passkey.ErrInvalid
	}
	return nil
}

func TestService_APasskeyConfirmsItsOwnCeremony(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	keys := &passkeys{count: 1, accept: `{"signed":true}`, begun: map[string]bool{}}
	f.Passkeys, f.MaxAttempts = keys, 5
	alice := f.user(usertest.User{Email: "alice@example.com"})
	digest := withdrawal("9", "d")

	st, err := f.Settings(ctx, alice)
	require.NoError(t, err)
	assert.Equal(t, []Method{MethodPasskey, MethodEmail}, st.Available)
	assert.Equal(t, MethodPasskey, st.Effective, "a passkey first, as it resists phishing")

	sent := len(f.sender.sent)
	c, err := f.Start(ctx, alice, PurposeWithdrawal, digest, Details{Amount: "9 USDT", Destination: "T..."}, "client")
	require.NoError(t, err)
	assert.Equal(t, MethodPasskey, c.Method)
	require.NotNil(t, c.Passkey, "the options for the device")
	assert.Len(t, f.sender.sent, sent, "nothing is sent")

	// A wrong answer takes the ceremony; the right one then needs a new one.
	require.ErrorIs(t, f.Confirm(ctx, alice, c.ID, Proof{Passkey: []byte(`{"signed":false}`)}, audit.Event{}), ErrInvalidCode)
	require.ErrorIs(t, f.Confirm(ctx, alice, c.ID, Proof{Passkey: []byte(keys.accept)}, audit.Event{}), ErrInvalidCode)
	require.ErrorIs(t, f.Confirm(ctx, alice, c.ID, Proof{Code: "123456"}, audit.Event{}), ErrInvalidCode, "a code does not confirm a passkey confirmation")
	renewed, err := f.Resend(ctx, alice, c.ID, "client")
	require.NoError(t, err)
	require.NotNil(t, renewed.Passkey)
	require.NoError(t, f.Confirm(ctx, alice, c.ID, Proof{Passkey: []byte(keys.accept)}, audit.Event{}))
	require.NoError(t, f.consume(alice, c.ID, digest))

	// Without a passkey, the method is not offered.
	keys.count = 0
	st, err = f.Settings(ctx, alice)
	require.NoError(t, err)
	assert.Equal(t, []Method{MethodEmail}, st.Available)
}
