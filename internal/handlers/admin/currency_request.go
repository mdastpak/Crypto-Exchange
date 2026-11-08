package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
)

// defaultCurrencyStatus is the status of a currency created without one.
const defaultCurrencyStatus = "pending"

// currencyStatuses are the statuses an administrator can set. A currency is
// only marked deleted by DELETE /admin/currencies/{hk}.
var currencyStatuses = map[string]bool{"approved": true, "pending": true, "suspend": true}

var (
	symbolPattern   = regexp.MustCompile(`^[A-Z0-9]{2,15}$`)
	contractPattern = regexp.MustCompile(`^[A-Za-z0-9:_.-]{1,128}$`)
)

const (
	maxCurrencyNameLength  = 100
	maxLogoURL             = 2048
	maxCurrencyRequestBody = 64 << 10
)

// currencyInfoFields is the info object of a currency request. Pointers tell
// an omitted field from an empty or false one, so that an update changes only
// the fields it sends, and can set a flag back to false.
type currencyInfoFields struct {
	EnName   *string `json:"en_name"`
	FaName   *string `json:"fa_name"`
	IfFiat   *bool   `json:"if_fiat"`
	Symbol   *string `json:"symbol"`
	IsToken  *bool   `json:"is_token"`
	Contract *string `json:"contract"`
	LogoURL  *string `json:"logo_url"`
}

// currencyRequest is the body of POST /admin/currencies and
// PUT /admin/currencies/{hk}. The server-controlled fields (id, hk, and the
// timestamps) are not part of it, and decodeCurrencyRequest rejects them.
type currencyRequest struct {
	Status *string            `json:"status"`
	Info   currencyInfoFields `json:"info"`
	// Version is the version of the currency that an update is based on.
	// When it is sent and the currency has changed since, the update is
	// refused with 409. A create may not send it.
	Version *int64 `json:"version"`
}

// fieldErrors maps a field, such as "info.symbol", to what is wrong with it.
type fieldErrors map[string]string

func sendValidationErrors(c *gin.Context, errs fieldErrors) {
	helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, "Validation failed", gin.H{"errors": errs})
}

// errMalformedCurrencyRequest reports a body that is not a JSON object of the
// expected shape.
var errMalformedCurrencyRequest = errors.New("malformed currency request")

// decodeCurrencyRequest decodes the body strictly. An unknown field, such as
// a server-controlled id or hk, is a field error, named by its path: a field
// inside info is reported as "info.<name>". Other decoding failures return
// errMalformedCurrencyRequest.
//
// encoding/json names an unknown field without its parent, so info is
// decoded on its own, after the top level, and its unknown fields get the
// "info." prefix here.
func decodeCurrencyRequest(c *gin.Context) (currencyRequest, fieldErrors, error) {
	var top struct {
		Status  *string         `json:"status"`
		Info    json.RawMessage `json:"info"`
		Version *int64          `json:"version"`
	}
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, maxCurrencyRequestBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&top); err != nil {
		if name, ok := unknownField(err); ok {
			return currencyRequest{}, fieldErrors{name: "is not accepted"}, nil
		}
		return currencyRequest{}, nil, errMalformedCurrencyRequest
	}
	if decoder.More() {
		return currencyRequest{}, nil, errMalformedCurrencyRequest
	}
	req := currencyRequest{Status: top.Status, Version: top.Version}
	if len(top.Info) > 0 {
		info := json.NewDecoder(bytes.NewReader(top.Info))
		info.DisallowUnknownFields()
		if err := info.Decode(&req.Info); err != nil {
			if name, ok := unknownField(err); ok {
				return req, fieldErrors{"info." + name: "is not accepted"}, nil
			}
			return req, nil, errMalformedCurrencyRequest
		}
	}
	return req, nil, nil
}

// unknownField returns the name of the field that a decoder with
// DisallowUnknownFields refused, or false for any other error.
func unknownField(err error) (string, bool) {
	name, ok := strings.CutPrefix(err.Error(), "json: unknown field ")
	if !ok {
		return "", false
	}
	return strings.Trim(name, `"`), true
}

