// Package denylink is the "this was not me" link (#374). Every message
// that carries the code of a withdrawal confirmation (#161) carries a link
// too, and the link can only stop and protect. It never approves anything:
// the product owner decided on 2026-11-14 not to confirm operations by
// link, since links invite phishing, and mail scanners and link previews
// open them on their own. A link that can only stop is harmless when
// someone else opens it: they can hold the owner's withdrawals for a
// while, and gain nothing.
//
// A link is the configured base URL on the exchange's own domain and a
// token of 12 Base62 characters, about 71 bits from crypto/rand, short
// enough for an SMS template to hold after the domain (#371). The token is
// stored only as its SHA-256, bound to the user and the confirmation. It
// is used once, and it expires.
//
// Opening the link changes nothing: it shows a page of the exchange's
// site, which asks the user and posts the token to POST
// /public/deny/{token}. Deny then, in one transaction:
//
//  1. starts the security hold (#251), which locks the user's row before
//     anything else, as a withdrawal request does, so that a request in
//     flight commits first and is cancelled below;
//  2. marks the link used;
//  3. denies the confirmation, which can no longer be confirmed, sent
//     again, or consumed;
//  4. cancels the user's withdrawals whose payouts have not started, and
//     releases their funds (#70): someone else may hold the account, and
//     may have requested more than one;
//  5. ends every session of the user (#193);
//  6. records it all in the audit log.
//
// Once it commits, the user gets a notice by email of what was done.
package denylink

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/confirm"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// TokenLength is the length of a token, in characters of alphabet.
const TokenLength = 12

// alphabet is Base62: the characters of a token, which need no escaping
// in a URL or an SMS.
const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// ReasonDenied is the audit reason of a session that a link ended.
const ReasonDenied = "deny_link"

// ErrInvalid is a token that is not of a usable link: unknown, expired,
// used, or changed in SQL. The answer is the same for each, and names no
// account.
var ErrInvalid = errors.New("the link is not valid")

// Confirmations denies the confirmation of a link and sends the notice of
// a denial: confirm.Service.
type Confirmations interface {
	Deny(ctx context.Context, tx *sql.Tx, userHK, id string) (confirm.Details, error)
	NotifyDenied(ctx context.Context, userHK string, d confirm.Denial) error
}

// Withdrawals cancels the withdrawals of a denial: withdrawals.Service.
type Withdrawals interface {
	CancelOnDenial(ctx context.Context, tx *sql.Tx, userHK string, origin audit.Event) (cancelled, inProgress int, err error)
}

// Service issues and uses the links.
type Service struct {
	DB *sql.DB
	// BaseURL starts each link, such as "https://ex.ir/d/"; the token
	// completes it.
	BaseURL string
	// TTL is how long a link can be used.
	TTL time.Duration
	// Hold is the length of the security hold that a denial starts.
	Hold          time.Duration
	Confirmations Confirmations
	Withdrawals   Withdrawals
}

// Outcome is what a denial did, as the page shows it. It names no
// account.
type Outcome struct {
	// WithdrawalsCancelled counts the withdrawals that were cancelled, with
	// their funds released.
	WithdrawalsCancelled int `json:"withdrawals_cancelled"`
	// WithdrawalsInProgress counts the withdrawals that were being paid
	// already, which only support can follow.
	WithdrawalsInProgress int `json:"withdrawals_in_progress"`
	// SessionsEnded counts the sessions that ended.
	SessionsEnded int `json:"sessions_ended"`
	// SecurityHoldUntil is when the security hold ends.
	SecurityHoldUntil time.Time `json:"security_hold_until"`
}

// Valid reports whether token has the form of a token.
func Valid(token string) bool {
	if len(token) != TokenLength {
		return false
	}
	for i := 0; i < len(token); i++ {
		if !strings.ContainsRune(alphabet, rune(token[i])) {
			return false
		}
	}
	return true
}

// newToken returns a random token. A byte of 248 or more is skipped, so
// that each of the 62 characters is as likely: 248 is 4 times 62.
func newToken() (string, error) {
	out := make([]byte, 0, TokenLength)
	buf := make([]byte, 2*TokenLength)
	for len(out) < TokenLength {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("read random bytes: %w", err)
		}
		for _, b := range buf {
			if b >= 248 {
				continue
			}
			out = append(out, alphabet[b%62])
			if len(out) == TokenLength {
				break
			}
		}
	}
	return string(out), nil
}

