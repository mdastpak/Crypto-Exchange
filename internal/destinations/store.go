package destinations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/verification"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Errors of the store. A refusal by the user's verification level is a
// *verification.Refusal, and one by the security hold a
// *restrictions.Refusal.
var (
	// ErrNotFound is no destination of the user with that ID, or a
	// removed one.
	ErrNotFound = errors.New("no such withdrawal destination")
	// ErrExists is a destination that the user's book has already.
	ErrExists = errors.New("the withdrawal destination is in the book already")
	// ErrTaken is a card, an IBAN, or an account that is in another
	// user's book.
	ErrTaken = errors.New("the card, IBAN, or account is another user's destination")
	// ErrTooMany is a book that holds the most destinations allowed.
	ErrTooMany = errors.New("the book holds the most withdrawal destinations allowed")
	// ErrNationalIDRequired is a fiat destination of a user whose national
	// ID is not verified, so that its owner cannot be checked.
	ErrNationalIDRequired = errors.New("a verified national ID is required")
	// ErrNotOwned is a card, an IBAN, or an account that the provider says
	// is not the user's.
	ErrNotOwned = errors.New("the card, IBAN, or account does not belong to the user")
	// ErrOwnershipUnavailable is a fiat destination whose owner cannot be
	// checked now: no provider, or one that does not answer.
	ErrOwnershipUnavailable = errors.New("the ownership of the card, IBAN, or account cannot be checked now")
	// ErrNoKeys is a store without data keys, which cannot encrypt the
	// numbers and addresses.
	ErrNoKeys = errors.New("the data-protection keys are not loaded")
	// ErrNotUsableYet is a destination still in its waiting period.
	ErrNotUsableYet = errors.New("the withdrawal destination is not usable yet")
	// ErrUnverified is a destination that does not match its seal: it
	// was changed outside the backend, and is not used.
	ErrUnverified = rowseal.ErrUnverified
)

// NotUsableYet is ErrNotUsableYet with the time the destination becomes
// usable.
type NotUsableYet struct{ Until time.Time }

func (e *NotUsableYet) Error() string {
	return ErrNotUsableYet.Error() + " until " + e.Until.UTC().Format(time.RFC3339)
}

func (e *NotUsableYet) Unwrap() error { return ErrNotUsableYet }

// Store keeps the withdrawal destinations of users.
type Store struct {
	DB *sql.DB
	// Settings are the limits and waiting periods of the destinations.
	Settings config.WithdrawalsConfig
	// Levels checks that the user's verification level allows the
	// withdrawals that a destination is for (#157).
	Levels verification.Store
	// Ownership checks the owner of a card, an IBAN, or an account. Nil
	// is no provider: fiat destinations cannot be added.
	Ownership identity.BankOwnership
	// OwnershipTimeout bounds one check of the owner.
	OwnershipTimeout time.Duration
	// NationalIDs opens the national IDs that the ownership is checked
	// against.
	NationalIDs identity.Sealer
}

// place is where the value or the memo of the destination id is sealed:
// its associated data.
func place(id, column string) field.Place {
	return field.Place{Table: "withdrawal_destinations", Column: column, Row: id}
}

// indexDomain is the domain of the blind indexes of the destinations of a
// kind.
func indexDomain(k Kind) string { return "withdrawal_destinations.value:" + string(k) }

const listed = `id::text, kind, COALESCE(label, ''), masked, COALESCE(bank, ''), COALESCE(network, ''), usable_from, created_at, row_version`

func scanListed(scan func(...any) error, d *Destination) error {
	return scan(&d.ID, &d.Kind, &d.Label, &d.Masked, &d.Bank, &d.Network, &d.UsableFrom, &d.CreatedAt, &d.Version)
}

