package currencies

import (
	"net/url"
	"regexp"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/models"
)

// settableStatuses are the statuses an administrator can set. A currency
// is only marked deleted by a delete.
var settableStatuses = map[string]bool{
	models.CurrencyStatusApproved: true, models.CurrencyStatusPending: true, models.CurrencyStatusSuspend: true,
}

var (
	// SymbolPattern is the form of a symbol.
	SymbolPattern   = regexp.MustCompile(`^[A-Z0-9]{2,15}$`)
	contractPattern = regexp.MustCompile(`^[A-Za-z0-9:_.-]{1,128}$`)
)

const (
	maxNameLength = 100
	maxLogoURL    = 2048
)

// Validate checks a complete currency, a new one or a stored one with a
// change applied, and returns the problems by field, such as
// "info.symbol", or nil. The admin API and the import of the registry file
// check a currency the same way.
func Validate(status string, info models.CurrencyInfo) map[string]string {
	errs := map[string]string{}
	if !settableStatuses[status] {
		errs["status"] = "must be approved, pending, or suspend"
	}
	checkName := func(field, value string) {
		switch {
		case value == "":
			errs[field] = "is required"
		case utf8.RuneCountInString(value) > maxNameLength:
			errs[field] = "must be at most 100 characters"
		}
	}
	checkName("info.en_name", info.EnName)
	checkName("info.fa_name", info.FaName)
	if !SymbolPattern.MatchString(info.Symbol) {
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
