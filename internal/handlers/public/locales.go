package public

import (
	"net/http"

	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/gin-gonic/gin"
)

// LocalesInfo is the answer of GET /public/locales.
type LocalesInfo struct {
	// Default is the code of the fallback locale.
	Default string `json:"default" example:"en"`
	// Current is the locale of this request, for <html lang dir>.
	Current i18n.Locale `json:"current"`
	// Source is where the current locale came from: query, header, cookie,
	// accept_language, site (the default of the site the request comes
	// from), or default.
	Source string `json:"source" example:"accept_language" enums:"query,header,cookie,accept_language,site,default"`
	// Locales are the supported locales, in the order of the language
	// switcher.
	Locales []i18n.Locale `json:"locales"`
}

// Locales answers GET /public/locales from the registry the server was
// started with.
//
// @Summary List the supported languages and their text direction
// @Description The locales the product supports, each with its names and its direction (ltr or rtl), the default, and the locale this request negotiated: from the lang query parameter, the X-Locale header, the locale cookie, Accept-Language, or the default, in that order (#283). Frontends build the language switcher from it and set lang and dir on the root element from current.
// @Tags system
// @Produce json
// @Param lang query string false "A locale for this request, such as fa"
// @Param X-Locale header string false "The locale an app chose"
// @Success 200 {object} models.APIResponse{info=LocalesInfo}
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/locales [get]
func Locales(registry *i18n.Registry) gin.HandlerFunc {
	return func(c *gin.Context) {
		n, ok := i18n.FromGin(c)
		if !ok {
			n = registry.Negotiate(c.Request)
		}
		// The answer names the visitor's locale, so no shared cache may
		// keep it.
		c.Header("Cache-Control", "private, no-store")
		helpers.SendResponse(c, http.StatusOK, i18n.M("LOCALES_RETRIEVED_SUCCESSFULLY"), LocalesInfo{
			Default: registry.Default().Code,
			Current: n.Locale,
			Source:  n.Source,
			Locales: registry.Locales(),
		})
	}
}
