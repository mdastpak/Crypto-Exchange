package user

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/clientip"
	"github.com/mdastpak/crypto-exchange/internal/confirm"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/verification"
	"github.com/mdastpak/crypto-exchange/internal/withdrawals"

	"github.com/gin-gonic/gin"
)

// The codes of the refusals of withdrawals (#70).
const (
	codeWithdrawalsUnavailable       = "withdrawals_unavailable"
	codeIdempotencyKeyRequired       = "idempotency_key_required"
	codeIdempotencyKeyReused         = "idempotency_key_reused"
	codeWithdrawalsClosed            = "withdrawals_closed"
	codeInsufficientBalance          = "insufficient_balance"
	codeWithdrawalNotFound           = "withdrawal_not_found"
	codeWithdrawalNotCancellable     = "withdrawal_not_cancellable"
	codeDestinationNotUsableYet      = "destination_not_usable_yet"
	codeConfirmationNotConfirmed     = "confirmation_not_confirmed"
	codeConfirmationConsumed         = "confirmation_consumed"
	codeConfirmationOfAnotherRequest = "confirmation_mismatch"
	codeWithdrawalFeeChanged         = "withdrawal_fee_changed"
)

// withdrawalRequest asks for a withdrawal. Amount is a decimal string.
type withdrawalRequest struct {
	CurrencyHK string `json:"currency_hk" format:"uuid"`
	// Network is a network of the currency, such as "tron", or "internal"
	// for rials.
	Network string `json:"network" example:"tron"`
	// DestinationID is a destination of the user's book that is usable.
	DestinationID string         `json:"destination_id" format:"uuid"`
	Amount        shared.Decimal `json:"amount" swaggertype:"string" example:"100"`
	// ConfirmationID is the confirmation that the first request returned,
	// once confirmed with POST /user/confirmations/{id}.
	ConfirmationID string `json:"confirmation_id,omitempty" format:"uuid"`
}

// withdrawalChallenge is the answer to a request without a confirmation.
type withdrawalChallenge struct {
	Confirmation confirm.Challenge `json:"confirmation"`
}

func (h *UserHandler) withdrawalsCaller(c *gin.Context) (string, bool) {
	claims, ok := sessionCaller(c)
	if !ok {
		return "", false
	}
	if h.Withdrawals == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codeWithdrawalsUnavailable, i18n.M("WITHDRAWALS_UNAVAILABLE"), nil)
		return "", false
	}
	return claims.UUID, true
}

