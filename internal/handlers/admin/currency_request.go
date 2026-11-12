package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
)

// defaultCurrencyStatus is the status of a currency created without one.
const defaultCurrencyStatus = "pending"

const maxCurrencyRequestBody = 64 << 10

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
// It is an error, so that an edit of a store can refuse a request with it.
type fieldErrors map[string]string

func (f fieldErrors) Error() string { return fmt.Sprintf("invalid fields: %v", map[string]string(f)) }

func sendValidationErrors(c *gin.Context, errs fieldErrors) {
	helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": errs})
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
// with an update applied; see currencies.Validate.
func validateCurrency(status string, info models.CurrencyInfo) fieldErrors {
	if errs := currencies.Validate(status, info); errs != nil {
		return fieldErrors(errs)
	}
	return nil
}

// sendVersionConflict answers a change that is based on a version of the
// currency that is no longer the current one.
func sendVersionConflict(c *gin.Context, current int64) {
	helpers.SendError(c, http.StatusConflict, "version_conflict", i18n.M("CURRENCY_VERSION_CONFLICT"),
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

// currencyWriteFailures are the messages of an unexpected failure, by
// action.
var currencyWriteFailures = map[string]i18n.Message{
	"create": i18n.M("CURRENCY_CREATE_FAILED"),
	"update": i18n.M("CURRENCY_UPDATE_FAILED"),
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
					i18n.M("SYMBOL_RESERVED"),
					gin.H{"code": "symbol_reserved", "field": field})
				return
			}
			// The indexes include deleted currencies, so that a symbol keeps
			// naming one hk for clients that cached it (#196): a deleted one
			// is restored, not created again.
			helpers.SendError(c, http.StatusConflict, code,
				i18n.M("CURRENCY_FIELD_EXISTS", i18n.A("field", i18n.Key("CURRENCY_FIELD_"+strings.ToUpper(what)))),
				gin.H{"code": code, "field": field})
			return
		case "23514", "22P02": // check_violation, invalid_text_representation
			sendValidationErrors(c, fieldErrors{"info": "does not satisfy the currency constraints"})
			return
		}
	}
	sendUnrecordedOr(c, err, action, currencyWriteFailures[action])
}
