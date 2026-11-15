package confirm

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"
	"github.com/mdastpak/crypto-exchange/internal/userauth"

	"github.com/stretchr/testify/require"
)

// dbAuthenticator is an authenticator app that reads the database on a
// connection of its own, as userauth.TOTP does.
type dbAuthenticator struct {
	db   *sql.DB
	code string
}

func (a dbAuthenticator) Enabled(ctx context.Context, _ string) (bool, error) {
	var on bool
	err := a.db.QueryRowContext(ctx, `SELECT true`).Scan(&on)
	return on, err
}

func (a dbAuthenticator) Verify(ctx context.Context, _, code string) error {
	var one int
	if err := a.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		return err
	}
	if code != a.code {
		return userauth.ErrWrongTOTP
	}
	return nil
}

// TestService_NeedsOneConnectionOfThePool runs every step of a
// confirmation with a pool of one connection (#405): a step that held a
// transaction while it read on another connection would wait for itself
// until its deadline.
func TestService_NeedsOneConnectionOfThePool(t *testing.T) {
	f := newFixture(t)
	alice := f.user(usertest.User{Email: "alice@example.com", Mobile: "09121234567"})
	f.Authenticator = dbAuthenticator{db: f.DB, code: "654321"}
	f.DB.SetMaxOpenConns(1)
	t.Cleanup(func() { f.DB.SetMaxOpenConns(0) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// By SMS: a new code, a wrong one, and the right one.
	require.NoError(t, f.SetMethod(ctx, alice, MethodSMS, audit.Event{}))
	digest := withdrawal("3", "d")
	c, err := f.Start(ctx, alice, PurposeWithdrawal, digest, Details{Amount: "3 USDT", Destination: "T..."}, "client")
	require.NoError(t, err)
	_, err = f.Resend(ctx, alice, c.ID, "client")
	require.NoError(t, err)
	require.ErrorIs(t, f.Confirm(ctx, alice, c.ID, Proof{Code: "000000"}, audit.Event{}), ErrInvalidCode)
	require.NoError(t, f.Confirm(ctx, alice, c.ID, Proof{Code: f.sender.last().Code}, audit.Event{}))
	require.NoError(t, f.consume(alice, c.ID, digest))

	// By the authenticator, which reads the database itself.
	require.NoError(t, f.SetMethod(ctx, alice, MethodTOTP, audit.Event{}))
	c, err = f.Start(ctx, alice, PurposeWithdrawal, digest, Details{Amount: "3 USDT", Destination: "T..."}, "client")
	require.NoError(t, err)
	require.Equal(t, MethodTOTP, c.Method)
	require.ErrorIs(t, f.Confirm(ctx, alice, c.ID, Proof{Code: "000000"}, audit.Event{}), ErrInvalidCode)
	require.NoError(t, f.Confirm(ctx, alice, c.ID, Proof{Code: "654321"}, audit.Event{}))
	require.NoError(t, ctx.Err(), "no step waited for its deadline")
}

// TestService_AProofOfACodeThatWasReplaced checks that a code checked
// before a new one was sent does not confirm: the proof is checked before
// the lock, and the lock checks that the code is still the one sent.
func TestService_AProofOfACodeThatWasReplaced(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user(usertest.User{Email: "alice@example.com"})
	c, err := f.Start(ctx, alice, PurposeWithdrawal, withdrawal("1", "d"), Details{Amount: "1 USDT", Destination: "T..."}, "client")
	require.NoError(t, err)
	first := f.sender.last().Code
	_, err = f.Resend(ctx, alice, c.ID, "client")
	require.NoError(t, err)
	require.ErrorIs(t, f.Confirm(ctx, alice, c.ID, Proof{Code: first}, audit.Event{}), ErrInvalidCode, "the first code is of the session before")
	require.NoError(t, f.Confirm(ctx, alice, c.ID, Proof{Code: f.sender.last().Code}, audit.Event{}))
}
