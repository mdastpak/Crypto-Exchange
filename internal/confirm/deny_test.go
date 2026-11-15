package confirm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// links issues a numbered link for each message, or fails.
type links struct {
	mu     sync.Mutex
	issued []string
	fail   error
}

func (l *links) Issue(_ context.Context, _, id string) (Link, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil {
		return Link{}, l.fail
	}
	l.issued = append(l.issued, id)
	token := fmt.Sprintf("TOKEN%07d", len(l.issued))
	return Link{Token: token, URL: "https://ex.example/d/" + token}, nil
}

// TestService_EachMessageCarriesItsOwnDenyLink: the first code and each
// code sent again carry a link of their own (#374), and no code goes out
// without its link.
func TestService_EachMessageCarriesItsOwnDenyLink(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	l := &links{}
	f.Links = l
	alice := f.user(usertest.User{Email: "alice@example.com"})

	c, err := f.Start(ctx, alice, PurposeWithdrawal, withdrawal("100", "dest-1"), Details{Amount: "100 USDT", Destination: "TR7NHq...gjLj6t"}, "client")
	require.NoError(t, err)
	first := f.sender.last()
	assert.Equal(t, "TOKEN0000001", first.DenyToken)
	assert.Equal(t, "https://ex.example/d/TOKEN0000001", first.DenyLink)
	_, err = f.Resend(ctx, alice, c.ID, "client")
	require.NoError(t, err)
	again := f.sender.last()
	assert.Equal(t, "TOKEN0000002", again.DenyToken, "a link of its own")
	assert.Equal(t, []string{c.ID, c.ID}, l.issued, "bound to the confirmation")

	l.fail = errors.New("the database is down")
	sent := len(f.sender.sent)
	_, err = f.Resend(ctx, alice, c.ID, "client")
	require.Error(t, err)
	assert.Len(t, f.sender.sent, sent, "no code without its link")
}

// TestService_ADeniedConfirmationConfirmsNothing denies a confirmation in
// the transaction of a link: it can no longer be confirmed, sent again,
// or consumed, and a denial cleared in SQL is detected by the seal.
func TestService_ADeniedConfirmationConfirmsNothing(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user(usertest.User{Email: "alice@example.com"})
	digest := withdrawal("100", "dest-1")
	details := Details{Amount: "100 USDT", Destination: "TR7NHq...gjLj6t"}

	confirmed, err := f.Start(ctx, alice, PurposeWithdrawal, digest, details, "client")
	require.NoError(t, err)
	require.NoError(t, f.Confirm(ctx, alice, confirmed.ID, Proof{Code: f.sender.last().Code}, audit.Event{}))
	waiting, err := f.Start(ctx, alice, PurposeWithdrawal, digest, details, "client")
	require.NoError(t, err)

	deny := func(id string) Details {
		t.Helper()
		tx, err := f.DB.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer tx.Rollback()
		got, err := f.Deny(ctx, tx, alice, id)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		return got
	}
	assert.Equal(t, details, deny(confirmed.ID), "what it was for")
	deny(waiting.ID)
	assert.Equal(t, details, deny(waiting.ID), "twice is once")

	require.ErrorIs(t, f.consume(alice, confirmed.ID, digest), ErrDenied, "a confirmed one runs nothing")
	require.ErrorIs(t, f.Confirm(ctx, alice, waiting.ID, Proof{Code: f.sender.last().Code}, audit.Event{}), ErrDenied)
	_, err = f.Resend(ctx, alice, waiting.ID, "client")
	require.ErrorIs(t, err, ErrDenied)

	_, err = f.DB.Exec(`UPDATE operation_confirmations SET denied_at = NULL WHERE id = $1`, confirmed.ID)
	require.NoError(t, err)
	require.ErrorIs(t, f.consume(alice, confirmed.ID, digest), rowseal.ErrUnverified, "a denial cleared in SQL")
}

// TestService_NotifyDeniedEmailsTheUser sends the notice of a denial to the
// verified email address alone, which has no SMS template.
func TestService_NotifyDeniedEmailsTheUser(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user(usertest.User{Email: "alice@example.com", Mobile: "09121234567"})
	require.NoError(t, f.NotifyDenied(ctx, alice, Denial{Details: Details{Amount: "100 USDT", Destination: "TR7NHq...gjLj6t"},
		Cancelled: 2, InProgress: 1, HoldHours: 24}))
	require.Len(t, f.sender.sent, 1)
	got := f.sender.last()
	assert.Equal(t, notify.PurposeDenied, got.Purpose)
	assert.Equal(t, notify.ContactEmail, got.ContactType)
	assert.Equal(t, "alice@example.com", got.Contact)
	assert.Equal(t, "100 USDT to TR7NHq...gjLj6t", got.Details)
	assert.Equal(t, 2, got.Cancelled)
	assert.Equal(t, 1, got.InProgress)
	assert.Equal(t, 24, got.HoldHours)
	assert.Empty(t, got.Code)
	assert.Empty(t, got.DenyLink, "the notice carries no link")
}
