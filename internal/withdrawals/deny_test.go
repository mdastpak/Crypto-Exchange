package withdrawals

import (
	"context"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/confirm"
	"github.com/mdastpak/crypto-exchange/internal/denylink"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// links returns the "this was not me" links of f (#374).
func (f fixture) links() denylink.Service {
	return denylink.Service{DB: f.DB, BaseURL: "https://ex.example/d/", TTL: time.Hour, Hold: 24 * time.Hour,
		Confirmations: f.confirm, Withdrawals: f.Service}
}

func (f fixture) confirmationOf(withdrawalID string) string {
	f.t.Helper()
	var id string
	require.NoError(f.t, f.DB.QueryRow(`SELECT confirmation_id FROM withdrawals WHERE id = $1`, withdrawalID).Scan(&id))
	return id
}

func (f fixture) session() string {
	f.t.Helper()
	var id string
	require.NoError(f.t, f.DB.QueryRow(`INSERT INTO sessions (user_id, auth_method, access_token_hash, refresh_token_hash, expires_at)
		VALUES ($1, 'OTP', 'a', 'r', NOW() + INTERVAL '1 hour') RETURNING id`, f.user).Scan(&id))
	require.NoError(f.t, rowseal.Reseal(context.Background(), f.DB, rowseal.Sessions, "id = $1", id))
	return id
}

func (f fixture) events(action string) []audit.Event {
	f.t.Helper()
	rows, err := f.DB.Query(`SELECT actor_id, COALESCE(reason, '') FROM audit_events WHERE action = $1 ORDER BY id`, action)
	require.NoError(f.t, err)
	defer rows.Close()
	var out []audit.Event
	for rows.Next() {
		var e audit.Event
		require.NoError(f.t, rows.Scan(&e.ActorID, &e.Reason))
		out = append(out, e)
	}
	require.NoError(f.t, rows.Err())
	return out
}

// TestDeny_StopsTheWithdrawalsAndSecuresTheAccount uses the link of a
// withdrawal's message (#374): it cancels that withdrawal and the user's
// other one that is not being paid yet, releases their funds, ends the
// session, starts the security hold, and audits it all. The one being
// paid already is counted, not stopped.
func TestDeny_StopsTheWithdrawalsAndSecuresTheAccount(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "")
	now := time.Now()
	paying, err := f.withdraw(f.request("100", "deny-paying"))
	require.NoError(t, err)
	_, err = f.payouts(config.PayoutsConfig{}, &now).Run(ctx)
	require.NoError(t, err)
	first, err := f.withdraw(f.request("50", "deny-1"))
	require.NoError(t, err)
	second, err := f.withdraw(f.request("20", "deny-2"))
	require.NoError(t, err)
	session := f.session()

	links := f.links()
	link, err := links.Issue(ctx, f.user, f.confirmationOf(first.ID))
	require.NoError(t, err)
	assert.Len(t, link.Token, denylink.TokenLength)
	assert.Equal(t, "https://ex.example/d/"+link.Token, link.URL)

	out, err := links.Deny(ctx, link.Token, origin)
	require.NoError(t, err)
	assert.Equal(t, 2, out.WithdrawalsCancelled, "the link's and the other one")
	assert.Equal(t, 1, out.WithdrawalsInProgress, "the one being paid")
	assert.Equal(t, 1, out.SessionsEnded)
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), out.SecurityHoldUntil, time.Minute)

	for _, id := range []string{first.ID, second.ID} {
		got, err := f.Get(ctx, f.user, id)
		require.NoError(t, err)
		assert.Equal(t, StatusCancelled, got.Status)
	}
	got, err := f.Get(ctx, f.user, paying.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusProcessing, got.Status)
	available, locked := f.balance()
	assert.Equal(t, "899", available, "the funds of the cancelled ones are back")
	assert.Equal(t, "101", locked, "the one being paid stays locked")

	var status string
	require.NoError(t, f.DB.QueryRow(`SELECT status FROM sessions WHERE id = $1`, session).Scan(&status))
	assert.Equal(t, "REVOKED", status)
	hold, err := restrictions.SecurityHold(ctx, f.DB, f.user)
	require.NoError(t, err)
	require.NotNil(t, hold)
	_, err = f.Request(ctx, f.user, f.request("10", "after-deny"), "client", origin)
	var refusal *restrictions.Refusal
	require.ErrorAs(t, err, &refusal, "the hold refuses a new withdrawal")
	assert.Equal(t, restrictions.CodeSecurityHold, refusal.Code)

	denials := f.events(audit.ActionUserOperationDeny)
	require.Len(t, denials, 1)
	assert.Equal(t, f.user, denials[0].ActorID)
	cancels := f.events(audit.ActionUserWithdrawalCancel)
	require.Len(t, cancels, 2)
	assert.Equal(t, ReasonDenied, cancels[0].Reason)
	revokes := f.events(audit.ActionUserSessionRevoke)
	require.Len(t, revokes, 1)
	assert.Equal(t, denylink.ReasonDenied, revokes[0].Reason)

	_, err = links.Deny(ctx, link.Token, origin)
	require.ErrorIs(t, err, denylink.ErrInvalid, "a link is used once")
}

