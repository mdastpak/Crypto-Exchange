package user

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/balances"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/pagination"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// The codes of the balance routes (#365).
const (
	codeAccountNotFound     = "account_not_found"
	codeBalanceUnverified   = "balance_unverified"
	codeBalancesUnavailable = "balances_unavailable"
)

// AccountBalances are the balances of one account of the user.
type AccountBalances struct {
	// SubAccountID is the sub-account, absent for the main account.
	SubAccountID *string            `json:"sub_account_id,omitempty" format:"uuid"`
	Balances     []balances.Balance `json:"balances"`
}

// BalancesInfo is the answer of GET /user/balances: the balances, grouped
// by account, one group for the main account or a sub-account, and one per
// account with ?account=all.
type BalancesInfo struct {
	Accounts []AccountBalances `json:"accounts"`
}

// TransactionPage is a page of the history. It describes
// pagination.Page[balances.Change] in the OpenAPI document.
type TransactionPage struct {
	Items []balances.Change `json:"items"`
	// NextCursor asks for the next page. It is left out on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// balancesCaller returns the signed-in user, or answers and reports false.
func (h *UserHandler) balancesCaller(c *gin.Context) (string, bool) {
	claims, ok := sessionCaller(c)
	if !ok {
		return "", false
	}
	if h.Balances == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codeBalancesUnavailable, i18n.M("BALANCES_UNAVAILABLE"), nil)
		return "", false
	}
	return claims.UUID, true
}

