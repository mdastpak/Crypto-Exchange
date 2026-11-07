package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/pagination"

	"github.com/gin-gonic/gin"
)

// CurrencyPage is a page of currencies. It describes
// pagination.Page[models.Currency] in the OpenAPI document, which swag
// cannot read from the generic type.
type CurrencyPage struct {
	Items []models.Currency `json:"items"`
	// NextCursor asks for the next page. It is left out on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// ListCurrencies returns the currencies of every status, newest first, a
// page at a time.
//
// @Summary List currencies
// @Description Every currency, whatever its status, deleted ones included, newest first by default, with its version. Read from the database, not the public cache. The filters are those of GET /public/currencies, plus status; they are optional and combine, and an unknown parameter is refused. Pages follow the pagination convention of docs/api.md.
// @Tags admin currencies
// @Produce json
// @Security BearerAuth
// @Param status query []string false "Statuses, repeated or comma-separated" collectionFormat(csv) Enums(approved, pending, suspend, deleted)
// @Param q query string false "Text in the symbol, English name, or Persian name, normalized; symbols that match it come first" maxLength(64)
// @Param symbol query []string false "Symbols, in any case, repeated or comma-separated" collectionFormat(csv)
// @Param en_name query string false "Text in the English name, in any case" maxLength(64)
// @Param fa_name query string false "Text in the Persian name, with Arabic letters and Persian or Arabic digits matching the Persian ones" maxLength(64)
// @Param contract query string false "The contract address, in any case" maxLength(128)
// @Param has_contract query bool false "Only currencies with a contract, or only those without"
// @Param is_token query bool false "Only tokens, or only currencies that are not"
// @Param if_fiat query bool false "Only fiat currencies, or only those that are not"
// @Param created_since query string false "Created at or after this RFC 3339 time" format(date-time)
// @Param created_until query string false "Created before this RFC 3339 time" format(date-time)
// @Param updated_since query string false "Updated at or after this RFC 3339 time" format(date-time)
// @Param sort query string false "The order, with - for descending; the hk breaks ties" Enums(symbol, -symbol, en_name, -en_name, created_at, -created_at, updated_at, -updated_at)
// @Param limit query int false "The number of currencies, 50 by default" minimum(1) maximum(200)
// @Param cursor query string false "The next_cursor of the previous page"
// @Success 200 {object} models.APIResponse{info=CurrencyPage}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/currencies [get]
func (h *AdminHandler) ListCurrencies(c *gin.Context) {
	page, errs := pagination.Parse(c)
	query, queryErrs := currencies.ParseQuery(c.Request.URL.Query(), "status")
	if errs == nil {
		errs = map[string]string{}
	}
	for field, problem := range queryErrs {
		errs[field] = problem
	}
	if len(errs) > 0 {
		sendValidationErrors(c, errs)
		return
	}
	query.DefaultDesc = true

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	all, err := allCurrencies(ctx, h.PostgresDB.DB)
	if err != nil {
		log.Printf("Error listing currencies: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to list currencies.", nil)
		return
	}
	result, err := query.Page(all, page)
	if errors.Is(err, pagination.ErrInvalidCursor) {
		sendValidationErrors(c, fieldErrors{"cursor": "is not a cursor of this list"})
		return
	}
	if err != nil {
		log.Printf("Error listing currencies: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to list currencies.", nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, "Currencies retrieved successfully.", result)
}

// allCurrencies reads every currency, deleted ones included, for the
// filters of the list to run over. Currencies created in the same second
// are ordered by hk, so the order is total and a cursor never skips one.
const allCurrenciesQuery = `SELECT c.id, c.hk, c.status, c.info, c.version, c.created_at, c.updated_at, c.deleted_at, ` +
	currencies.DetailColumns + ` FROM currencies c`

func allCurrencies(ctx context.Context, db *sql.DB) ([]models.Currency, error) {
	rows, err := db.QueryContext(ctx, allCurrenciesQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []models.Currency
	for rows.Next() {
		var cur models.Currency
		var info []byte
		var created, updated time.Time
		var deleted sql.NullTime
		var lifecycle currencies.DetailScan
		if err := rows.Scan(append([]any{&cur.ID, &cur.HK, &cur.Status, &info, &cur.Version, &created, &updated, &deleted}, lifecycle.Dest()...)...); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(info, &cur.Info); err != nil {
			return nil, fmt.Errorf("currency %s: %w", cur.HK, err)
		}
		var err error
		if err = lifecycle.Apply(&cur); err != nil {
			return nil, fmt.Errorf("currency %s: %w", cur.HK, err)
		}
		cur.CreatedAt = timestamp(sql.NullTime{Time: created, Valid: true})
		cur.UpdatedAt = timestamp(sql.NullTime{Time: updated, Valid: true})
		if deleted.Valid {
			at := timestamp(deleted)
			cur.DeletedAt = &at
		}
		list = append(list, cur)
	}
	return list, rows.Err()
}
