// Package restrictions holds the compliance restrictions of users (#220): a
// freeze of the whole account, a block of some operations, or a hold of one
// currency.
//
// The user_restrictions table is the only source of truth. Every check reads
// it, so that a restriction created on one instance applies on every other
// at once. A check that guards a change of money runs in the transaction of
// that change, after Lock, which takes a share lock of the user's row:
// creating a restriction takes the exclusive lock of the same row, so it
// waits for operations in flight, and operations that start after it see
// it. An operation is never half restricted.
package restrictions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/helpers"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Scope is what a restriction covers.
type Scope string

const (
	// ScopeAccount freezes the account: the user cannot sign in, and every
	// session ends.
	ScopeAccount Scope = "account"
	// ScopeOperation blocks some operations on every currency.
	ScopeOperation Scope = "operation"
	// ScopeAsset blocks some operations on one currency.
	ScopeAsset Scope = "asset"
)

// Operation is a kind of operation that a restriction can block.
type Operation string

const (
	OpDeposit  Operation = "deposit"
	OpWithdraw Operation = "withdraw"
	OpConvert  Operation = "convert"
	OpBuy      Operation = "buy"
	OpSell     Operation = "sell"
	// OpTrade blocks buying and selling.
	OpTrade Operation = "trade"
)

// Operations are the operations that a restriction can block.
var Operations = []Operation{OpDeposit, OpWithdraw, OpConvert, OpBuy, OpSell, OpTrade}

// covers reports whether blocking blocked blocks op.
func covers(blocked Operation, op Operation) bool {
	return blocked == op || (blocked == OpTrade && (op == OpBuy || op == OpSell))
}

// Reason is why a restriction was created. It is internal: users never see
// it.
type Reason string

const (
	ReasonCourtOrder  Reason = "court_order"
	ReasonAMLReview   Reason = "aml_review"
	ReasonSanctions   Reason = "sanctions"
	ReasonFraud       Reason = "fraud"
	ReasonSecurity    Reason = "security"
	ReasonUserRequest Reason = "user_request"
	ReasonOther       Reason = "other"
)

// Reasons are the reasons a restriction can have.
var Reasons = []Reason{ReasonCourtOrder, ReasonAMLReview, ReasonSanctions, ReasonFraud, ReasonSecurity, ReasonUserRequest, ReasonOther}

// Confidential reports whether the user must not learn when a restriction of
// this reason ends, since that could tip off the subject of a case.
func (r Reason) Confidential() bool {
	switch r {
	case ReasonCourtOrder, ReasonAMLReview, ReasonSanctions, ReasonFraud:
		return true
	}
	return false
}

// Restriction is a restriction of a user.
type Restriction struct {
	HK         string      `json:"id" format:"uuid"`
	UserHK     string      `json:"user_hk" format:"uuid"`
	Scope      Scope       `json:"scope" enums:"account,operation,asset"`
	Operations []Operation `json:"operations"`
	CurrencyHK *string     `json:"currency_hk,omitempty" format:"uuid"`
	Reason     Reason      `json:"reason" example:"aml_review"`
	CaseRef    string      `json:"case_ref,omitempty" example:"AML-2026-0042"`
	Note       string      `json:"note,omitempty"`
	EndsAt     *time.Time  `json:"ends_at,omitempty"`
	CreatedBy  string      `json:"created_by" format:"uuid"`
	CreatedAt  time.Time   `json:"created_at"`
	LiftedAt   *time.Time  `json:"lifted_at,omitempty"`
	LiftedBy   *string     `json:"lifted_by,omitempty" format:"uuid"`
	LiftNote   string      `json:"lift_note,omitempty"`
	Version    int64       `json:"version" example:"1"`
	// Active is whether the restriction applies now: not lifted, and not
	// past its end.
	Active bool `json:"active"`
}