// resolveAccount reads ?account=, or answers and reports false.
func (h *UserHandler) resolveAccount(ctx context.Context, c *gin.Context, userHK string) (balances.Account, bool) {
	a, err := h.Balances.Resolve(ctx, userHK, c.Query("account"))
	switch {
	case errors.Is(err, balances.ErrAccountNotFound):
		// Another user's sub-account, an unknown one, and a deleted one
		// answer alike.
		helpers.SendError(c, http.StatusNotFound, codeAccountNotFound, i18n.M("ACCOUNT_NOT_FOUND"), nil)
		return a, false
	case err != nil:
		log.Printf("Failed to read the account of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("INTERNAL_SERVER_ERROR"), nil)
		return a, false
	}
	return a, true
}

// currencyParam reads ?currency=, a currency hk, into errs.
func currencyParam(c *gin.Context, errs map[string]string) string {
	raw := c.Query("currency")
	if raw == "" {
		return ""
	}
	if _, err := uuid.Parse(raw); err != nil {
		errs["currency"] = "must be the UUID of a currency"
	}
	return raw
}

// GetBalances returns the signed-in user's balances (#365).
//
// @Summary My balances
// @Description The balances of the signed-in user, grouped by account, one per currency: available, locked, pending_deposit, total (available plus locked), and version, which rises with every change, so that a client joins this snapshot with the stream of account.balances (#66) by applying only the events of a higher version. account selects the main account (the default), a sub-account of the user by its ID, or all, every account of the user. A sub-account of another user, an unknown one, and a deleted one all answer 404 account_not_found. currency filters to one currency. A currency without a balance row is left out: show it as zero. Amounts are decimal strings. Each balance is checked against its row seal (#139); one that fails is not returned, its finding is recorded, and the answer is 503 balance_unverified with info.currency_hk.
// @Tags user-balances
// @Produce json
// @Security BearerAuth
// @Param account query string false "main (the default), all, or the ID of a sub-account"
// @Param currency query string false "A currency hk" format(uuid)
// @Success 200 {object} models.APIResponse{info=BalancesInfo}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "account_not_found"
// @Failure 503 {object} models.APIResponse "balance_unverified, with info.currency_hk, or balances_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/balances [get]
func (h *UserHandler) GetBalances(c *gin.Context) {
	userHK, ok := h.balancesCaller(c)
	if !ok {
		return
	}
	errs := map[string]string{}
	currency := currencyParam(c, errs)
	if len(errs) > 0 {
		sendValidation(c, errs)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	a, ok := h.resolveAccount(ctx, c, userHK)
	if !ok {
		return
	}
	list, err := h.Balances.Balances(ctx, userHK, a, currency)
	if !h.balanceError(c, userHK, err) {
		return
	}
	info := BalancesInfo{Accounts: []AccountBalances{}}
	for _, b := range list {
		n := len(info.Accounts)
		if n == 0 || !sameAccount(info.Accounts[n-1].SubAccountID, b.SubAccountID) {
			info.Accounts = append(info.Accounts, AccountBalances{SubAccountID: b.SubAccountID, Balances: []balances.Balance{}})
			n++
		}
		info.Accounts[n-1].Balances = append(info.Accounts[n-1].Balances, b)
	}
	if len(info.Accounts) == 0 && !a.All {
		var sub *string
		if a.Sub != nil {
			id := a.Sub.String()
			sub = &id
		}
		info.Accounts = append(info.Accounts, AccountBalances{SubAccountID: sub, Balances: []balances.Balance{}})
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("BALANCES_RETRIEVED"), info)
}

func sameAccount(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// balanceError answers err, and reports whether there was none.
func (h *UserHandler) balanceError(c *gin.Context, userHK string, err error) bool {
	var unverified *balances.Unverified
	switch {
	case err == nil:
		return true
	case errors.As(err, &unverified):
		helpers.SendError(c, http.StatusServiceUnavailable, codeBalanceUnverified, i18n.M("BALANCE_UNVERIFIED"),
			gin.H{"currency_hk": unverified.CurrencyHK})
	default:
		log.Printf("Failed to read the balances of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("INTERNAL_SERVER_ERROR"), nil)
	}
	return false
}

// ListTransactions returns the history of the signed-in user's balances
// (#365).
//
// @Summary My transaction history
// @Description Every change of the signed-in user's balances, newest first: its id, the entry that made it (entry_id) and its kind, the change_type, the currency, the amount, the available, locked, and pending_deposit amounts after it, the account (sub_account_id, absent for the main account), what caused it (reference, such as a withdrawal and its ID), and its time. account selects the main account (the default), a sub-account by its ID, or all; a sub-account of another user, an unknown one, and a deleted one answer 404 account_not_found. currency, kind, since (inclusive), and until (exclusive) filter it. Pages follow the pagination convention of docs/api.md; the cursor holds the time and the id of a change. The changes of one entry share its time.
// @Tags user-balances
// @Produce json
// @Security BearerAuth
// @Param account query string false "main (the default), all, or the ID of a sub-account"
// @Param currency query string false "A currency hk" format(uuid)
// @Param kind query string false "The kind of the entry" Enums(deposit_detected,deposit_credited,deposit_failed,lock,release,transfer,trade,withdrawal,conversion)
// @Param since query string false "From this time, inclusive (RFC 3339)" format(date-time)
// @Param until query string false "Before this time, exclusive (RFC 3339)" format(date-time)
// @Param limit query int false "The number of changes, 50 by default" minimum(1) maximum(200)
// @Param cursor query string false "The next_cursor of the previous page"
// @Success 200 {object} models.APIResponse{info=TransactionPage}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "account_not_found"
// @Failure 503 {object} models.APIResponse "balances_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/transactions [get]
func (h *UserHandler) ListTransactions(c *gin.Context) {
	userHK, ok := h.balancesCaller(c)
	if !ok {
		return
	}
	page, errs := pagination.Parse(c)
	if errs == nil {
		errs = map[string]string{}
	}
	f := balances.Filter{CurrencyHK: currencyParam(c, errs), Kind: c.Query("kind")}
	if f.Kind != "" && !slices.Contains(balances.Kinds, f.Kind) {
		errs["kind"] = "is not a kind of entry"
	}
	for name, into := range map[string]*time.Time{"since": &f.Since, "until": &f.Until} {
		if raw := c.Query(name); raw != "" {
			t, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				errs[name] = "must be an RFC 3339 time"
			}
			*into = t
		}
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && !f.Since.Before(f.Until) {
		errs["until"] = "must be after since"
	}
	var after balances.Position
	resumed, err := page.After(&after)
	if err != nil || (resumed && (after.Time.IsZero() || uuid.Validate(after.ID) != nil)) {
		errs["cursor"] = "is not a cursor of this list"
	}
	if len(errs) > 0 {
		sendValidation(c, errs)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	if f.Account, ok = h.resolveAccount(ctx, c, userHK); !ok {
		return
	}
	var from *balances.Position
	if resumed {
		from = &after
	}
	list, err := h.Balances.History(ctx, userHK, f, from, page.Limit+1)
	if err != nil {
		log.Printf("Failed to read the history of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("INTERNAL_SERVER_ERROR"), nil)
		return
	}
	result, err := pagination.NewPage(list, page, func(ch balances.Change) interface{} {
		return balances.Position{Time: ch.Time, ID: ch.ID}
	})
	if err != nil {
		log.Printf("Failed to page the history of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("INTERNAL_SERVER_ERROR"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("TRANSACTIONS_RETRIEVED"), result)
}

// sendValidation answers 400 validation_failed with errs.
func sendValidation(c *gin.Context, errs map[string]string) {
	helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": errs})
}