// hash is what the table stores of a token.
func hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Issue issues the link of a message of the confirmation id of the user,
// on a connection of its own: each message gets a link of its own. It
// implements confirm.DenyLinks.
func (s Service) Issue(ctx context.Context, userHK, confirmationID string) (confirm.Link, error) {
	token, err := newToken()
	if err != nil {
		return confirm.Link{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return confirm.Link{}, err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, `INSERT INTO deny_links (token_hash, user_hk, confirmation_id, expires_at)
		VALUES ($1, $2, $3, NOW() + make_interval(secs => $4)) RETURNING id::text`,
		hash(token), userHK, confirmationID, s.TTL.Seconds()).Scan(&id); err != nil {
		return confirm.Link{}, fmt.Errorf("record the link: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.DenyLinks, "id = $1", id); err != nil {
		return confirm.Link{}, err
	}
	if err := tx.Commit(); err != nil {
		return confirm.Link{}, err
	}
	return confirm.Link{Token: token, URL: s.BaseURL + token}, nil
}

// Deny uses the link of token, with origin, the audit event of the
// request: see the package's documentation. It returns ErrInvalid for a
// token that is not of a usable link.
func (s Service) Deny(ctx context.Context, token string, origin audit.Event) (Outcome, error) {
	if !Valid(token) {
		return Outcome{}, ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Outcome{}, err
	}
	defer tx.Rollback()
	var id, userHK, confirmationID string
	var expired, used bool
	row := rowseal.DenyLinks.NewRow()
	err = tx.QueryRowContext(ctx, `SELECT id::text, user_hk::text, confirmation_id::text, expires_at <= NOW(), used_at IS NOT NULL, `+
		rowseal.DenyLinks.Fields("")+` FROM deny_links WHERE token_hash = $1 FOR UPDATE`, hash(token)).
		Scan(append([]any{&id, &userHK, &confirmationID, &expired, &used}, row.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Outcome{}, ErrInvalid
	}
	if err != nil {
		return Outcome{}, fmt.Errorf("read the link: %w", err)
	}
	// A link raised or renewed in SQL denies nothing; its finding is
	// recorded.
	if err := rowseal.Check(ctx, rowseal.DenyLinks, row); err != nil {
		if errors.Is(err, rowseal.ErrUnverified) {
			return Outcome{}, ErrInvalid
		}
		return Outcome{}, err
	}
	if expired || used {
		return Outcome{}, ErrInvalid
	}

	out := Outcome{}
	if out.SecurityHoldUntil, err = restrictions.StartSecurityHold(ctx, tx, userHK, s.Hold); err != nil {
		return Outcome{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deny_links SET used_at = NOW() WHERE id = $1`, id); err != nil {
		return Outcome{}, fmt.Errorf("use the link: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.DenyLinks, "id = $1", id); err != nil {
		return Outcome{}, err
	}
	denied := true
	details, err := s.Confirmations.Deny(ctx, tx, userHK, confirmationID)
	if errors.Is(err, rowseal.ErrUnverified) {
		// A confirmation changed in SQL keeps its finding, and the
		// account is secured all the same.
		log.Printf("The confirmation %s of a denial does not match its seal; the account of user %s is secured", confirmationID, userHK)
		denied = false
	} else if err != nil {
		return Outcome{}, err
	}
	origin.ActorType, origin.ActorID = audit.ActorUser, userHK
	if out.WithdrawalsCancelled, out.WithdrawalsInProgress, err = s.Withdrawals.CancelOnDenial(ctx, tx, userHK, origin); err != nil {
		return Outcome{}, err
	}
	ended, err := auth.EndAll(ctx, tx, userHK)
	if err != nil {
		return Outcome{}, err
	}
	out.SessionsEnded = len(ended)

	e := audit.Of(origin, audit.ActionUserOperationDeny, audit.TargetOperationConfirmation, confirmationID)
	e.Changes = &audit.Changes{After: map[string]any{"confirmation_denied": denied,
		"withdrawals_cancelled": out.WithdrawalsCancelled, "withdrawals_in_progress": out.WithdrawalsInProgress,
		"ended_sessions": out.SessionsEnded, "security_hold_until": out.SecurityHoldUntil.UTC().Format(time.RFC3339)}}
	if err := audit.Record(ctx, tx, e); err != nil {
		return Outcome{}, err
	}
	for _, session := range ended {
		se := audit.Of(origin, audit.ActionUserSessionRevoke, audit.TargetSession, session)
		se.Reason = ReasonDenied
		if err := audit.Record(ctx, tx, se); err != nil {
			return Outcome{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Outcome{}, err
	}

	notice := confirm.Denial{Details: details, Cancelled: out.WithdrawalsCancelled, InProgress: out.WithdrawalsInProgress,
		HoldHours: int(s.Hold / time.Hour)}
	if denied {
		if err := s.Confirmations.NotifyDenied(ctx, userHK, notice); err != nil {
			// The account is secured; the page shows what was done.
			log.Printf("Failed to send the notice of a denial to user %s: %v", userHK, err)
		}
	}
	return out, nil
}