// RequestWithdrawal asks for a withdrawal (#70).
//
// @Summary Request a withdrawal
// @Description Withdraws amount of a currency, on one of its networks, to a destination of the user's book (#160), in two steps under one Idempotency-Key. Without confirmation_id, the request is checked as it would run, and a confirmation of the user's second factor starts (#161): 202 with the confirmation, whose code went to the user by their method. Confirm it with POST /user/confirmations/{id}, then send the same request with confirmation_id: it runs in one transaction, under a lock of the user, and locks the amount and the fee of the network in the ledger. 201 with the withdrawal; the same request again answers 200 with it. The fee of the network is added to the amount, and both count towards the limits of the user's level (#157). A withdrawal of at least withdrawals.review_above of its currency waits for an administrator (status review, review_reason amount); while thresholds are configured, one of a currency without a threshold and without a "*" default waits whatever the amount (review_reason no_threshold, #406); the others are queued for their payouts. Closed by the withdrawals lockdown, the user's restrictions, and the security hold.
// @Tags user-withdrawals
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param Idempotency-Key header string true "1 to 100 letters, digits, dots, colons, underscores, or hyphens; the same for both steps"
// @Param body body withdrawalRequest true "The withdrawal"
// @Success 200 {object} models.APIResponse{info=withdrawals.Withdrawal} "The request ran before"
// @Success 201 {object} models.APIResponse{info=withdrawals.Withdrawal}
// @Success 202 {object} models.APIResponse{info=withdrawalChallenge}
// @Failure 400 {object} models.APIResponse "validation_failed, or idempotency_key_required"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "verification_required, withdrawal_limit_exceeded, withdrawal_limit_unconfigured, account_frozen, operation_restricted, asset_restricted, or security_hold"
// @Failure 409 {object} models.APIResponse "insufficient_balance, withdrawals_closed, destination_not_usable_yet, idempotency_key_reused, confirmation_method_required, confirmation_not_confirmed, confirmation_expired, confirmation_consumed, confirmation_mismatch, withdrawal_fee_changed (with info.confirmed_fee and info.fee), confirmation_denied, or row_unverified"
// @Failure 503 {object} models.APIResponse "withdrawals_unavailable or feature_locked"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/withdrawals [post]
func (h *UserHandler) RequestWithdrawal(c *gin.Context) {
	userHK, ok := h.withdrawalsCaller(c)
	if !ok {
		return
	}
	key := c.GetHeader("Idempotency-Key")
	if key == "" {
		helpers.SendError(c, http.StatusBadRequest, codeIdempotencyKeyRequired, i18n.M("IDEMPOTENCY_KEY_REQUIRED"), nil)
		return
	}
	var req withdrawalRequest
	if !decodeStrict(c, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	out, err := h.Withdrawals.Request(ctx, userHK, withdrawals.Request{
		CurrencyHK: req.CurrencyHK, Network: req.Network, DestinationID: req.DestinationID, Amount: req.Amount,
		ConfirmationID: req.ConfirmationID, IdempotencyKey: key,
	}, clientip.Key(c.ClientIP(), h.settings().RateLimits.IPv6PrefixLength), audit.FromRequest(c))
	switch {
	case err != nil:
		sendWithdrawalError(c, userHK, err)
	case out.Challenge != nil:
		helpers.SendResponse(c, http.StatusAccepted, i18n.M("WITHDRAWAL_CONFIRMATION_SENT"), withdrawalChallenge{Confirmation: *out.Challenge})
	case out.Replayed:
		helpers.SendResponse(c, http.StatusOK, i18n.M("WITHDRAWAL_RETRIEVED"), out.Withdrawal)
	default:
		helpers.SendResponse(c, http.StatusCreated, i18n.M("WITHDRAWAL_REQUESTED"), out.Withdrawal)
	}
}

// ListWithdrawals lists the user's withdrawals (#70).
//
// @Summary List my withdrawals
// @Description The withdrawals of the signed-in user, newest first, at most limit (100). before, an RFC 3339 time, lists those created before it, for the next page: the created_at of the last one.
// @Tags user-withdrawals
// @Produce json
// @Security BearerAuth
// @Param limit query int false "At most 100" minimum(1) maximum(100)
// @Param before query string false "An RFC 3339 time" format(date-time)
// @Success 200 {object} models.APIResponse{info=[]withdrawals.Withdrawal}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/withdrawals [get]
func (h *UserHandler) ListWithdrawals(c *gin.Context) {
	userHK, ok := h.withdrawalsCaller(c)
	if !ok {
		return
	}
	limit, before, ok := listParams(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	list, err := h.Withdrawals.List(ctx, userHK, before, limit)
	if err != nil {
		sendWithdrawalError(c, userHK, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("WITHDRAWALS_RETRIEVED"), list)
}

// listParams reads limit and before.
func listParams(c *gin.Context) (int, *time.Time, bool) {
	errs := map[string]string{}
	limit := withdrawals.MaxList
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > withdrawals.MaxList {
			errs["limit"] = "must be from 1 to 100"
		}
		limit = n
	}
	var before *time.Time
	if raw := c.Query("before"); raw != "" {
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			errs["before"] = "must be an RFC 3339 time"
		}
		before = &t
	}
	if len(errs) > 0 {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": errs})
		return 0, nil, false
	}
	return limit, before, true
}

// GetWithdrawal returns a withdrawal of the user (#70).
//
// @Summary Get my withdrawal
// @Tags user-withdrawals
// @Produce json
// @Security BearerAuth
// @Param id path string true "The withdrawal" format(uuid)
// @Success 200 {object} models.APIResponse{info=withdrawals.Withdrawal}
// @Failure 401 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "withdrawal_not_found"
// @Failure 409 {object} models.APIResponse "row_unverified"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/withdrawals/{id} [get]
func (h *UserHandler) GetWithdrawal(c *gin.Context) {
	userHK, ok := h.withdrawalsCaller(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	w, err := h.Withdrawals.Get(ctx, userHK, c.Param("id"))
	if err != nil {
		sendWithdrawalError(c, userHK, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("WITHDRAWAL_RETRIEVED"), w)
}

// CancelWithdrawal cancels a withdrawal of the user (#70).
//
// @Summary Cancel my withdrawal
// @Description Cancels a withdrawal whose payouts have not started (status review or queued), and releases its amount and fee. Audited as user.withdrawal_cancel.
// @Tags user-withdrawals
// @Produce json
// @Security BearerAuth
// @Param id path string true "The withdrawal" format(uuid)
// @Success 200 {object} models.APIResponse{info=withdrawals.Withdrawal}
// @Failure 401 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "withdrawal_not_found"
// @Failure 409 {object} models.APIResponse "withdrawal_not_cancellable or row_unverified"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/withdrawals/{id}/cancel [post]
func (h *UserHandler) CancelWithdrawal(c *gin.Context) {
	userHK, ok := h.withdrawalsCaller(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	w, err := h.Withdrawals.Cancel(ctx, userHK, c.Param("id"), audit.FromRequest(c))
	if err != nil {
		sendWithdrawalError(c, userHK, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("WITHDRAWAL_CANCELLED"), w)
}

// sendWithdrawalError answers a refusal of a withdrawal.
func sendWithdrawalError(c *gin.Context, userHK string, err error) {
	var invalid withdrawals.Invalid
	var notYet *destinations.NotUsableYet
	switch {
	case errors.As(err, &invalid):
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": invalid})
	case restrictions.Send(c, err), verification.Send(c, err):
	case errors.As(err, &notYet):
		helpers.SendError(c, http.StatusConflict, codeDestinationNotUsableYet, i18n.M("DESTINATION_NOT_USABLE_YET"),
			gin.H{"usable_from": notYet.Until.UTC().Format(time.RFC3339)})
	case errors.Is(err, withdrawals.ErrIdempotencyConflict):
		helpers.SendError(c, http.StatusConflict, codeIdempotencyKeyReused, i18n.M("IDEMPOTENCY_KEY_REUSED"), nil)
	case errors.Is(err, withdrawals.ErrClosed):
		helpers.SendError(c, http.StatusConflict, codeWithdrawalsClosed, i18n.M("WITHDRAWALS_CLOSED"), nil)
	case errors.Is(err, withdrawals.ErrInsufficientBalance):
		helpers.SendError(c, http.StatusConflict, codeInsufficientBalance, i18n.M("INSUFFICIENT_BALANCE"), nil)
	case errors.Is(err, withdrawals.ErrNotFound):
		helpers.SendError(c, http.StatusNotFound, codeWithdrawalNotFound, i18n.M("WITHDRAWAL_NOT_FOUND"), nil)
	case errors.Is(err, withdrawals.ErrNotCancellable):
		helpers.SendError(c, http.StatusConflict, codeWithdrawalNotCancellable, i18n.M("WITHDRAWAL_NOT_CANCELLABLE"), nil)
	case errors.Is(err, confirm.ErrNotConfirmed):
		helpers.SendError(c, http.StatusConflict, codeConfirmationNotConfirmed, i18n.M("CONFIRMATION_NOT_CONFIRMED"), nil)
	case errors.Is(err, confirm.ErrConsumed):
		helpers.SendError(c, http.StatusConflict, codeConfirmationConsumed, i18n.M("CONFIRMATION_CONSUMED"), nil)
	case errors.Is(err, withdrawals.ErrFeeChanged):
		var changed *withdrawals.FeeChangedError
		info := gin.H{}
		if errors.As(err, &changed) {
			info["confirmed_fee"], info["fee"] = changed.Confirmed, changed.Current
		}
		helpers.SendError(c, http.StatusConflict, codeWithdrawalFeeChanged, i18n.M("WITHDRAWAL_FEE_CHANGED"), info)
	case errors.Is(err, confirm.ErrMismatch):
		helpers.SendError(c, http.StatusConflict, codeConfirmationOfAnotherRequest, i18n.M("CONFIRMATION_MISMATCH"), nil)
	case errors.Is(err, destinations.ErrNoKeys):
		helpers.SendError(c, http.StatusServiceUnavailable, codeWithdrawalsUnavailable, i18n.M("WITHDRAWALS_UNAVAILABLE"), nil)
	case errors.Is(err, rowseal.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, "row_unverified", i18n.M("WITHDRAWAL_UNVERIFIED"), nil)
	case isConfirmationError(err):
		sendConfirmationError(c, userHK, err)
	default:
		log.Printf("Failed to handle a withdrawal of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("WITHDRAWAL_FAILED"), nil)
	}
}

// isConfirmationError reports a refusal of the confirmations, which
// sendConfirmationError answers.
func isConfirmationError(err error) bool {
	for _, target := range []error{confirm.ErrNotFound, confirm.ErrExpired, confirm.ErrNoMethod, confirm.ErrMethodUnavailable,
		confirm.ErrTooManyAttempts, confirm.ErrDenied} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// decodeStrict decodes the body into into, and refuses an unknown field,
// such as a misspelled amount, rather than ignore it.
func decodeStrict(c *gin.Context, into any) bool {
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return false
	}
	return true
}
