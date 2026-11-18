package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/kyc"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/verification"
)

// The KYC review (#72): administrators read the applications that wait,
// with their form and documents, and approve them, which grants level 2,
// or reject them with a reason that the user reads.

const (
	codeKYCUnavailable    = "kyc_unavailable"
	codeKYCNotFound       = "kyc_application_not_found"
	codeKYCNotUnderReview = "kyc_application_not_under_review"
	codeKYCLevelRequired  = "identity_verification_required"
	codeKYCLinkInvalid    = "kyc_link_invalid"
	codeKYCChanged        = "kyc_document_changed"
)

// kycDecision is an approval or a rejection.
type kycDecision struct {
	// Reason, for a rejection, is what the user reads, 1 to 500
	// characters.
	Reason string `json:"reason,omitempty" example:"The selfie is blurred; please take it again in daylight."`
	// Notes are for the administrators, at most 1000 characters.
	Notes string `json:"notes,omitempty" example:"Address matches the utility bill."`
}

func (h *AdminHandler) kycConfigured(c *gin.Context) bool {
	if h.KYC == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codeKYCUnavailable, i18n.M("KYC_UNAVAILABLE"), nil)
		return false
	}
	return true
}

// @Summary List KYC applications
// @Description The KYC applications of a status (#72), UNDER_REVIEW by default, the oldest submitted first, at most limit (100). Each has the user's user_hk and its documents, without their content.
// @Tags admin kyc
// @Produce json
// @Security BearerAuth
// @Param status query string false "A status" Enums(PENDING,UNDER_REVIEW,APPROVED,REJECTED)
// @Param limit query int false "1 to 100; 100 by default"
// @Success 200 {object} models.APIResponse{info=[]kyc.Application}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "row_unverified"
// @Failure 503 {object} models.APIResponse "kyc_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/kyc/applications [get]
func (h *AdminHandler) ListKYCApplications(c *gin.Context) {
	if !h.kycConfigured(c) {
		return
	}
	problems := fieldErrors{}
	status := kyc.Status(c.Query("status"))
	switch status {
	case "", kyc.StatusPending, kyc.StatusUnderReview, kyc.StatusApproved, kyc.StatusRejected:
	default:
		problems["status"] = "is not a status of an application"
	}
	limit := kyc.MaxList
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > kyc.MaxList {
			problems["limit"] = "must be from 1 to 100"
		}
		limit = n
	}
	if len(problems) > 0 {
		sendValidationErrors(c, problems)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	list, err := h.KYC.Queue(ctx, status, limit)
	if err != nil {
		sendKYCError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("KYC_APPLICATIONS_RETRIEVED"), list)
}