// ActiveSQL is the condition, on a row of user_restrictions named r, of a
// restriction that applies now. An end needs no job: a restriction stops
// applying when NOW() passes it.
const ActiveSQL = `r.lifted_at IS NULL AND (r.ends_at IS NULL OR r.ends_at > NOW())`

// FrozenSQL is the condition, on a user hk expression, of a frozen account,
// for queries that read the user.
func FrozenSQL(userHK string) string {
	return `EXISTS (SELECT 1 FROM user_restrictions r WHERE r.user_hk = ` + userHK +
		` AND r.scope = 'account' AND ` + ActiveSQL + `)`
}

// Columns are the columns that Scan reads, in its order, on a row named r.
const Columns = `r.hk, r.user_hk, r.scope, array_to_string(r.operations, ','), r.currency_hk, r.reason, COALESCE(r.case_ref, ''),
	COALESCE(r.note, ''), r.ends_at, r.created_by, r.created_at, r.lifted_at, r.lifted_by,
	COALESCE(r.lift_note, ''), r.version, (` + ActiveSQL + `)`

type scanner interface{ Scan(dest ...any) error }

// Scan reads a row of Columns.
func Scan(row scanner) (Restriction, error) {
	var r Restriction
	var ops string
	var currency, liftedBy sql.NullString
	var endsAt, liftedAt sql.NullTime
	err := row.Scan(&r.HK, &r.UserHK, &r.Scope, &ops, &currency, &r.Reason, &r.CaseRef, &r.Note, &endsAt,
		&r.CreatedBy, &r.CreatedAt, &liftedAt, &liftedBy, &r.LiftNote, &r.Version, &r.Active)
	if err != nil {
		return r, err
	}
	r.Operations = []Operation{}
	for _, op := range strings.Split(ops, ",") {
		if op != "" {
			r.Operations = append(r.Operations, Operation(op))
		}
	}
	if currency.Valid {
		r.CurrencyHK = &currency.String
	}
	if liftedBy.Valid {
		r.LiftedBy = &liftedBy.String
	}
	if endsAt.Valid {
		r.EndsAt = &endsAt.Time
	}
	if liftedAt.Valid {
		r.LiftedAt = &liftedAt.Time
	}
	return r, nil
}

// Querier runs queries: a *sql.DB, or a *sql.Tx for a check in the
// transaction of an operation.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// maxActive bounds the restrictions that a check reads.
const maxActive = 1000

// Active returns the restrictions of a user that apply now, newest first.
func Active(ctx context.Context, q Querier, userHK string) ([]Restriction, error) {
	return List(ctx, q, userHK, true, maxActive)
}

// ErrUserNotFound is a lock of a user that does not exist.
var ErrUserNotFound = errors.New("user not found")

