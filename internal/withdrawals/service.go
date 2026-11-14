package withdrawals

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/confirm"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/domain/currency"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Confirmer starts and consumes the confirmations of the second factor
// (#161): confirm.Service.
type Confirmer interface {
	Start(ctx context.Context, userHK string, p confirm.Purpose, digest []byte, details confirm.Details, client string) (confirm.Challenge, error)
	Consume(ctx context.Context, tx *sql.Tx, userHK, id string, p confirm.Purpose, digest []byte) error
}

// DestinationBook returns a destination of a user's book that a withdrawal
// may use: destinations.Store.
type DestinationBook interface {
	Usable(ctx context.Context, q destinations.Querier, userHK, id string) (destinations.Resolved, error)
}

// LevelLimits checks a withdrawal against the limits of the user's level
// (#157), in the transaction of the request: verification.Store.
type LevelLimits interface {
	CheckWithdrawal(ctx context.Context, tx *sql.Tx, userHK, currencyHK string, amount shared.Decimal) error
}

// Service runs the withdrawals.
type Service struct {
	DB           *sql.DB
	Confirm      Confirmer
	Destinations DestinationBook
	Levels       LevelLimits
	Ledger       ledger.Ledger
	// ReviewAbove holds, by currency symbol, the amount from which a
	// withdrawal waits for an administrator.
	ReviewAbove map[string]shared.Decimal
}

// Request asks for a withdrawal of Amount of the currency CurrencyHK, on
// Network, to the destination DestinationID of the user's book.
type Request struct {
	CurrencyHK    string
	Network       string
	DestinationID string
	Amount        shared.Decimal
	// ConfirmationID is the confirmation of the request, once the user
	// confirmed it; empty starts one.
	ConfirmationID string
	// IdempotencyKey names the request: the same key returns the same
	// withdrawal.
	IdempotencyKey string
}

// Outcome is the answer to a request: a confirmation to confirm, or the
// withdrawal. Replayed reports a request that ran before with its key.
type Outcome struct {
	Challenge  *confirm.Challenge
	Withdrawal *Withdrawal
	Replayed   bool
}

