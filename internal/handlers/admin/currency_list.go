package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/pagination"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// listableStatuses are the statuses that the admin list can filter by:
// those an administrator sets, and deleted.
var listableStatuses = map[string]bool{
	models.CurrencyStatusApproved: true, models.CurrencyStatusPending: true,
	models.CurrencyStatusSuspend: true, models.CurrencyStatusDeleted: true,
}

// CurrencyPage is a page of currencies. It describes
// pagination.Page[models.Currency] in the OpenAPI document, which swag
// cannot read from the generic type.
type CurrencyPage struct {
	Items []models.Currency `json:"items"`
	// NextCursor asks for the next page. It is left out on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// currencyFilter selects currencies. An empty field selects any value.
type currencyFilter struct {
	Status          string
	Symbol          string
	IsToken, IfFiat *bool
}

// currencyPosition is the cursor of the admin list: the creation time and
// the hk of the last currency of a page. Both are public to administrators.
type currencyPosition struct {
	CreatedAt time.Time `json:"c"`
	HK        string    `json:"h"`
}

// listedCurrency is a currency of the list with its exact creation time,
// which the cursor needs and the response rounds to the second.
type listedCurrency struct {
	models.Currency
	created time.Time
}

// ListCurrencies returns the currencies of every status, newest first, a
// page at a time.
//
// @Summary List currencies
// @Description Every currency, whatever its status, deleted ones included, newest first, with its version. Read from the database, not the public cache. The filters are optional and combine. Pages follow the pagination convention of docs/api.md.
// @Tags admin currencies
// @Produce json
// @Security BearerAuth
// @Param status query string false "The status" Enums(approved, pending, suspend, deleted)
// @Param symbol query string false "The symbol, in any case"
// @Param is_token query bool false "Only tokens, or only currencies that are not"
// @Param if_fiat query bool false "Only fiat currencies, or only those that are not"
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
	filter, filterErrs := parseCurrencyFilter(c)
	if errs == nil {
		errs = map[string]string{}
	}
	for field, problem := range filterErrs {
		errs[field] = problem
	}
	if len(errs) > 0 {
		sendValidationErrors(c, errs)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	result, err := listCurrencies(ctx, h.PostgresDB.DB, filter, page)
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

// parseCurrencyFilter reads the filters of the list, and the problems of
// those that are invalid.
func parseCurrencyFilter(c *gin.Context) (currencyFilter, fieldErrors) {
	var f currencyFilter
	errs := fieldErrors{}
	if raw := c.Query("status"); raw != "" {
		if !listableStatuses[raw] {
			errs["status"] = "must be approved, pending, suspend, or deleted"
		}
		f.Status = raw
	}
	if raw := strings.TrimSpace(c.Query("symbol")); raw != "" {
		if len(raw) > 15 {
			errs["symbol"] = "must be at most 15 characters"
		}
		// Symbols are stored in upper case, as a create normalizes them.
		f.Symbol = strings.ToUpper(raw)
	}
	for name, flag := range map[string]**bool{"is_token": &f.IsToken, "if_fiat": &f.IfFiat} {
		if raw := c.Query(name); raw != "" {
			v, err := strconv.ParseBool(raw)
			if err != nil {
				errs[name] = "must be true or false"
				continue
			}
			*flag = &v
		}
	}
	if len(errs) == 0 {
		return f, nil
	}
	return f, errs
}

// listCurrencies returns a page of the currencies that f selects. The keyset
// on (created_at, hk) uses idx_currencies_created_hk, so that every page
// costs one range scan however deep it is, and any instance can serve the
// next page, since the cursor holds the whole position.
func listCurrencies(ctx context.Context, db *sql.DB, f currencyFilter, page pagination.Request) (pagination.Page[models.Currency], error) {
	var where []string
	var args []interface{}
	add := func(condition string, values ...interface{}) {
		numbers := make([]interface{}, len(values))
		for i, v := range values {
			args = append(args, v)
			numbers[i] = len(args)
		}
		where = append(where, fmt.Sprintf(condition, numbers...))
	}
	if f.Status != "" {
		add("status = $%d::status_enum", f.Status)
	}
	if f.Symbol != "" {
		add("info->>'symbol' = $%d", f.Symbol)
	}
	if f.IsToken != nil {
		add("(info->>'is_token')::boolean = $%d", *f.IsToken)
	}
	if f.IfFiat != nil {
		add("(info->>'if_fiat')::boolean = $%d", *f.IfFiat)
	}
	var after currencyPosition
	more, err := page.After(&after)
	if err != nil {
		return pagination.Page[models.Currency]{}, err
	}
	if more {
		if _, err := uuid.Parse(after.HK); err != nil || after.CreatedAt.IsZero() {
			return pagination.Page[models.Currency]{}, pagination.ErrInvalidCursor
		}
		add("(created_at, hk) < ($%d, $%d)", after.CreatedAt, after.HK)
	}

	query := `SELECT id, hk, status, info, version, created_at, updated_at, deleted_at FROM currencies`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, page.Limit+1)
	query += fmt.Sprintf(" ORDER BY created_at DESC, hk DESC LIMIT $%d", len(args)) //nolint:gosec // G202: the query joins constant fragments and placeholder numbers only; every value is an argument.

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return pagination.Page[models.Currency]{}, err
	}
	defer rows.Close()
	var listed []listedCurrency
	for rows.Next() {
		var l listedCurrency
		var info []byte
		var updated time.Time
		var deleted sql.NullTime
		if err := rows.Scan(&l.ID, &l.HK, &l.Status, &info, &l.Version, &l.created, &updated, &deleted); err != nil {
			return pagination.Page[models.Currency]{}, err
		}
		if err := json.Unmarshal(info, &l.Info); err != nil {
			return pagination.Page[models.Currency]{}, fmt.Errorf("currency %s: %w", l.HK, err)
		}
		l.CreatedAt = timestamp(sql.NullTime{Time: l.created, Valid: true})
		l.UpdatedAt = timestamp(sql.NullTime{Time: updated, Valid: true})
		if deleted.Valid {
			at := timestamp(deleted)
			l.DeletedAt = &at
		}
		listed = append(listed, l)
	}
	if err := rows.Err(); err != nil {
		return pagination.Page[models.Currency]{}, err
	}

	built, err := pagination.NewPage(listed, page, func(l listedCurrency) interface{} {
		return currencyPosition{CreatedAt: l.created, HK: l.HK}
	})
	if err != nil {
		return pagination.Page[models.Currency]{}, err
	}
	result := pagination.Page[models.Currency]{Items: make([]models.Currency, len(built.Items)), NextCursor: built.NextCursor}
	for i, l := range built.Items {
		result.Items[i] = l.Currency
	}
	return result, nil
}
