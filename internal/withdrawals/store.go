package withdrawals

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/google/uuid"
)

// querier reads in a transaction or out of one.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// columns are the columns of a Withdrawal, with its seal, under the alias
// w, with its currency c and destination d.
var columns = `w.id, w.user_id, w.currency_hk, c.info->>'symbol', w.network, w.destination_id, d.masked, w.amount, w.fee,
	w.status, COALESCE(w.review_reason, ''), COALESCE(w.reason, ''), w.reviewed_at, w.created_at, w.updated_at,
	w.completed_at, w.cancelled_at, ` + rowseal.Withdrawals.Fields("w")

const from = ` FROM withdrawals w JOIN currencies c ON c.hk = w.currency_hk
	JOIN withdrawal_destinations d ON d.id = w.destination_id`

// scan reads a row of columns. The withdrawal is returned with its seal,
// which the caller checks.
func scan(scanFn func(...any) error) (Withdrawal, *rowseal.Row, error) {
	var w Withdrawal
	sealed := rowseal.Withdrawals.NewRow()
	var status string
	err := scanFn(append([]any{&w.ID, &w.UserHK, &w.CurrencyHK, &w.Currency, &w.Network, &w.DestinationID, &w.Destination,
		&w.Amount, &w.Fee, &status, &w.ReviewReason, &w.Reason, &w.ReviewedAt, &w.CreatedAt, &w.UpdatedAt,
		&w.CompletedAt, &w.CancelledAt}, sealed.Dest()...)...)
	w.Status = Status(status)
	w.Total = w.Amount.Add(w.Fee)
	return w, sealed, err
}

// read returns the withdrawal where selects, which must be one, and checks
// its seal: rowseal.ErrUnverified in enforce mode for one changed in SQL.
func read(ctx context.Context, q querier, where string, args ...any) (Withdrawal, error) {
	return readLocked(ctx, q, where, false, args...)
}

func readLocked(ctx context.Context, q querier, where string, lock bool, args ...any) (Withdrawal, error) {
	query := `SELECT ` + columns + from + ` WHERE ` + where
	if lock {
		query += ` FOR UPDATE OF w`
	}
	w, sealed, err := scan(q.QueryRowContext(ctx, query, args...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return w, ErrNotFound
	}
	if err != nil {
		return w, err
	}
	return w, rowseal.Check(ctx, rowseal.Withdrawals, sealed)
}

// Get returns a withdrawal of the user.
func (s Service) Get(ctx context.Context, userHK, id string) (Withdrawal, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Withdrawal{}, ErrNotFound
	}
	w, err := read(ctx, s.DB, `w.id = $1 AND w.user_id = $2`, id, userHK)
	if err != nil {
		return w, err
	}
	w.UserHK = ""
	w.Payouts, err = payoutsOf(ctx, s.DB, w.ID)
	return w, err
}

// MaxList bounds a list of withdrawals.
const MaxList = 100

// List returns the user's withdrawals, newest first, at most limit, and
// only those created before before, when it is set. A withdrawal that
// fails its seal in enforce mode is left out.
func (s Service) List(ctx context.Context, userHK string, before *time.Time, limit int) ([]Withdrawal, error) {
	list, err := s.list(ctx, `w.user_id = $1 AND ($2::timestamptz IS NULL OR w.created_at < $2)`, false, limit, userHK, before)
	for i := range list {
		list[i].UserHK = ""
	}
	return list, err
}

// Review returns the withdrawals that wait for an administrator, oldest
// first, at most limit; or, with status, those of the status, newest
// first. One that fails its seal in enforce mode makes it
// rowseal.ErrUnverified, as the other views of administrators do.
func (s Service) Review(ctx context.Context, status Status, limit int) ([]Withdrawal, error) {
	if status == "" {
		status = StatusReview
	}
	order := `DESC`
	if status == StatusReview {
		order = `ASC`
	}
	return s.listOrdered(ctx, `w.status = $1`, true, order, limit, string(status))
}

func (s Service) list(ctx context.Context, where string, strict bool, limit int, args ...any) ([]Withdrawal, error) {
	return s.listOrdered(ctx, where, strict, `DESC`, limit, args...)
}