// @Summary Get a KYC application
// @Description A KYC application with its form, decrypted, its documents, and its review (#72). Each reading is audited as admin.kyc_view, as it shows personal data.
// @Tags admin kyc
// @Produce json
// @Security BearerAuth
// @Param id path string true "The application" format(uuid)
// @Success 200 {object} models.APIResponse{info=kyc.Review}
// @Failure 401 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "kyc_application_not_found"
// @Failure 409 {object} models.APIResponse "row_unverified"
// @Failure 503 {object} models.APIResponse "kyc_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/kyc/applications/{id} [get]
func (h *AdminHandler) GetKYCApplication(c *gin.Context) {
	if !h.kycConfigured(c) {
		return
	}
	origin, _, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	r, err := h.KYC.Detail(ctx, c.Param("id"), origin)
	if err != nil {
		sendKYCError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	helpers.SendResponse(c, http.StatusOK, i18n.M("KYC_APPLICATION_RETRIEVED"), r)
}

// @Summary Approve a KYC application
// @Description Approves an application under review (#72), and raises the user to level 2 in the same transaction (audited as user.verification_set and admin.kyc_approve). A user who lost level 1 meanwhile is not raised (403 identity_verification_required). A sensitive route: it needs a step-up.
// @Tags admin kyc
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "The application" format(uuid)
// @Param body body kycDecision false "Notes"
// @Success 200 {object} models.APIResponse{info=kyc.Application}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required or identity_verification_required"
// @Failure 404 {object} models.APIResponse "kyc_application_not_found"
// @Failure 409 {object} models.APIResponse "kyc_application_not_under_review or row_unverified"
// @Failure 503 {object} models.APIResponse "kyc_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/kyc/applications/{id}/approve [post]
func (h *AdminHandler) ApproveKYCApplication(c *gin.Context) {
	h.decideKYC(c, true)
}

// @Summary Reject a KYC application
// @Description Rejects an application under review with a reason that the user reads (#72); the user may apply again. A sensitive route: it needs a step-up. Audited as admin.kyc_reject.
// @Tags admin kyc
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "The application" format(uuid)
// @Param body body kycDecision true "The reason"
// @Success 200 {object} models.APIResponse{info=kyc.Application}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 404 {object} models.APIResponse "kyc_application_not_found"
// @Failure 409 {object} models.APIResponse "kyc_application_not_under_review or row_unverified"
// @Failure 503 {object} models.APIResponse "kyc_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/kyc/applications/{id}/reject [post]
func (h *AdminHandler) RejectKYCApplication(c *gin.Context) {
	h.decideKYC(c, false)
}

func (h *AdminHandler) decideKYC(c *gin.Context, approve bool) {
	if !h.kycConfigured(c) {
		return
	}
	var req kycDecision
	if c.Request.ContentLength != 0 && !decodeStrict(c, &req) {
		return
	}
	origin, adminHK, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	var a kyc.Application
	var err error
	if approve {
		a, err = h.KYC.Approve(ctx, c.Param("id"), adminHK, req.Notes, origin)
	} else {
		a, err = h.KYC.Reject(ctx, c.Param("id"), adminHK, req.Reason, req.Notes, origin)
	}
	if err != nil {
		sendKYCError(c, err)
		return
	}
	message := i18n.M("KYC_APPLICATION_APPROVED")
	if !approve {
		message = i18n.M("KYC_APPLICATION_REJECTED")
	}
	helpers.SendResponse(c, http.StatusOK, message, a)
}

// @Summary Get a link to a KYC document
// @Description A link to the content of a document (#72), signed for the administrator for a minute, to open in a browser without the token. The link is checked, the administrator must still be active, and each opening is audited as admin.kyc_document_view.
// @Tags admin kyc
// @Produce json
// @Security BearerAuth
// @Param id path string true "The document" format(uuid)
// @Success 200 {object} models.APIResponse{info=kyc.Link}
// @Failure 401 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "kyc_application_not_found"
// @Failure 409 {object} models.APIResponse "row_unverified"
// @Failure 503 {object} models.APIResponse "kyc_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/kyc/documents/{id}/link [get]
func (h *AdminHandler) KYCDocumentLink(c *gin.Context) {
	if !h.kycConfigured(c) {
		return
	}
	_, adminHK, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	link, err := h.KYC.Link(ctx, c.Param("id"), adminHK)
	if err != nil {
		sendKYCError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("KYC_DOCUMENT_LINK"), link)
}

// @Summary Open a KYC document
// @Description The content of a document (#72), decrypted, for a link of GET /admin/kyc/documents/{id}/link: no token, but the signature of the link, for its administrator, who must still be active, until it expires. It answers with the kind of the file, never cached, and is audited as admin.kyc_document_view.
// @Tags admin kyc
// @Produce png,jpeg,application/pdf,json
// @Param id path string true "The document" format(uuid)
// @Param admin query string true "The administrator the link is for" format(uuid)
// @Param expires query int true "When the link expires, in Unix seconds"
// @Param sig query string true "The signature"
// @Success 200 {file} file "The document"
// @Failure 403 {object} models.APIResponse "kyc_link_invalid"
// @Failure 404 {object} models.APIResponse "kyc_application_not_found"
// @Failure 409 {object} models.APIResponse "kyc_document_changed or row_unverified"
// @Failure 503 {object} models.APIResponse "kyc_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/kyc/documents/{id}/content [get]
func (h *AdminHandler) KYCDocumentContent(c *gin.Context) {
	if !h.kycConfigured(c) {
		return
	}
	adminHK := c.Query("admin")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	if h.Accounts == nil {
		helpers.SendError(c, http.StatusForbidden, codeKYCLinkInvalid, i18n.M("KYC_LINK_INVALID"), nil)
		return
	}
	if active, err := h.Accounts.IsActive(ctx, adminHK); err != nil || !active {
		helpers.SendError(c, http.StatusForbidden, codeKYCLinkInvalid, i18n.M("KYC_LINK_INVALID"), nil)
		return
	}
	content, mime, err := h.KYC.Content(ctx, c.Param("id"), adminHK, c.Query("expires"), c.Query("sig"), audit.FromRequest(c))
	if err != nil {
		sendKYCError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'; sandbox")
	c.Header("Content-Disposition", "inline")
	c.Data(http.StatusOK, mime, content)
}

func sendKYCError(c *gin.Context, err error) {
	var invalid kyc.Invalid
	switch {
	case errors.As(err, &invalid):
		sendValidationErrors(c, fieldErrors(invalid))
	case errors.Is(err, kyc.ErrNotFound):
		helpers.SendError(c, http.StatusNotFound, codeKYCNotFound, i18n.M("KYC_APPLICATION_NOT_FOUND"), nil)
	case errors.Is(err, kyc.ErrNotUnderReview):
		helpers.SendError(c, http.StatusConflict, codeKYCNotUnderReview, i18n.M("KYC_APPLICATION_NOT_UNDER_REVIEW"), nil)
	case errors.Is(err, kyc.ErrLevelRequired):
		helpers.SendError(c, http.StatusForbidden, codeKYCLevelRequired, i18n.M("KYC_LEVEL_REQUIRED"), nil)
	case errors.Is(err, kyc.ErrLinkInvalid):
		helpers.SendError(c, http.StatusForbidden, codeKYCLinkInvalid, i18n.M("KYC_LINK_INVALID"), nil)
	case errors.Is(err, kyc.ErrContentChanged):
		helpers.SendError(c, http.StatusConflict, codeKYCChanged, i18n.M("KYC_DOCUMENT_CHANGED"), nil)
	case errors.Is(err, kyc.ErrUnavailable):
		log.Printf("KYC is unavailable for an administrator: %v", err)
		helpers.SendError(c, http.StatusServiceUnavailable, codeKYCUnavailable, i18n.M("KYC_UNAVAILABLE"), nil)
	case errors.Is(err, rowseal.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
	case errors.Is(err, verification.ErrInactive):
		helpers.SendError(c, http.StatusConflict, "account_inactive", i18n.M("KYC_ACCOUNT_INACTIVE"), nil)
	default:
		log.Printf("Failed to handle a KYC application for an administrator: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("INTERNAL_ERROR"), nil)
	}
}