// idempotencyKey is the form of a key: what an HTTP header carries well.
var idempotencyKey = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,100}$`)

func (r Request) validate() error {
	problems := Invalid{}
	if !idempotencyKey.MatchString(r.IdempotencyKey) {
		problems["idempotency_key"] = "must be 1 to 100 letters, digits, dots, colons, underscores, or hyphens"
	}
	for field, value := range map[string]string{"currency_hk": r.CurrencyHK, "destination_id": r.DestinationID} {
		if _, err := uuid.Parse(value); err != nil {
			problems[field] = "must be a UUID"
		}
	}
	if r.ConfirmationID != "" {
		if _, err := uuid.Parse(r.ConfirmationID); err != nil {
			problems["confirmation_id"] = "must be a UUID"
		}
	}
	if r.Network == "" || len(r.Network) > 50 {
		problems["network"] = "is required"
	}
	if !r.Amount.IsPositive() {
		problems["amount"] = "must be positive"
	}
	if len(problems) > 0 {
		return problems
	}
	return nil
}

// digest is what a confirmation of the request is bound to (#161): its
// currency, network, amount, destination, and key. A retry of the key
// compares it too.
func (r Request) digest() []byte {
	return confirm.Digest(confirm.PurposeWithdrawal, strings.ToLower(r.CurrencyHK), r.Network, r.Amount.String(),
		strings.ToLower(r.DestinationID), r.IdempotencyKey)
}

// plan is a request as it would run.
type plan struct {
	currency    models.Currency
	network     currency.Network
	destination destinations.Resolved
	fee, total  shared.Decimal
	details     confirm.Details
	review      string
}

// Request asks for a withdrawal of the user. Without a confirmation, it
// checks the request and starts its confirmation, which goes to the user
// by their method. With a confirmed one, it runs the withdrawal, and
// returns it. client is the requester, for the limits of the codes.
func (s Service) Request(ctx context.Context, userHK string, r Request, client string, origin audit.Event) (Outcome, error) {
	if err := r.validate(); err != nil {
		return Outcome{}, err
	}
	digest := r.digest()
	if w, err := s.replay(ctx, s.DB, userHK, r.IdempotencyKey, digest); w != nil || err != nil {
		return Outcome{Withdrawal: w, Replayed: true}, err
	}
	if r.ConfirmationID == "" {
		p, err := s.dryRun(ctx, userHK, r)
		if err != nil {
			return Outcome{}, err
		}
		challenge, err := s.Confirm.Start(ctx, userHK, confirm.PurposeWithdrawal, digest, p.details, client)
		if err != nil {
			return Outcome{}, err
		}
		return Outcome{Challenge: &challenge}, nil
	}
	out, err := s.run(ctx, userHK, r, digest, origin)
	// Two requests of the key at once both pass the lock of the user,
	// which is shared. The second finds the confirmation consumed, or the
	// key taken, once the first commits: it is a retry of the first.
	if errors.Is(err, confirm.ErrConsumed) || isConstraint(err, "withdrawals_user_key") {
		if w, again := s.replay(ctx, s.DB, userHK, r.IdempotencyKey, digest); w != nil && again == nil {
			return Outcome{Withdrawal: w, Replayed: true}, nil
		}
	}
	return out, err
}

// isConstraint reports a violation of the unique constraint name.
func isConstraint(err error, name string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == name
}

// dryRun checks the request in a transaction that it rolls back, so that
// no code is sent for a withdrawal that would be refused.
func (s Service) dryRun(ctx context.Context, userHK string, r Request) (plan, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return plan{}, err
	}
	defer tx.Rollback()
	return s.check(ctx, tx, userHK, r, false)
}

// run records the withdrawal and locks its funds, in one transaction.
func (s Service) run(ctx context.Context, userHK string, r Request, digest []byte, origin audit.Event) (Outcome, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Outcome{}, err
	}
	defer tx.Rollback()
	// One request of the user at a time: the second waits, and then finds
	// the first by its key, or counts it in the limits.
	if err := restrictions.Lock(ctx, tx, userHK); err != nil {
		return Outcome{}, err
	}
	if w, err := s.replay(ctx, tx, userHK, r.IdempotencyKey, digest); w != nil || err != nil {
		return Outcome{Withdrawal: w, Replayed: true}, err
	}
	p, err := s.check(ctx, tx, userHK, r, true)
	if err != nil {
		return Outcome{}, err
	}
	if err := s.Confirm.Consume(ctx, tx, userHK, r.ConfirmationID, confirm.PurposeWithdrawal, digest); err != nil {
		return Outcome{}, err
	}
	status := StatusQueued
	if p.review != "" {
		status = StatusReview
	}
	var id string
	err = tx.QueryRowContext(ctx, `INSERT INTO withdrawals (user_id, currency_hk, network, destination_id, amount, fee, status,
			idempotency_key, request_digest, confirmation_id, review_reason)
		VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric, $7, $8, $9, $10, NULLIF($11, '')) RETURNING id`,
		userHK, r.CurrencyHK, r.Network, r.DestinationID, r.Amount.String(), p.fee.String(), string(status),
		r.IdempotencyKey, digest, r.ConfirmationID, p.review).Scan(&id)
	if err != nil {
		return Outcome{}, fmt.Errorf("record the withdrawal: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.Withdrawals, "id = $1", id); err != nil {
		return Outcome{}, err
	}
	if _, err := s.Ledger.In(tx).Lock(ctx, "withdrawal:"+id+":lock", account(userHK),
		ledger.Amount{Currency: uuidOf(r.CurrencyHK), Value: p.total}, ledger.Ref{Type: ledger.RefWithdrawal, ID: id}); err != nil {
		return Outcome{}, err
	}
	origin.ActorType, origin.ActorID = audit.ActorUser, userHK
	e := audit.Of(origin, audit.ActionUserWithdrawalRequest, audit.TargetWithdrawal, id)
	e.Changes = &audit.Changes{After: map[string]any{
		"currency": p.currency.Info.Symbol, "network": r.Network, "destination": p.destination.Masked,
		"amount": r.Amount.String(), "fee": p.fee.String(), "status": string(status),
	}}
	if err := audit.Record(ctx, tx, e); err != nil {
		return Outcome{}, err
	}
	w, err := read(ctx, tx, `w.id = $1`, id)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Withdrawal: &w}, tx.Commit()
}

// replay returns the withdrawal that a request with the key made, when
// there is one: ErrIdempotencyConflict if it asked for another.
func (s Service) replay(ctx context.Context, q querier, userHK, key string, digest []byte) (*Withdrawal, error) {
	var id string
	var stored []byte
	err := q.QueryRowContext(ctx, `SELECT id, request_digest FROM withdrawals WHERE user_id = $1 AND idempotency_key = $2`,
		userHK, key).Scan(&id, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(stored, digest) != 1 {
		return nil, ErrIdempotencyConflict
	}
	w, err := read(ctx, q, `w.id = $1`, id)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// check checks a request in tx, as it would run. With lock, the network
// is share-locked, so that it does not change before the withdrawal
// commits. The destination always is.
func (s Service) check(ctx context.Context, tx *sql.Tx, userHK string, r Request, lock bool) (plan, error) {
	var p plan
	if err := restrictions.Check(ctx, tx, userHK, restrictions.OpWithdraw, r.CurrencyHK); err != nil {
		return p, err
	}
	cur, err := currencies.Detail(ctx, tx, r.CurrencyHK)
	if errors.Is(err, currencies.ErrNotFound) || (err == nil && (cur.DeletedAt != nil || cur.Status != models.CurrencyStatusApproved)) {
		return p, invalid("currency_hk", "is not a listed currency")
	}
	if err != nil {
		return p, err
	}
	if cur.Lifecycle != nil && !cur.Lifecycle.Capabilities.Withdraw {
		return p, ErrClosed
	}
	p.currency = cur

	n, err := currencies.NetworkOf(ctx, tx, r.CurrencyHK, r.Network, lock)
	if errors.Is(err, currencies.ErrNetworkNotFound) {
		return p, invalid("network", "is not a network of %s", cur.Info.Symbol)
	}
	if err != nil {
		return p, err
	}
	if !n.WithdrawalEnabled {
		return p, ErrClosed
	}
	if !n.RoundQuantity(r.Amount).Equal(r.Amount) {
		return p, invalid("amount", "has more than %d decimals", n.Decimals)
	}
	switch err := n.ValidateWithdrawal(r.Amount); {
	case errors.Is(err, currency.ErrWithdrawalBelowMinimum):
		return p, invalid("amount", "is below the minimum of %s", n.MinWithdrawal)
	case errors.Is(err, currency.ErrWithdrawalAboveMaximum):
		return p, invalid("amount", "is above the maximum of %s", n.MaxWithdrawal)
	case err != nil:
		return p, invalid("amount", "%v", err)
	}
	p.network = n

	d, err := s.Destinations.Usable(ctx, tx, userHK, r.DestinationID)
	if errors.Is(err, destinations.ErrNotFound) {
		return p, invalid("destination_id", "is not a destination of your book")
	}
	if err != nil {
		return p, err
	}
	switch {
	case d.Fiat() != cur.Info.IfFiat:
		return p, invalid("destination_id", "is a %s destination, not one for %s", d.Kind, cur.Info.Symbol)
	case !d.Fiat() && d.Network != r.Network:
		return p, invalid("destination_id", "is an address on %s, not on %s", d.Network, r.Network)
	}
	p.destination = d

	p.fee = n.WithdrawalFee
	p.total = r.Amount.Add(p.fee)
	if err := s.Levels.CheckWithdrawal(ctx, tx, userHK, r.CurrencyHK, p.total); err != nil {
		return p, err
	}
	b, err := s.Ledger.Balance(ctx, account(userHK), uuidOf(r.CurrencyHK))
	if err != nil {
		return p, err
	}
	if b.Available.LessThan(p.total) {
		return p, ErrInsufficientBalance
	}

	p.details = confirm.Details{Amount: r.Amount.String() + " " + cur.Info.Symbol, Destination: shortDestination(d)}
	if utf8.RuneCountInString(p.details.Amount) > confirm.MaxDetailLength {
		return p, invalid("amount", "has too many digits for the confirmation message")
	}
	if threshold, ok := s.ReviewAbove[cur.Info.Symbol]; ok && r.Amount.GreaterThanOrEqual(threshold) {
		p.review = ReviewAmount
	}
	return p, nil
}

// shortDestination is the destination as the confirmation message shows
// it, within the length of an SMS parameter: masked, or its start and end.
func shortDestination(d destinations.Resolved) string {
	if utf8.RuneCountInString(d.Masked) <= confirm.MaxDetailLength {
		return d.Masked
	}
	v := d.Value
	if len(v) <= 10 {
		return v
	}
	return v[:6] + "..." + v[len(v)-4:]
}

// account is the main account of a user in the ledger.
func account(userHK string) ledger.Account { return ledger.Account{UserHK: uuidOf(userHK)} }

func uuidOf(s string) shared.UUID {
	id, _ := shared.UUIDFromString(s)
	return id
}
