package user

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/deposits"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/verification"
)

// The deposit addresses of the custody (#68).

const (
	codeDepositsUnavailable = "deposits_unavailable"
	codeDepositsClosed      = "deposits_closed"
	codeDepositNetwork      = "deposit_network_unsupported"
	codeDepositCurrency     = "currency_not_found"
)

// depositAddressRequest asks for the address of a currency on a network.
type depositAddressRequest struct {
	CurrencyHK string `json:"currency_hk" binding:"required" format:"uuid"`
	Network    string `json:"network" binding:"required" example:"tron"`
}

func (h *UserHandler) depositsCaller(c *gin.Context) (string, bool) {
	claims, ok := sessionCaller(c)
	if !ok {
		return "", false
	}
	if h.Deposits == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codeDepositsUnavailable, i18n.M("DEPOSITS_UNAVAILABLE"), nil)
		return "", false
	}
	return claims.UUID, true
}

// @Summary Get a deposit address
// @Description The user's deposit address of a currency on a network (#68), given at the first request and the same after: every currency of a network shares it, such as TRX and USDT on TRON. It is derived from the extended public key of the custody, so the server holds no private key. The user must be allowed crypto deposits by their level and restrictions, and the currency must take deposits on the network (409 deposits_closed). Closed by the deposits lockdown. Audited as user.deposit_address when it is given.
// @Tags user-deposits
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body depositAddressRequest true "The currency and its network"
// @Success 200 {object} models.APIResponse{info=deposits.Address}
// @Failure 400 {object} models.APIResponse "validation_failed or deposit_network_unsupported"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "verification_required, account_frozen, operation_restricted, or asset_restricted"
// @Failure 404 {object} models.APIResponse "currency_not_found"
// @Failure 409 {object} models.APIResponse "deposits_closed or row_unverified"
// @Failure 503 {object} models.APIResponse "deposits_unavailable or feature_locked"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/deposit-addresses [post]
func (h *UserHandler) GetDepositAddress(c *gin.Context) {
	userHK, ok := h.depositsCaller(c)
	if !ok {
		return
	}
	var req depositAddressRequest
	if !decodeStrict(c, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	a, err := h.Deposits.Get(ctx, userHK, req.CurrencyHK, req.Network, audit.FromRequest(c))
	if err != nil {
		sendDepositError(c, userHK, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("DEPOSIT_ADDRESS_RETRIEVED"), a)
}

// @Summary List the deposit addresses
// @Description The user's deposit addresses, one per network (#68).
// @Tags user-deposits
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=[]deposits.Address}
// @Failure 401 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "row_unverified"
// @Failure 503 {object} models.APIResponse "deposits_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/deposit-addresses [get]
func (h *UserHandler) ListDepositAddresses(c *gin.Context) {
	userHK, ok := h.depositsCaller(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	list, err := h.Deposits.List(ctx, userHK)
	if err != nil {
		sendDepositError(c, userHK, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("DEPOSIT_ADDRESSES_RETRIEVED"), list)
}

func sendDepositError(c *gin.Context, userHK string, err error) {
	switch {
	case restrictions.Send(c, err), verification.Send(c, err):
	case errors.Is(err, deposits.ErrUnsupported):
		helpers.SendError(c, http.StatusBadRequest, codeDepositNetwork, i18n.M("DEPOSIT_NETWORK_UNSUPPORTED"), nil)
	case errors.Is(err, deposits.ErrNotFound):
		helpers.SendError(c, http.StatusNotFound, codeDepositCurrency, i18n.M("CURRENCY_NOT_FOUND"), nil)
	case errors.Is(err, deposits.ErrClosed):
		helpers.SendError(c, http.StatusConflict, codeDepositsClosed, i18n.M("DEPOSITS_CLOSED"), nil)
	case errors.Is(err, rowseal.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, "row_unverified", i18n.M("ROW_UNVERIFIED"), nil)
	default:
		log.Printf("Failed to give user %s a deposit address: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("INTERNAL_ERROR"), nil)
	}
}