// apply returns status and info with the fields of the request applied.
// Text is trimmed, and the symbol is converted to upper case, so that a
// symbol is unique whatever case the client sends.
func (req currencyRequest) apply(status string, info models.CurrencyInfo) (string, models.CurrencyInfo) {
	if req.Status != nil {
		status = strings.TrimSpace(*req.Status)
	}
	setText := func(dst *string, src *string) {
		if src != nil {
			*dst = strings.TrimSpace(*src)
		}
	}
	setText(&info.EnName, req.Info.EnName)
	setText(&info.FaName, req.Info.FaName)
	setText(&info.Contract, req.Info.Contract)
	setText(&info.LogoURL, req.Info.LogoURL)
	if req.Info.Symbol != nil {
		info.Symbol = strings.ToUpper(strings.TrimSpace(*req.Info.Symbol))
	}
	if req.Info.IfFiat != nil {
		info.IfFiat = *req.Info.IfFiat
	}
	if req.Info.IsToken != nil {
		info.IsToken = *req.Info.IsToken
	}
	return status, info
}

// validateCurrency checks a complete currency: a new one, or a stored one
// with an update applied.
func validateCurrency(status string, info models.CurrencyInfo) fieldErrors {
	errs := fieldErrors{}
	if !currencyStatuses[status] {
		errs["status"] = "must be approved, pending, or suspend"
	}
	checkName := func(field, value string) {
		switch {
		case value == "":
			errs[field] = "is required"
		case utf8.RuneCountInString(value) > maxCurrencyNameLength:
			errs[field] = "must be at most 100 characters"
		}
	}
	checkName("info.en_name", info.EnName)
	checkName("info.fa_name", info.FaName)
	if !symbolPattern.MatchString(info.Symbol) {
		errs["info.symbol"] = "must be 2 to 15 upper-case letters or digits"
	}
	switch {
	case info.IsToken && info.Contract == "":
		errs["info.contract"] = "is required for a token"
	case info.Contract != "" && !contractPattern.MatchString(info.Contract):
		errs["info.contract"] = "must be 1 to 128 letters, digits, or ':', '_', '.', '-'"
	}
	if info.LogoURL != "" {
		u, err := url.Parse(info.LogoURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(info.LogoURL) > maxLogoURL {
			errs["info.logo_url"] = "must be an https URL of at most 2048 characters"
		}
	}
	if info.IfFiat && info.IsToken {
		errs["info.is_token"] = "must be false for a fiat currency"
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// sendVersionConflict answers a change that is based on a version of the
// currency that is no longer the current one.
func sendVersionConflict(c *gin.Context, current int64) {
	helpers.SendError(c, http.StatusConflict, "version_conflict", "The currency has changed since that version; read it again and retry.",
		gin.H{"code": "version_conflict", "version": current})
}

// versionQuery reads the optional version query parameter of a delete. ok
// is false when it is set but not a number of at least 1.
func versionQuery(c *gin.Context) (version *int64, ok bool) {
	raw, present := c.GetQuery("version")
	if !present {
		return nil, true
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 1 {
		return nil, false
	}
	return &v, true
}

// sendCurrencyWriteError answers a failed insert or update. PostgreSQL errors
// arrive from the pgx driver as *pgconn.PgError and are told apart by their
// SQLSTATE code.
func sendCurrencyWriteError(c *gin.Context, err error, action string) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			field, code, what := "info.symbol", "duplicate_symbol", "symbol"
			if strings.Contains(pgErr.ConstraintName, "contract") {
				field, code, what = "info.contract", "duplicate_contract", "contract"
			}
			if pgErr.ConstraintName == "currency_symbols_reserved" {
				// A previous symbol of another currency, after a rebrand
				// (#215).
				helpers.SendError(c, http.StatusConflict, "symbol_reserved",
					"This symbol was the symbol of another currency, which keeps it until an administrator releases it.",
					gin.H{"code": "symbol_reserved", "field": field})
				return
			}
			// The indexes include deleted currencies, so that a symbol keeps
			// naming one hk for clients that cached it (#196): a deleted one
			// is restored, not created again.
			helpers.SendError(c, http.StatusConflict, code,
				"A currency with this "+what+" already exists. If it was deleted, restore it instead.",
				gin.H{"code": code, "field": field})
			return
		case "23514", "22P02": // check_violation, invalid_text_representation
			sendValidationErrors(c, fieldErrors{"info": "does not satisfy the currency constraints"})
			return
		}
	}
	log.Printf("Error trying to %s a currency: %v", action, err)
	helpers.SendResponse(c, http.StatusInternalServerError, "Failed to "+action+" currency due to an unexpected error.", nil)
}