// Lock takes a share lock of the user's row in tx, which a new restriction
// of the user waits for. Operations of the same user still run together.
func Lock(ctx context.Context, tx *sql.Tx, userHK string) error {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE hk = $1 FOR SHARE`, userHK).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to lock the user: %w", err)
	}
	return nil
}

// Error codes of the refusals.
const (
	CodeAccountFrozen       = "account_frozen"
	CodeOperationRestricted = "operation_restricted"
	CodeAssetRestricted     = "asset_restricted"
)

// Refusal is an operation that a restriction blocks. It is an error.
type Refusal struct {
	Code      string
	Operation Operation
	// CurrencyHK is the held currency of an asset restriction.
	CurrencyHK string
	// Until is when the restriction ends, nil when it has no end or its
	// reason is confidential.
	Until *time.Time
}

func (r *Refusal) Error() string {
	if r.CurrencyHK != "" {
		return fmt.Sprintf("%s: %s of %s", r.Code, r.Operation, r.CurrencyHK)
	}
	return fmt.Sprintf("%s: %s", r.Code, r.Operation)
}

// Check returns a *Refusal when a restriction of the user blocks op on the
// currency currencyHK, which is empty for an operation on no currency, or
// when the security hold of the user (#251) blocks op. A restriction comes
// before the hold, since it says more. A check that guards a change of
// money runs in its transaction after Lock.
func Check(ctx context.Context, q Querier, userHK string, op Operation, currencyHK string) error {
	if !slices.Contains(Operations, op) {
		return fmt.Errorf("unknown operation %q", op)
	}
	active, err := Active(ctx, q, userHK)
	if err != nil {
		return err
	}
	// An account freeze comes first, then a block of every currency, then a
	// hold of this one. Among restrictions of the same rank, the refusal
	// ends with the last of them, and has no end when one of them has none.
	var found *Refusal
	rank := 0
	for _, r := range active {
		var code string
		var n int
		switch {
		case r.Scope == ScopeAccount:
			code, n = CodeAccountFrozen, 3
		case r.Scope == ScopeOperation && blocks(r, op):
			code, n = CodeOperationRestricted, 2
		case r.Scope == ScopeAsset && currencyHK != "" && r.CurrencyHK != nil && *r.CurrencyHK == currencyHK && blocks(r, op):
			code, n = CodeAssetRestricted, 1
		default:
			continue
		}
		end := Public(r).Until
		switch {
		case n > rank:
			rank = n
			found = &Refusal{Code: code, Operation: op, Until: end}
			if code == CodeAssetRestricted {
				found.CurrencyHK = currencyHK
			}
		case n == rank && (end == nil || found.Until == nil):
			found.Until = nil
		case n == rank && end.After(*found.Until):
			found.Until = end
		}
	}
	if found == nil {
		if held(op) {
			return CheckSecurityHold(ctx, q, userHK, op)
		}
		return nil
	}
	return found
}

func blocks(r Restriction, op Operation) bool {
	for _, b := range r.Operations {
		if covers(b, op) {
			return true
		}
	}
	return false
}

// PublicRestriction is a restriction as its user sees it: without the
// reason, the case, the note, or who created it.
type PublicRestriction struct {
	ID         string      `json:"id" format:"uuid"`
	Scope      Scope       `json:"scope" enums:"account,operation,asset"`
	Operations []Operation `json:"operations"`
	CurrencyHK *string     `json:"currency_hk,omitempty" format:"uuid"`
	// Until is when the restriction ends. It is absent when the restriction
	// has no end, or its end is not shown.
	Until     *time.Time `json:"until,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// Public returns r as its user sees it.
func Public(r Restriction) PublicRestriction {
	p := PublicRestriction{ID: r.HK, Scope: r.Scope, Operations: r.Operations, CurrencyHK: r.CurrencyHK, CreatedAt: r.CreatedAt}
	if p.Operations == nil {
		p.Operations = []Operation{}
	}
	if !r.Reason.Confidential() {
		p.Until = r.EndsAt
	}
	return p
}

// Messages of the refusals. They name no reason, since the user may be the
// subject of a case.
const (
	MessageAccountFrozen       = "This account is frozen. Please contact support."
	messageOperationRestricted = "This operation is not available on your account. Please contact support."
	messageAssetRestricted     = "This operation is not available for this currency on your account. Please contact support."
)

// Send answers a refusal with 403 and its code. It reports false, and sends
// nothing, for an err that is not a *Refusal.
func Send(c *gin.Context, err error) bool {
	var r *Refusal
	if !errors.As(err, &r) {
		return false
	}
	info := gin.H{"operation": r.Operation}
	message := messageOperationRestricted
	switch r.Code {
	case CodeAccountFrozen:
		message = MessageAccountFrozen
	case CodeAssetRestricted:
		message = messageAssetRestricted
		info["currency_hk"] = r.CurrencyHK
	case CodeSecurityHold:
		message = messageSecurityHold
	}
	if r.Until != nil {
		info["until"] = r.Until.UTC().Format(time.RFC3339)
	}
	helpers.SendError(c, http.StatusForbidden, r.Code, message, info)
	return true
}

// NewHK returns the hk of a new restriction.
func NewHK() string { return uuid.NewString() }