// List returns the destinations of the user's book, the newest first. A
// destination that does not match its seal is left out in enforce mode,
// and its finding recorded.
func (s Store) List(ctx context.Context, userHK string) ([]Destination, error) {
	//nolint:gosec // G202: the columns are constants of this package and rowseal; the value is an argument.
	rows, err := s.DB.QueryContext(ctx, `SELECT `+listed+`, `+rowseal.WithdrawalDestinations.Fields("")+`
		FROM withdrawal_destinations WHERE user_hk = $1 AND removed_at IS NULL ORDER BY created_at DESC, id`, userHK)
	if err != nil {
		return nil, fmt.Errorf("list the withdrawal destinations: %w", err)
	}
	defer rows.Close()
	type read struct {
		d   Destination
		row *rowseal.Row
	}
	var all []read
	for rows.Next() {
		var r read
		r.row = rowseal.WithdrawalDestinations.NewRow()
		if err := scanListed(func(dest ...any) error { return rows.Scan(append(dest, r.row.Dest()...)...) }, &r.d); err != nil {
			return nil, fmt.Errorf("read a withdrawal destination: %w", err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the withdrawal destinations: %w", err)
	}
	out := make([]Destination, 0, len(all))
	for _, r := range all {
		if err := rowseal.Check(ctx, rowseal.WithdrawalDestinations, r.row); err != nil {
			if errors.Is(err, rowseal.ErrUnverified) {
				continue
			}
			return nil, err
		}
		out = append(out, r.d)
	}
	return out, nil
}

// Add adds a destination to the user's book, after the checks of the
// package comment, and records origin, the request's audit event, with
// the outcome. It returns the destination as the user sees it.
func (s Store) Add(ctx context.Context, userHK string, r Request, origin audit.Event) (Destination, error) {
	n, err := normalize(r)
	if err != nil {
		return Destination{}, err
	}
	cipher := field.Active()
	if cipher == nil {
		return Destination{}, ErrNoKeys
	}
	if err := s.Levels.Check(ctx, s.DB, userHK, verification.WithdrawAction(n.Kind.Fiat())); err != nil {
		return Destination{}, err
	}
	if err := restrictions.CheckSecurityHold(ctx, s.DB, userHK, restrictions.OpWithdraw); err != nil {
		return Destination{}, err
	}
	if err := s.checkBook(ctx, userHK, n); err != nil {
		return Destination{}, err
	}
	event := origin
	event.ActorType, event.ActorID = audit.ActorUser, userHK
	event.Action, event.TargetType = audit.ActionUserDestinationAdd, audit.TargetWithdrawalDestination
	after := map[string]interface{}{"kind": n.Kind, "masked": n.Masked}
	if n.Bank != "" {
		after["bank"] = n.Bank
	}
	if n.Network != "" {
		after["network"] = n.Network
	}
	var verifiedBy, verificationRef *string
	if n.Kind.Fiat() {
		match, provider, err := s.checkOwner(ctx, userHK, n)
		if err != nil {
			return Destination{}, err
		}
		after["ownership_provider"], after["ownership_ref"] = provider, match.Ref
		if !match.Matched {
			// No destination was made: the target is the user.
			event.TargetType, event.TargetID = audit.TargetUser, userHK
			event.Outcome, event.Reason = audit.Failure, "not_owned"
			event.Changes = &audit.Changes{After: after}
			if err := audit.Record(ctx, s.DB, event); err != nil {
				return Destination{}, err
			}
			return Destination{}, ErrNotOwned
		}
		verifiedBy, verificationRef = &provider, &match.Ref
	}
	return s.insert(ctx, userHK, n, cipher, verifiedBy, verificationRef, event, after)
}

// checkBook refuses a destination that the book has already, a fiat one
// in another user's book, and a book that is full. It compares the blind
// indexes of every loaded key version, so that a destination indexed
// before a rotation is still found.
func (s Store) checkBook(ctx context.Context, userHK string, n normalized) error {
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM withdrawal_destinations WHERE user_hk = $1 AND removed_at IS NULL`,
		userHK).Scan(&count); err != nil {
		return fmt.Errorf("count the withdrawal destinations: %w", err)
	}
	if count >= s.Settings.Destinations() {
		return ErrTooMany
	}
	var owner string
	err := s.DB.QueryRowContext(ctx, `SELECT user_hk::text FROM withdrawal_destinations
		WHERE kind = $1 AND value_bidx = ANY($2::bytea[]) AND removed_at IS NULL AND (kind <> 'crypto' OR user_hk = $3)
		ORDER BY user_hk = $3 DESC LIMIT 1`,
		n.Kind, field.Active().Indexes(indexDomain(n.Kind), n.Index), userHK).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("find the withdrawal destination: %w", err)
	case owner == userHK:
		return ErrExists
	}
	return ErrTaken
}

// checkOwner asks the provider whether the card, IBAN, or account belongs
// to the user's verified national ID.
func (s Store) checkOwner(ctx context.Context, userHK string, n normalized) (identity.Match, string, error) {
	// The national ID first: a user without one can do something about
	// it, and a user with one waits for the provider.
	nationalID, has, err := identity.NationalID(ctx, s.DB, s.NationalIDs, userHK)
	if err != nil {
		return identity.Match{}, "", fmt.Errorf("read the national ID: %w", err)
	}
	if !has {
		return identity.Match{}, "", ErrNationalIDRequired
	}
	if s.Ownership == nil {
		return identity.Match{}, "", ErrOwnershipUnavailable
	}
	timeout := s.OwnershipTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	match, err := s.Ownership.MatchBank(checkCtx, nationalID, identity.BankAccount{Kind: string(n.Kind), Number: n.Value, Bank: n.Bank})
	if err != nil {
		return identity.Match{}, "", fmt.Errorf("%w: %s: %w", ErrOwnershipUnavailable, s.Ownership.Provider(), err)
	}
	return match, s.Ownership.Provider(), nil
}

func (s Store) insert(ctx context.Context, userHK string, n normalized, cipher *field.Cipher, verifiedBy, verificationRef *string,
	event audit.Event, after map[string]interface{}) (Destination, error) {
	id := uuid.NewString()
	value, err := cipher.Seal(place(id, "value_enc"), n.Value)
	if err != nil {
		return Destination{}, err
	}
	index, err := cipher.Index(indexDomain(n.Kind), n.Index)
	if err != nil {
		return Destination{}, err
	}
	var memo []byte
	if n.Memo != "" {
		if memo, err = cipher.Seal(place(id, "memo_enc"), n.Memo); err != nil {
			return Destination{}, err
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Destination{}, err
	}
	defer tx.Rollback()
	// The additions of one user take turns, so two at once cannot both
	// find room for one more in a full list: the count is taken again
	// under the lock.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"withdrawal_destinations:"+userHK); err != nil {
		return Destination{}, fmt.Errorf("lock the withdrawal destinations of the user: %w", err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM withdrawal_destinations WHERE user_hk = $1 AND removed_at IS NULL`,
		userHK).Scan(&count); err != nil {
		return Destination{}, fmt.Errorf("count the withdrawal destinations: %w", err)
	}
	if count >= s.Settings.Destinations() {
		return Destination{}, ErrTooMany
	}
	var d Destination
	err = scanListed(tx.QueryRowContext(ctx, `INSERT INTO withdrawal_destinations
			(id, user_hk, kind, value_enc, value_bidx, masked, label, bank, network, memo_enc, verified_by, verification_ref, usable_from)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''), $10, $11, $12, NOW() + make_interval(secs => $13))
		RETURNING `+listed,
		id, userHK, n.Kind, value, index, n.Masked, n.Label, n.Bank, n.Network, memo, verifiedBy, verificationRef,
		s.Settings.DestinationWait(n.Kind.Fiat()).Seconds()).Scan, &d)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if n.Kind.Fiat() {
			return Destination{}, ErrTaken
		}
		return Destination{}, ErrExists
	}
	if err != nil {
		return Destination{}, fmt.Errorf("add the withdrawal destination: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.WithdrawalDestinations, "id = $1", id); err != nil {
		return Destination{}, err
	}
	d.Version++
	after["usable_from"] = d.UsableFrom.UTC().Format(time.RFC3339)
	event.TargetID, event.Outcome, event.Changes = id, audit.Success, &audit.Changes{After: after}
	if err := audit.Record(ctx, tx, event); err != nil {
		return Destination{}, err
	}
	if err := tx.Commit(); err != nil {
		return Destination{}, err
	}
	return d, nil
}

// Remove removes a destination from the user's book, and records origin
// with the outcome. A removed destination keeps its row, unusable.
func (s Store) Remove(ctx context.Context, userHK, id string, origin audit.Event) error {
	if _, err := uuid.Parse(id); err != nil {
		return ErrNotFound
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind, masked string
	err = tx.QueryRowContext(ctx, `UPDATE withdrawal_destinations SET removed_at = NOW()
		WHERE id = $1 AND user_hk = $2 AND removed_at IS NULL RETURNING kind, masked`, id, userHK).Scan(&kind, &masked)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("remove the withdrawal destination: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.WithdrawalDestinations, "id = $1", id); err != nil {
		return err
	}
	event := origin
	event.ActorType, event.ActorID = audit.ActorUser, userHK
	event.Action, event.TargetType, event.TargetID = audit.ActionUserDestinationRemove, audit.TargetWithdrawalDestination, id
	event.Outcome = audit.Success
	event.Changes = &audit.Changes{Before: map[string]interface{}{"kind": kind, "masked": masked}}
	if err := audit.Record(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit()
}

// Querier runs queries: a *sql.DB, or the *sql.Tx of a withdrawal request.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Usable returns a destination of the user's book that a withdrawal may
// use now, with its number or address in the clear, for the withdrawal
// workflow (#70). It refuses a destination that is not the user's, is
// removed, does not match its seal in enforce mode (ErrUnverified), or is
// still waiting (*NotUsableYet). A withdrawal request calls it in its own
// transaction, with FOR SHARE, so that the destination cannot be removed
// before the request commits.
func (s Store) Usable(ctx context.Context, q Querier, userHK, id string) (Resolved, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Resolved{}, ErrNotFound
	}
	var r Resolved
	var value, memo []byte
	var usable bool
	row := rowseal.WithdrawalDestinations.NewRow()
	err := q.QueryRowContext(ctx, `SELECT `+listed+`, value_enc, memo_enc, usable_from <= NOW(), `+rowseal.WithdrawalDestinations.Fields("")+`
		FROM withdrawal_destinations WHERE id = $1 AND user_hk = $2 AND removed_at IS NULL FOR SHARE`, id, userHK).
		Scan(append([]any{&r.ID, &r.Kind, &r.Label, &r.Masked, &r.Bank, &r.Network, &r.UsableFrom, &r.CreatedAt, &r.Version,
			&value, &memo, &usable}, row.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Resolved{}, ErrNotFound
	}
	if err != nil {
		return Resolved{}, fmt.Errorf("read the withdrawal destination: %w", err)
	}
	if err := rowseal.Check(ctx, rowseal.WithdrawalDestinations, row); err != nil {
		return Resolved{}, err
	}
	if !usable {
		return Resolved{}, &NotUsableYet{Until: r.UsableFrom}
	}
	cipher := field.Active()
	if cipher == nil {
		return Resolved{}, ErrNoKeys
	}
	if r.Value, err = cipher.Open(place(r.ID, "value_enc"), value); err != nil {
		return Resolved{}, fmt.Errorf("open the withdrawal destination: %w", err)
	}
	if memo != nil {
		if r.Memo, err = cipher.Open(place(r.ID, "memo_enc"), memo); err != nil {
			return Resolved{}, fmt.Errorf("open the memo of the withdrawal destination: %w", err)
		}
	}
	if err := revalidate(r); err != nil {
		return Resolved{}, err
	}
	return r, nil
}

// revalidate checks the number or address of a destination again, as
// normalize checked it when it was added: a card's Luhn digit, an IBAN's
// check digits, an account's digits and bank, an address for its network.
// Nothing stores one that fails, so a failure marks a value written outside
// the backend, and the withdrawal is refused rather than sent.
func revalidate(r Resolved) error {
	req := Request{Kind: r.Kind, Number: r.Value, Bank: r.Bank}
	if r.Kind == KindCrypto {
		req = Request{Kind: r.Kind, Network: r.Network, Address: r.Value, Memo: r.Memo}
	}
	if r.Kind != KindAccount {
		req.Bank = ""
	}
	n, err := normalize(req)
	if err != nil || n.Value != r.Value {
		return fmt.Errorf("%w: the stored number or address of withdrawal destination %s is not valid", ErrUnverified, r.ID)
	}
	return nil
}
