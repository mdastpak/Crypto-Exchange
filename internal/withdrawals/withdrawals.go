// Package withdrawals runs the withdrawals of users (#70).
//
// A withdrawal is asked in two steps. Without a confirmation, the request
// is checked as it would run, and a confirmation of the user's second
// factor is started, bound to the digest of the request (#161). With the
// confirmation, it runs in one transaction, under a lock of the user: the
// checks again, the confirmation consumed, the withdrawal recorded, and its
// amount and fee locked in the ledger (#60). The idempotency key of the
// request makes a retry return the same withdrawal, and refuses another
// request with the key.
//
// The checks are the user's restrictions and security hold (#251), the
// lifecycle of the currency (#216), the state and limits of the network
// (#57), the destination, which must be the user's, usable, and of the
// kind and network of the currency (#160), the limits of the user's level
// (#157), and the balance.
//
// A withdrawal of at least withdrawals.review_above of its currency waits
// for an administrator; the others are queued for their payouts. Until a
// payout starts, the user may cancel it, and an administrator may reject
// one in review: both release its funds. Withdrawals are sealed rows
// (#139): one changed in SQL is not acted on.
package withdrawals

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
)

// Status is where a withdrawal is.
type Status string

// The statuses of a withdrawal.
const (
	// StatusReview waits for an administrator.
	StatusReview Status = "review"
	// StatusQueued waits for its payouts.
	StatusQueued Status = "queued"
	// StatusProcessing is being paid.
	StatusProcessing Status = "processing"
	// StatusPartiallyPaid has some of its payouts paid.
	StatusPartiallyPaid Status = "partially_paid"
	// StatusCompleted is paid.
	StatusCompleted Status = "completed"
	// StatusCancelled was cancelled by the user, StatusRejected by an
	// administrator, and StatusFailed could not be paid; their funds are
	// released.
	StatusCancelled Status = "cancelled"
	StatusRejected  Status = "rejected"
	StatusFailed    Status = "failed"
)

// Cancellable reports whether the user may still cancel a withdrawal of
// the status: no payout has started.
func (s Status) Cancellable() bool { return s == StatusReview || s == StatusQueued }

// ReviewAmount is the reason of a withdrawal held for review because of
// its amount.
const ReviewAmount = "amount"

// Withdrawal is a withdrawal of a user.
type Withdrawal struct {
	ID string `json:"id" format:"uuid"`
	// UserHK is the user's, in the views of administrators.
	UserHK     string `json:"user_hk,omitempty" format:"uuid"`
	CurrencyHK string `json:"currency_hk" format:"uuid"`
	// Currency is the symbol of the currency.
	Currency string `json:"currency" example:"USDT"`
	Network  string `json:"network" example:"tron"`
	// DestinationID is the destination of the user's book, and
	// Destination its number or address, masked.
	DestinationID string `json:"destination_id" format:"uuid"`
	Destination   string `json:"destination" example:"TR7NHq...gjLj6t"`
	// Amount is what the destination receives, Fee the fee of the network,
	// and Total their sum, which left the user's available balance.
	Amount shared.Decimal `json:"amount" swaggertype:"string" example:"100"`
	Fee    shared.Decimal `json:"fee" swaggertype:"string" example:"1"`
	Total  shared.Decimal `json:"total" swaggertype:"string" example:"101"`
	Status Status         `json:"status" enums:"review,queued,processing,partially_paid,completed,cancelled,rejected,failed" example:"queued"`
	// ReviewReason is why it waits, or waited, for an administrator.
	ReviewReason string `json:"review_reason,omitempty" enums:"amount"`
	// Reason is why it was rejected or failed.
	Reason      string     `json:"reason,omitempty"`
	ReviewedAt  *time.Time `json:"reviewed_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	CancelledAt *time.Time `json:"cancelled_at,omitempty"`
	// Payouts are the transfers that pay it, in the view of one withdrawal.
	Payouts []Payout `json:"payouts,omitempty"`
}

// Errors of the withdrawals.
var (
	// ErrNotFound is a withdrawal that is not the user's, or does not
	// exist.
	ErrNotFound = errors.New("withdrawal not found")
	// ErrIdempotencyConflict refuses a request whose idempotency key a
	// request for another withdrawal used.
	ErrIdempotencyConflict = errors.New("the idempotency key was used for another withdrawal")
	// ErrClosed refuses a withdrawal of a currency, or on a network, whose
	// withdrawals are closed: by its lifecycle, such as a delisting, or by
	// the network.
	ErrClosed = errors.New("withdrawals of the currency on this network are closed")
	// ErrNotCancellable refuses to cancel a withdrawal whose payouts
	// started, or that is settled.
	ErrNotCancellable = errors.New("the withdrawal can no longer be cancelled")
	// ErrNotInReview refuses to approve or reject a withdrawal that does
	// not wait for review.
	ErrNotInReview = errors.New("the withdrawal does not wait for review")
	// ErrInsufficientBalance is a withdrawal of more than the available
	// balance.
	ErrInsufficientBalance = ledger.ErrInsufficientBalance
)

// Invalid is a request with problems, by field.
type Invalid map[string]string

func (i Invalid) Error() string {
	fields := make([]string, 0, len(i))
	for field, problem := range i {
		fields = append(fields, field+" "+problem)
	}
	sort.Strings(fields)
	return "invalid withdrawal: " + strings.Join(fields, "; ")
}

func invalid(field, format string, args ...any) Invalid {
	return Invalid{field: fmt.Sprintf(format, args...)}
}