func (s Service) listOrdered(ctx context.Context, where string, strict bool, order string, limit int, args ...any) ([]Withdrawal, error) {
	if limit <= 0 || limit > MaxList {
		limit = MaxList
	}
	//nolint:gosec // G202: the columns, the conditions, and the order are constants of this package; the values are arguments.
	rows, err := s.DB.QueryContext(ctx, `SELECT `+columns+from+` WHERE `+where+`
		ORDER BY w.created_at `+order+`, w.id `+order+` LIMIT `+fmt.Sprint(limit), args...)
	if err != nil {
		return nil, err
	}
	var all []Withdrawal
	var seals []*rowseal.Row
	for rows.Next() {
		w, sealed, err := scan(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		all, seals = append(all, w), append(seals, sealed)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	list := []Withdrawal{}
	for i, w := range all {
		if err := rowseal.Check(ctx, rowseal.Withdrawals, seals[i]); err != nil {
			if strict {
				return nil, err
			}
			continue
		}
		list = append(list, w)
	}
	return list, nil
}

// Cancel cancels a withdrawal of the user whose payouts have not started,
// and releases its funds.
func (s Service) Cancel(ctx context.Context, userHK, id string, origin audit.Event) (Withdrawal, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Withdrawal{}, ErrNotFound
	}
	origin.ActorType, origin.ActorID = audit.ActorUser, userHK
	w, err := s.settle(ctx, `w.id = $1 AND w.user_id = $2`, []any{id, userHK}, func(w Withdrawal) error {
		if !w.Status.Cancellable() {
			return ErrNotCancellable
		}
		return nil
	}, StatusCancelled, "", "", origin, audit.ActionUserWithdrawalCancel)
	w.UserHK = ""
	return w, err
}

// Approve lets a withdrawal held for review go to its payouts, as the
// administrator adminHK.
func (s Service) Approve(ctx context.Context, id, adminHK string, origin audit.Event) (Withdrawal, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Withdrawal{}, ErrNotFound
	}
	return s.settle(ctx, `w.id = $1`, []any{id}, inReview, StatusQueued, adminHK, "", origin, audit.ActionWithdrawalApprove)
}

// MaxReason bounds the reason of a rejection.
const MaxReason = 500

// Reject refuses a withdrawal held for review, as the administrator
// adminHK, and releases its funds; the user sees reason.
func (s Service) Reject(ctx context.Context, id, adminHK, reason string, origin audit.Event) (Withdrawal, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Withdrawal{}, ErrNotFound
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > MaxReason {
		return Withdrawal{}, invalid("reason", "is required, at most %d characters", MaxReason)
	}
	return s.settle(ctx, `w.id = $1`, []any{id}, inReview, StatusRejected, adminHK, reason, origin, audit.ActionWithdrawalReject)
}

func inReview(w Withdrawal) error {
	if w.Status != StatusReview {
		return ErrNotInReview
	}
	return nil
}

// settle moves the withdrawal where selects to status, after allowed
// accepts it, with the audit event of action. A cancelled or rejected one
// releases its funds in the same transaction. adminHK is the reviewer of
// an approval or a rejection.
func (s Service) settle(ctx context.Context, where string, args []any, allowed func(Withdrawal) error, status Status,
	adminHK, reason string, origin audit.Event, action string) (Withdrawal, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Withdrawal{}, err
	}
	defer tx.Rollback()
	// A withdrawal changed in SQL is not settled until its finding is
	// resolved: its funds stay locked.
	w, err := readLocked(ctx, tx, where, true, args...)
	if err != nil {
		return Withdrawal{}, err
	}
	if err := allowed(w); err != nil {
		return Withdrawal{}, err
	}
	release := status == StatusCancelled || status == StatusRejected
	_, err = tx.ExecContext(ctx, `UPDATE withdrawals SET status = $2, reason = NULLIF($3, ''),
			reviewed_by = COALESCE(NULLIF($4, '')::uuid, reviewed_by),
			reviewed_at = CASE WHEN $4 <> '' THEN NOW() ELSE reviewed_at END,
			cancelled_at = CASE WHEN $5 THEN NOW() ELSE cancelled_at END
		WHERE id = $1`, w.ID, string(status), reason, adminHK, release)
	if err != nil {
		return Withdrawal{}, fmt.Errorf("settle the withdrawal: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.Withdrawals, "id = $1", w.ID); err != nil {
		return Withdrawal{}, err
	}
	if release {
		if _, err := s.Ledger.In(tx).Release(ctx, "withdrawal:"+w.ID+":release", account(w.UserHK),
			ledger.Amount{Currency: uuidOf(w.CurrencyHK), Value: w.Total}, ledger.Ref{Type: ledger.RefWithdrawal, ID: w.ID}); err != nil {
			return Withdrawal{}, err
		}
	}
	e := audit.Of(origin, action, audit.TargetWithdrawal, w.ID)
	after := map[string]any{"status": string(status)}
	if reason != "" {
		after["reason"] = reason
	}
	e.Changes = &audit.Changes{Before: map[string]any{"status": string(w.Status)}, After: after}
	if err := audit.Record(ctx, tx, e); err != nil {
		return Withdrawal{}, err
	}
	w, err = read(ctx, tx, `w.id = $1`, w.ID)
	if err != nil {
		return Withdrawal{}, err
	}
	return w, tx.Commit()
}
