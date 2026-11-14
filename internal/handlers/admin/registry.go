package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/assetregistry"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/domain/market"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// The registry of how the currencies move and trade (#57): the networks of
// each currency and the markets of two currencies. config/assets.yaml is
// imported into them; these routes read and change them one at a time,
// with the checks of the file.

// The codes of the refusals of the registry routes.
const (
	codeNetworkContractFixed = "network_contract_fixed"
	codeContractTaken        = "contract_taken"
	codeMarketNotFound       = "market_not_found"
)

// networkRequest is a network of a currency, whole: a field left out is
// empty, false, or 0. Amounts are decimal strings.
type networkRequest struct {
	// ContractAddress is the token's contract, which its network's format
	// and checksum must accept; empty for the native asset of the network.
	// It does not change once the network exists.
	ContractAddress string `json:"contract_address" example:"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"`
	// Decimals of an amount on the network, from 0 to 36. Required, and
	// fixed once the network exists.
	Decimals *int `json:"decimals" example:"6"`
	// CustodyMode is OFF_CHAIN on the internal network of fiat currencies
	// and ON_CHAIN on the others.
	CustodyMode       string `json:"custody_mode" enums:"ON_CHAIN,OFF_CHAIN" example:"ON_CHAIN"`
	DepositEnabled    bool   `json:"deposit_enabled" example:"true"`
	WithdrawalEnabled bool   `json:"withdrawal_enabled" example:"true"`
	// MinDeposit is "0" when left out.
	MinDeposit    string `json:"min_deposit" example:"1"`
	MinWithdrawal string `json:"min_withdrawal" example:"10"`
	// MaxWithdrawal of "0" is no maximum.
	MaxWithdrawal string `json:"max_withdrawal" example:"0"`
	WithdrawalFee string `json:"withdrawal_fee" example:"1"`
	// FeeCurrency is the symbol of the currency that pays the network fee:
	// the native asset of the network, such as TRX on tron.
	FeeCurrency string `json:"fee_currency" example:"TRX"`
	RequiresTag bool   `json:"requires_tag" example:"false"`
	// ExplorerURL is an https URL.
	ExplorerURL string `json:"explorer_url" example:"https://tronscan.org"`
}

// marketRequest is a market, whole. Amounts are decimal strings.
type marketRequest struct {
	Status       string `json:"status" enums:"ACTIVE,INACTIVE,MAINTENANCE,SUSPENDED,DELISTED" example:"ACTIVE"`
	MinOrderSize string `json:"min_order_size" example:"0.0001"`
	// MaxOrderSize of "0" is no maximum.
	MaxOrderSize string `json:"max_order_size" example:"0"`
	MinNotional  string `json:"min_notional" example:"10"`
	TickSize     string `json:"tick_size" example:"0.01"`
	LotSize      string `json:"lot_size" example:"0.0001"`
	// MakerFeeBPS and TakerFeeBPS are fees in basis points, from 0 to
	// 1000. Required.
	MakerFeeBPS *int `json:"maker_fee_bps" example:"10"`
	TakerFeeBPS *int `json:"taker_fee_bps" example:"20"`
}

// problemFields names the fields of the registry file as the requests do.
var problemFields = map[string]string{"fee_asset": "fee_currency"}

// invalidFields returns the problems of an *currencies.Invalid by field.
func invalidFields(invalid *currencies.Invalid) fieldErrors {
	errs := fieldErrors{}
	for _, p := range invalid.Problems {
		field := p.Field
		if renamed, ok := problemFields[field]; ok {
			field = renamed
		}
		if _, taken := errs[field]; !taken {
			errs[field] = p.Text
		}
	}
	return errs
}

