package user

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/agreements"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/gin-gonic/gin"
)

// The legal documents of a signed-in user (#316). A new version of a
// document closes the changes of every user who has not accepted it, until
// they do; reading stays open, so that the client can show what is asked.

const codeTermsUpdate = "terms_update_required"

// AgreementsInfo is the documents that the user is asked to accept.
type AgreementsInfo struct {
	// Documents are the required documents in their current versions.
	Documents []agreements.Document `json:"documents"`
	// Pending are those whose current version the user has not accepted.
	// While it is not empty, the changes of the user answer 403
	// terms_update_required.
	Pending []agreements.Document `json:"pending"`
}

// AcceptAgreementsRequest accepts documents.
type AcceptAgreementsRequest struct {
	// Accept are the versions that the user accepted, by kind. It must
	// have the current version of every required document.
	Accept map[string]string `json:"accept" example:"terms:2026-11,privacy:2026-11"`
}

// country returns the country of a request, or "".
func (h *UserHandler) country(c *gin.Context) string {
	if h.GeoIP == nil {
		return ""
	}
	return h.GeoIP.Resolve(c).Country
}

// agreementsOf answers with the documents of a user.
func (h *UserHandler) agreementsOf(c *gin.Context, ctx context.Context, userHK string) {
	pending, err := h.Agreements.Missing(ctx, userHK)
	if err != nil {
		log.Printf("Failed to read the agreements of a user: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_AGREEMENTS"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("AGREEMENTS"), AgreementsInfo{Documents: agreements.Required(h.settings().Legal), Pending: pending})
}

// @Summary List the legal documents of the user
// @Description The documents that legal.documents requires, in their current versions, and those of them that the user has not accepted yet. While any is pending, every change of the user but POST /user/agreements, signing out, and ending sessions answers 403 terms_update_required.
// @Tags user-agreements
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=AgreementsInfo}
// @Failure 401 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/agreements [get]
func (h *UserHandler) GetAgreements(c *gin.Context) {
	claims, ok := sessionCaller(c)
	if !ok {
		return
	}
	if h.Agreements == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("FAILED_TO_READ_AGREEMENTS"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	h.agreementsOf(c, ctx, claims.UUID)
}

// @Summary Accept the legal documents
// @Description Records that the user accepted the current version of every required document, with the time, the address, the user agent, and the country of the request. accept has the version of each document by kind; one that misses a document or names another version answers 409 terms_update_required with the pending documents, and records nothing. A version that the user already accepted keeps its first record.
// @Tags user-agreements
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body AcceptAgreementsRequest true "The accepted versions"
// @Success 200 {object} models.APIResponse{info=AgreementsInfo}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse{info=AgreementsInfo} "terms_update_required"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/agreements [post]
func (h *UserHandler) AcceptAgreements(c *gin.Context) {
	claims, ok := sessionCaller(c)
	if !ok {
		return
	}
	var req AcceptAgreementsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	if h.Agreements == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("FAILED_TO_READ_AGREEMENTS"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	legal := h.settings().Legal
	if !agreements.Covers(legal, req.Accept) {
		pending, err := h.Agreements.Missing(ctx, claims.UUID)
		if err != nil {
			log.Printf("Failed to read the agreements of a user: %v", err)
			helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_AGREEMENTS"), nil)
			return
		}
		helpers.SendError(c, http.StatusConflict, codeTermsUpdate, i18n.M("TERMS_UPDATE_REQUIRED"),
			AgreementsInfo{Documents: agreements.Required(h.settings().Legal), Pending: pending})
		return
	}
	origin := agreements.Origin{Country: h.country(c), Event: audit.FromRequest(c)}
	if err := h.Agreements.Accept(ctx, claims.UUID, req.Accept, origin); err != nil {
		log.Printf("Failed to record agreements: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_RECORD_AGREEMENTS"), nil)
		return
	}
	h.agreementsOf(c, ctx, claims.UUID)
}

// RequireAgreements refuses the changes of a user, every method but GET,
// HEAD, and OPTIONS, while a required document waits for their
// acceptance. The routes registered before it stay open. Without required
// documents it reads nothing.
func (h *UserHandler) RequireAgreements() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		if len(agreements.Required(h.settings().Legal)) == 0 || h.Agreements == nil {
			c.Next()
			return
		}
		claims, ok := sessionCaller(c)
		if !ok {
			c.Abort()
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		defer cancel()
		pending, err := h.Agreements.Missing(ctx, claims.UUID)
		if err != nil {
			// Fail closed: a change is not let through unchecked.
			log.Printf("Failed to read the agreements of a user: %v", err)
			helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("FAILED_TO_READ_AGREEMENTS"), nil)
			c.Abort()
			return
		}
		if len(pending) > 0 {
			helpers.SendError(c, http.StatusForbidden, codeTermsUpdate, i18n.M("TERMS_UPDATE_REQUIRED"),
				AgreementsInfo{Documents: agreements.Required(h.settings().Legal), Pending: pending})
			c.Abort()
			return
		}
		c.Next()
	}
}