// TestDeny_AConfirmationCanNoLongerBeConfirmed denies a confirmation that
// is waiting for its code: the code then confirms nothing, and the
// confirmation cannot be sent again.
func TestDeny_AConfirmationCanNoLongerBeConfirmed(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "")
	out, err := f.Request(ctx, f.user, f.request("100", "deny-waiting"), "client", origin)
	require.NoError(t, err)
	require.NotNil(t, out.Challenge)
	links := f.links()
	link, err := links.Issue(ctx, f.user, out.Challenge.ID)
	require.NoError(t, err)

	result, err := links.Deny(ctx, link.Token, origin)
	require.NoError(t, err)
	assert.Zero(t, result.WithdrawalsCancelled, "none was requested yet")
	err = f.confirm.Confirm(ctx, f.user, out.Challenge.ID, confirm.Proof{Code: "123456"}, origin)
	require.ErrorIs(t, err, confirm.ErrDenied)
	_, err = f.confirm.Resend(ctx, f.user, out.Challenge.ID, "client")
	require.ErrorIs(t, err, confirm.ErrDenied)
	available, _ := f.balance()
	assert.Equal(t, "1000", available)
}

// endsAtNotice ends the request of a denial as its notice goes out, and
// records whether the notice's context ended with it.
type endsAtNotice struct {
	denylink.Confirmations
	cancel context.CancelFunc
	called bool
	err    error
}

func (e *endsAtNotice) NotifyDenied(ctx context.Context, userHK string, d confirm.Denial) error {
	e.cancel()
	e.called, e.err = true, ctx.Err()
	return e.Confirmations.NotifyDenied(ctx, userHK, d)
}

// TestDeny_TheNoticeDoesNotEndWithTheRequest ends the request of a denial
// once the account is secured, as a page closed after the POST does: the
// notice still goes out, since the used link cannot be asked again.
func TestDeny_TheNoticeDoesNotEndWithTheRequest(t *testing.T) {
	f := newFixture(t, "")
	w, err := f.withdraw(f.request("100", "deny-notice"))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notices := &endsAtNotice{Confirmations: f.confirm, cancel: cancel}
	links := f.links()
	links.Confirmations = notices
	link, err := links.Issue(ctx, f.user, f.confirmationOf(w.ID))
	require.NoError(t, err)

	_, err = links.Deny(ctx, link.Token, origin)
	require.NoError(t, err)
	require.True(t, notices.called, "the notice is sent")
	assert.NoError(t, notices.err, "the notice's context outlives the request")
}

// TestDeny_RefusesWhatIsNotAUsableLink: a malformed or unknown token, an
// expired link, and a link raised in SQL all answer the same, and change
// nothing.
func TestDeny_RefusesWhatIsNotAUsableLink(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "")
	w, err := f.withdraw(f.request("100", "deny-refused"))
	require.NoError(t, err)
	links := f.links()
	for _, token := range []string{"", "short", "ABCDEFGHIJK!", "ABCDEFGHIJKLM", "ABCDEFGHIJKL"} {
		_, err := links.Deny(ctx, token, origin)
		require.ErrorIs(t, err, denylink.ErrInvalid, token)
	}

	expired, err := links.Issue(ctx, f.user, f.confirmationOf(w.ID))
	require.NoError(t, err)
	_, err = f.DB.Exec(`UPDATE deny_links SET expires_at = NOW() - INTERVAL '1 minute'`)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, f.DB, rowseal.DenyLinks, "TRUE"))
	_, err = links.Deny(ctx, expired.Token, origin)
	require.ErrorIs(t, err, denylink.ErrInvalid, "expired")

	// Renewed in SQL, without its seal.
	_, err = f.DB.Exec(`UPDATE deny_links SET expires_at = NOW() + INTERVAL '1 hour'`)
	require.NoError(t, err)
	_, err = links.Deny(ctx, expired.Token, origin)
	require.ErrorIs(t, err, denylink.ErrInvalid, "a link raised in SQL denies nothing")

	got, err := f.Get(ctx, f.user, w.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusQueued, got.Status, "nothing changed")
	hold, err := restrictions.SecurityHold(ctx, f.DB, f.user)
	require.NoError(t, err)
	assert.Nil(t, hold)
	var hashes int
	require.NoError(t, f.DB.QueryRow(`SELECT COUNT(*) FROM deny_links WHERE token_hash = convert_to($1, 'UTF8')`, expired.Token).Scan(&hashes))
	assert.Zero(t, hashes, "the token is stored only as its hash")
}