// @Summary List the networks of a currency
// @Description How the currency moves on each network (#57): the contract, the decimals, the custody mode, whether deposits and withdrawals are open, the limits, the withdrawal fee, and the currency that pays the network fee. A deleted currency keeps its networks.
// @Tags admin currencies
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The currency's UUID" format(uuid)
// @Success 200 {object} models.APIResponse{info=[]currency.Network}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "row_unverified: a row fails its seal (#139)"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies/{hk}/networks [get]
func (h *AdminHandler) ListNetworks(c *gin.Context) {
	hk := c.Param("hk")
	if _, err := uuid.Parse(hk); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_CURRENCY_ID"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	list, err := h.Registry.Networks(ctx, hk)
	switch {
	case errors.Is(err, currencies.ErrNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("CURRENCY_NOT_FOUND"), nil)
	case errors.Is(err, rowseal.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
	case err != nil:
		log.Printf("Failed to list the networks of currency %s: %v", hk, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("NETWORKS_RETRIEVAL_FAILED"), nil)
	default:
		helpers.SendResponse(c, http.StatusOK, i18n.M("NETWORKS_RETRIEVED"), list)
	}
}

// @Summary Add or change a network of a currency
// @Description Writes how the currency moves on the network (#57), whole, with the checks of config/assets.yaml: a known network (internal for fiat currencies, and only for them), a contract that the network's format and checksum accept and no other currency has there, none for the native asset, the custody mode, the native asset as the fee currency, and the limits. The contract and the decimals of a network do not change once it exists (409 network_contract_fixed): deposits are read by the contract, so a new one would credit another token as this currency. A token that moved to a new contract is a migration of the currency; the reviewed registry file can still change them. To stop a network, close its deposits and withdrawals. 201 for a new network, 200 otherwise. A sensitive route: it needs a step-up. Audited as currency.network_set with the fields that changed. The public currency list shows the networks of approved currencies.
// @Tags admin currencies
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The currency's UUID" format(uuid)
// @Param network path string true "The network" Enums(internal,bitcoin,ethereum,tron)
// @Param body body networkRequest true "The network"
// @Success 200 {object} models.APIResponse{info=currency.Network}
// @Success 201 {object} models.APIResponse{info=currency.Network}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "network_contract_fixed, contract_taken, or row_unverified: the network fails its seal"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies/{hk}/networks/{network} [put]
func (h *AdminHandler) SetNetwork(c *gin.Context) {
	hk := c.Param("hk")
	if _, err := uuid.Parse(hk); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_CURRENCY_ID"), nil)
		return
	}
	var req networkRequest
	if !decodeStrict(c, &req) {
		return
	}
	errs := fieldErrors{}
	if req.Decimals == nil {
		errs["decimals"] = "is required"
	}
	if req.ExplorerURL != "" {
		if u, err := url.Parse(req.ExplorerURL); err != nil || u.Scheme != "https" || u.Host == "" || len(req.ExplorerURL) > 500 {
			errs["explorer_url"] = "must be an https URL of at most 500 characters"
		}
	}
	if len(errs) > 0 {
		sendValidationErrors(c, errs)
		return
	}
	n := assetregistry.Network{
		Network: c.Param("network"), ContractAddress: req.ContractAddress, FeeAsset: req.FeeCurrency,
		MinDeposit: req.MinDeposit, MinWithdrawal: req.MinWithdrawal, MaxWithdrawal: req.MaxWithdrawal,
		WithdrawalFee: req.WithdrawalFee, DepositEnabled: req.DepositEnabled, WithdrawalEnabled: req.WithdrawalEnabled,
		CustodyMode: shared.SettlementMode(req.CustodyMode), RequiresTag: req.RequiresTag, ExplorerURL: req.ExplorerURL,
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	w, err := h.Registry.SetNetwork(ctx, hk, n, *req.Decimals, h.currencyOrigin(c))
	var invalid *currencies.Invalid
	switch {
	case errors.As(err, &invalid):
		sendValidationErrors(c, invalidFields(invalid))
		return
	case errors.Is(err, currencies.ErrNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("CURRENCY_NOT_FOUND"), nil)
		return
	case errors.Is(err, currencies.ErrContractFixed):
		helpers.SendError(c, http.StatusConflict, codeNetworkContractFixed, i18n.M("NETWORK_CONTRACT_FIXED"), nil)
		return
	case errors.Is(err, currencies.ErrContractTaken):
		helpers.SendError(c, http.StatusConflict, codeContractTaken, i18n.M("CONTRACT_TAKEN"), nil)
		return
	case errors.Is(err, rowseal.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
		return
	case err != nil:
		sendRegistryWriteError(c, err, "network of currency "+hk, i18n.M("NETWORK_SET_FAILED"))
		return
	}
	if w.Public && w.Changed {
		h.afterPublicChange(ctx, hk)
	}
	status := http.StatusOK
	if w.Created {
		status = http.StatusCreated
	}
	helpers.SendResponse(c, status, i18n.M("NETWORK_SET"), w.Network)
}

// @Summary List the markets
// @Description The markets of two currencies (#57) that are not deleted, by symbol, with their status and rules.
// @Tags admin markets
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=[]market.Market}
// @Failure 401 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "row_unverified: a row fails its seal (#139)"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/markets [get]
func (h *AdminHandler) ListMarkets(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	list, err := h.Registry.Markets(ctx)
	if errors.Is(err, rowseal.ErrUnverified) {
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
		return
	}
	if err != nil {
		log.Printf("Failed to list the markets: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("MARKETS_RETRIEVAL_FAILED"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("MARKETS_RETRIEVED"), list)
}

// @Summary Show a market
// @Tags admin markets
// @Produce json
// @Security BearerAuth
// @Param base path string true "The symbol of the base currency" example(BTC)
// @Param quote path string true "The symbol of the quote currency" example(USDT)
// @Success 200 {object} models.APIResponse{info=market.Market}
// @Failure 401 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "market_not_found"
// @Failure 409 {object} models.APIResponse "row_unverified: a row fails its seal (#139)"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/markets/{base}/{quote} [get]
func (h *AdminHandler) GetMarket(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	base, quote := pair(c)
	m, err := h.Registry.Market(ctx, base, quote)
	switch {
	case errors.Is(err, currencies.ErrMarketNotFound):
		helpers.SendError(c, http.StatusNotFound, codeMarketNotFound, i18n.M("MARKET_NOT_FOUND"), nil)
	case errors.Is(err, rowseal.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
	case err != nil:
		log.Printf("Failed to read the market %s/%s: %v", base, quote, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("MARKETS_RETRIEVAL_FAILED"), nil)
	default:
		helpers.SendResponse(c, http.StatusOK, i18n.M("MARKET_RETRIEVED"), m)
	}
}

// @Summary Add or change a market
// @Description Writes the market of two currencies, named by symbol (#57), whole, with the checks of config/assets.yaml: two different currencies that exist, a known status, order sizes whose minimum is not over the maximum, a positive tick and lot size, a minimum notional of at least 0, and fees from 0 to 1000 basis points. ACTIVE trades; MAINTENANCE and SUSPENDED pause trading, and INACTIVE and DELISTED take the market out of the public list. 201 for a new market, 200 otherwise. A sensitive route: it needs a step-up. Audited as market.set with the fields that changed. Every instance shows the change in the public list within ten seconds.
// @Tags admin markets
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param base path string true "The symbol of the base currency" example(BTC)
// @Param quote path string true "The symbol of the quote currency" example(USDT)
// @Param body body marketRequest true "The market"
// @Success 200 {object} models.APIResponse{info=market.Market}
// @Success 201 {object} models.APIResponse{info=market.Market}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 409 {object} models.APIResponse "row_unverified: a row fails its seal (#139)"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/markets/{base}/{quote} [put]
func (h *AdminHandler) SetMarket(c *gin.Context) {
	var req marketRequest
	if !decodeStrict(c, &req) {
		return
	}
	errs := fieldErrors{}
	if req.MakerFeeBPS == nil {
		errs["maker_fee_bps"] = "is required"
	}
	if req.TakerFeeBPS == nil {
		errs["taker_fee_bps"] = "is required"
	}
	if len(errs) > 0 {
		sendValidationErrors(c, errs)
		return
	}
	base, quote := pair(c)
	m := assetregistry.Market{
		Base: base, Quote: quote, Status: market.MarketStatus(req.Status),
		MinOrderSize: req.MinOrderSize, MaxOrderSize: req.MaxOrderSize, MinNotional: req.MinNotional,
		TickSize: req.TickSize, LotSize: req.LotSize, MakerFeeBPS: *req.MakerFeeBPS, TakerFeeBPS: *req.TakerFeeBPS,
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	w, err := h.Registry.SetMarket(ctx, m, h.currencyOrigin(c))
	var invalid *currencies.Invalid
	switch {
	case errors.As(err, &invalid):
		sendValidationErrors(c, invalidFields(invalid))
		return
	case errors.Is(err, rowseal.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
		return
	case err != nil:
		sendRegistryWriteError(c, err, "market "+m.Base+"/"+m.Quote, i18n.M("MARKET_SET_FAILED"))
		return
	}
	status := http.StatusOK
	if w.Created {
		status = http.StatusCreated
	}
	helpers.SendResponse(c, status, i18n.M("MARKET_SET"), w.Market)
}

// sendRegistryWriteError answers a write of what that failed: 500
// FAILED_TO_RECORD_THE_CHANGE when its audit event could not be written,
// which rolled it back, and otherwise 500 with failure.
func sendRegistryWriteError(c *gin.Context, err error, what string, failure i18n.Message) {
	if errors.Is(err, currencies.ErrUnrecorded) {
		log.Printf("Refused the change of the %s, which could not be recorded: %v", what, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_RECORD_THE_CHANGE"), nil)
		return
	}
	log.Printf("Failed to write the %s: %v", what, err)
	helpers.SendResponse(c, http.StatusInternalServerError, failure, nil)
}

// pair returns the symbols of the market of the path, which are upper case
// as every symbol is, so that /markets/btc/usdt finds BTC/USDT.
func pair(c *gin.Context) (base, quote string) {
	return strings.ToUpper(c.Param("base")), strings.ToUpper(c.Param("quote"))
}
