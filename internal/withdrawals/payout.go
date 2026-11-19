package withdrawals

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// PayoutStatus is where a payout is.
type PayoutStatus string

// The statuses of a payout.
const (
	// PayoutPlanned waits for its day, and for the hours of its rail.
	PayoutPlanned PayoutStatus = "planned"
	// PayoutSubmitted was handed to the provider, which pays it or
	// reports it.
	PayoutSubmitted PayoutStatus = "submitted"
	// PayoutCompleted is paid: its amount and fee left the user's balance.
	PayoutCompleted PayoutStatus = "completed"
	// PayoutFailed failed, and its amount went to another rail.
	PayoutFailed PayoutStatus = "failed"
	// PayoutReleased failed, and its amount and fee were released to the
	// user.
	PayoutReleased PayoutStatus = "released"
)

// Payout is a transfer that pays a withdrawal, or a part of it.
type Payout struct {
	ID           string `json:"id" format:"uuid"`
	WithdrawalID string `json:"withdrawal_id" format:"uuid"`
	// Seq orders the payouts of a withdrawal.
	Seq  int    `json:"seq" example:"1"`
	Rail string `json:"rail" enums:"card,paya,satna,pol,account,chain" example:"paya"`
	// Amount is what the destination receives by this payout, and Fee the
	// share of the withdrawal's fee that it charges.
	Amount shared.Decimal `json:"amount" swaggertype:"string" example:"100000000"`
	Fee    shared.Decimal `json:"fee" swaggertype:"string" example:"0"`
	Status PayoutStatus   `json:"status" enums:"planned,submitted,completed,failed,released" example:"submitted"`
	// ScheduledFor is when it may be sent: a later day holds what the
	// daily limits do not allow today.
	ScheduledFor time.Time `json:"scheduled_for"`
	// TrackingID is the bank's tracking number, or the hash of the
	// transaction on chain.
	TrackingID  string     `json:"tracking_id,omitempty"`
	SubmittedAt *time.Time `json:"submitted_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	FailedAt    *time.Time `json:"failed_at,omitempty"`

	// The fields of the administrators' views.
	Provider       string `json:"provider,omitempty" example:"manual"`
	Attempts       int    `json:"attempts,omitempty"`
	LastError      string `json:"last_error,omitempty"`
	UserHK         string `json:"user_hk,omitempty" format:"uuid"`
	Currency       string `json:"currency,omitempty" example:"IRR"`
	Destination    string `json:"destination,omitempty" example:"IR82054***************9002"`
	DestinationKey string `json:"-"`
	Kind           string `json:"destination_kind,omitempty" enums:"card,iban,account,crypto"`
	Bank           string `json:"bank,omitempty" example:"mellat"`
	Network        string `json:"network,omitempty" example:"internal"`

	idempotencyKey string
	providerRef    string
	currencyHK     string
}

// public is the payout as its user sees it.
func (p Payout) public() Payout {
	return Payout{ID: p.ID, WithdrawalID: p.WithdrawalID, Seq: p.Seq, Rail: p.Rail, Amount: p.Amount, Fee: p.Fee,
		Status: p.Status, ScheduledFor: p.ScheduledFor, TrackingID: p.TrackingID, SubmittedAt: p.SubmittedAt,
		CompletedAt: p.CompletedAt, FailedAt: p.FailedAt}
}

// PayoutDetail is a payout with its destination in the clear, which an
// administrator pays by hand.
type PayoutDetail struct {
	Payout
	// DestinationValue is the card number, the IBAN, the account number,
	// or the address, and DestinationMemo the memo of an address.
	DestinationValue string `json:"destination_value" example:"IR820540102680020817909002"`
	DestinationMemo  string `json:"destination_memo,omitempty"`
}

// Transfer is a payout as its provider receives it.
type Transfer struct {
	PayoutID string
	// IdempotencyKey names the transfer at the provider, so that a retry
	// never pays twice.
	IdempotencyKey string
	// Ref is the provider's reference, once it gave one.
	Ref         string
	Rail        string
	Currency    string
	Amount      shared.Decimal
	Destination destinations.Resolved
	// Network, Contract, and Decimals are those of a payout on chain: the
	// token's contract, empty for the native asset, and its precision.
	Network  string
	Contract string
	Decimals int
}

// The states of a transfer at its provider.
const (
	TransferPending     = "pending"
	TransferCompleted   = "completed"
	TransferFailed      = "failed"
	TransferNotReceived = "not_received"
)

// TransferState is where a transfer is, by its provider.
type TransferState struct {
	// Status is one of the Transfer constants. TransferNotReceived is a
	// transfer that the provider never got, which may be sent again.
	Status     string
	TrackingID string
	Reason     string
}

// ErrTransferRefused is a transfer that the provider refused for good: the
// payout fails, and its amount goes to another rail or back to the user.
// Any other error of Submit leaves the result unknown, which Status
// settles before anything is sent again.
var ErrTransferRefused = errors.New("the provider refused the transfer")

// Provider pays the transfers of withdrawals.
type Provider interface {
	Name() string
	// Submit hands t to the provider, and returns its reference.
	Submit(ctx context.Context, t Transfer) (string, error)
	// Status returns where t is.
	Status(ctx context.Context, t Transfer) (TransferState, error)
}

// Manual is the provider of an administrator who pays each transfer by
// hand, by bank or on chain, and records it with its tracking number
// (POST /admin/payouts/{id}/complete), or its failure. It is the provider
// until the APIs of the banks and payment providers are added.
type Manual struct{}

var _ Provider = Manual{}

// Name is manual.
func (Manual) Name() string { return config.PayoutProviderManual }

// Submit takes the transfer: an administrator pays it.
func (Manual) Submit(_ context.Context, t Transfer) (string, error) { return t.PayoutID, nil }

// Status is pending until an administrator records the transfer.
func (Manual) Status(context.Context, Transfer) (TransferState, error) {
	return TransferState{Status: TransferPending}, nil
}

// payoutColumns are the columns of a Payout, with its withdrawal w, its
// currency c, its destination d, and its seal, under the alias p.
var payoutColumns = `p.id, p.withdrawal_id, p.seq, p.rail, p.provider, p.amount, p.fee, p.status, p.scheduled_for,
	COALESCE(p.tracking_id, ''), p.submitted_at, p.completed_at, p.failed_at, p.attempts, COALESCE(p.last_error, ''),
	p.idempotency_key, COALESCE(p.provider_ref, ''), w.user_id, w.currency_hk, c.info->>'symbol', d.masked, d.kind,
	COALESCE(d.bank, ''), w.network, w.destination_id, ` + rowseal.WithdrawalPayouts.Fields("p")

const payoutFrom = ` FROM withdrawal_payouts p JOIN withdrawals w ON w.id = p.withdrawal_id
	JOIN currencies c ON c.hk = w.currency_hk JOIN withdrawal_destinations d ON d.id = w.destination_id`

func scanPayout(scanFn func(...any) error) (Payout, *rowseal.Row, error) {
	var p Payout
	sealed := rowseal.WithdrawalPayouts.NewRow()
	var status string
	err := scanFn(append([]any{&p.ID, &p.WithdrawalID, &p.Seq, &p.Rail, &p.Provider, &p.Amount, &p.Fee, &status,
		&p.ScheduledFor, &p.TrackingID, &p.SubmittedAt, &p.CompletedAt, &p.FailedAt, &p.Attempts, &p.LastError,
		&p.idempotencyKey, &p.providerRef, &p.UserHK, &p.currencyHK, &p.Currency, &p.Destination, &p.Kind, &p.Bank,
		&p.Network, &p.DestinationKey}, sealed.Dest()...)...)
	p.Status = PayoutStatus(status)
	return p, sealed, err
}

// readPayout returns the payout where selects, checked against its seal.
// With lock, it locks the payout, and q is a transaction.
func readPayout(ctx context.Context, q querier, where string, lock bool, args ...any) (Payout, error) {
	query := `SELECT ` + payoutColumns + payoutFrom + ` WHERE ` + where
	if lock {
		query += ` FOR UPDATE OF p`
	}
	p, sealed, err := scanPayout(q.QueryRowContext(ctx, query, args...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrPayoutNotFound
	}
	if err != nil {
		return p, err
	}
	return p, rowseal.Check(ctx, rowseal.WithdrawalPayouts, sealed)
}

// payoutsOf returns the payouts of a withdrawal, as its user sees them. A
// payout that fails its seal is left out in enforce mode.
func payoutsOf(ctx context.Context, q querier, withdrawalID string) ([]Payout, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+payoutColumns+payoutFrom+` WHERE p.withdrawal_id = $1 ORDER BY p.seq`, withdrawalID)
	if err != nil {
		return nil, err
	}
	var all []Payout
	var seals []*rowseal.Row
	for rows.Next() {
		p, sealed, err := scanPayout(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		all, seals = append(all, p), append(seals, sealed)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	list := []Payout{}
	for i, p := range all {
		if rowseal.Check(ctx, rowseal.WithdrawalPayouts, seals[i]) != nil {
			continue
		}
		list = append(list, p.public())
	}
	return list, nil
}

// Errors of the payouts.
var (
	// ErrPayoutNotFound is a payout that does not exist.
	ErrPayoutNotFound = errors.New("payout not found")
	// ErrPayoutNotOpen refuses to complete or fail a payout that is not
	// planned or submitted.
	ErrPayoutNotOpen = errors.New("the payout is not planned or submitted")
	// ErrPayoutAutomatic refuses an administrator who records a payout
	// that its provider pays and reports by itself.
	ErrPayoutAutomatic = errors.New("the payout is paid by its provider, which reports it")
)

// List returns the payouts of status, oldest first, at most limit, for
// the administrators: submitted by default, the ones that the manual
// provider waits on. One that fails its seal makes it
// rowseal.ErrUnverified, as the other views of administrators do.
func (p Payouts) List(ctx context.Context, status PayoutStatus, limit int) ([]Payout, error) {
	if status == "" {
		status = PayoutSubmitted
	}
	if limit <= 0 || limit > MaxList {
		limit = MaxList
	}
	//nolint:gosec // G202: the columns, the condition, and the limit are of this package; the value is an argument.
	rows, err := p.DB.QueryContext(ctx, `SELECT `+payoutColumns+payoutFrom+` WHERE p.status = $1
		ORDER BY p.scheduled_for, p.seq LIMIT `+fmt.Sprint(limit), string(status))
	if err != nil {
		return nil, err
	}
	var list []Payout
	var seals []*rowseal.Row
	for rows.Next() {
		pay, sealed, err := scanPayout(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list, seals = append(list, pay), append(seals, sealed)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, sealed := range seals {
		if err := rowseal.Check(ctx, rowseal.WithdrawalPayouts, sealed); err != nil {
			return nil, err
		}
	}
	if list == nil {
		list = []Payout{}
	}
	return list, nil
}

// Detail returns a payout with its destination in the clear, for an
// administrator who pays it by hand. The reading is audited, as the
// administrator from origin.
func (p Payouts) Detail(ctx context.Context, id string, origin audit.Event) (PayoutDetail, error) {
	if _, err := uuid.Parse(id); err != nil {
		return PayoutDetail{}, ErrPayoutNotFound
	}
	pay, err := readPayout(ctx, p.DB, `p.id = $1`, false, id)
	if err != nil {
		return PayoutDetail{}, err
	}
	d, err := p.Destinations.Resolve(ctx, p.DB, pay.UserHK, pay.DestinationKey)
	if err != nil {
		return PayoutDetail{}, err
	}
	e := audit.Of(origin, audit.ActionWithdrawalPayoutView, audit.TargetWithdrawal, pay.WithdrawalID)
	e.Changes = &audit.Changes{After: map[string]any{"payout": pay.ID, "destination": pay.Destination}}
	if err := audit.Record(ctx, p.DB, e); err != nil {
		return PayoutDetail{}, err
	}
	return PayoutDetail{Payout: pay, DestinationValue: d.Value, DestinationMemo: d.Memo}, nil
}
