package user

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/kyc"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// The KYC applications of a user (#72): a user at level 1 applies for
// level 2 with a form and documents, which an administrator reviews.

const (
	codeKYCUnavailable         = "kyc_unavailable"
	codeKYCNotFound            = "kyc_application_not_found"
	codeKYCLevelRequired       = "identity_verification_required"
	codeKYCAlreadyVerified     = "kyc_already_verified"
	codeKYCOpenApplication     = "kyc_application_open"
	codeKYCNotPending          = "kyc_application_submitted"
	codeKYCTooManyDocuments    = "kyc_documents_exceeded"
	codeKYCDocumentsIncomplete = "kyc_documents_missing"
)

// kycApplicationRequest opens an application.
type kycApplicationRequest struct {
	Form kyc.Form `json:"form"`
}

func (h *UserHandler) kycCaller(c *gin.Context) (string, bool) {
	claims, ok := sessionCaller(c)
	if !ok {
		return "", false
	}
	if h.KYC == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codeKYCUnavailable, i18n.M("KYC_UNAVAILABLE"), nil)
		return "", false
	}
	return claims.UUID, true
}

// @Summary Get the user's KYC application
// @Description The user's newest application for level 2 (#72): its status, the reason of a rejection, and its documents, without their content. 404 kyc_application_not_found when the user has none.
// @Tags user-kyc
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=kyc.Application}
// @Failure 401 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "kyc_application_not_found"
// @Failure 503 {object} models.APIResponse "kyc_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/kyc [get]
func (h *UserHandler) GetKYC(c *gin.Context) {
	userHK, ok := h.kycCaller(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	a, err := h.KYC.Current(ctx, userHK)
	if err != nil {
		sendKYCError(c, userHK, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("KYC_APPLICATION_RETRIEVED"), a)
}

// @Summary Apply for level 2
// @Description Opens a KYC application for level 2 (#72) with the user's address, occupation, and source of funds, which are stored encrypted. The user must be at level 1, with a verified identity (403 identity_verification_required), and have no open application (409 kyc_application_open). Then add the documents, and submit it.
// @Tags user-kyc
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body kycApplicationRequest true "The form"
// @Success 201 {object} models.APIResponse{info=kyc.Application}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "identity_verification_required"
// @Failure 409 {object} models.APIResponse "kyc_application_open, kyc_already_verified, or row_unverified"
// @Failure 503 {object} models.APIResponse "kyc_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/kyc/applications [post]
func (h *UserHandler) ApplyKYC(c *gin.Context) {
	userHK, ok := h.kycCaller(c)
	if !ok {
		return
	}
	var req kycApplicationRequest
	if !decodeStrict(c, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	a, err := h.KYC.Apply(ctx, userHK, req.Form, audit.FromRequest(c))
	if err != nil {
		sendKYCError(c, userHK, err)
		return
	}
	helpers.SendResponse(c, http.StatusCreated, i18n.M("KYC_APPLICATION_OPENED"), a)
}

// @Summary Add a document to a KYC application
// @Description Adds a document to the user's pending application (#72), as multipart/form-data: type, and file, a JPEG, a PNG, or a PDF of at most 10 MB, by its content. At most 10 documents. The file is encrypted before it is stored, and its name is not kept.
// @Tags user-kyc
// @Accept mpfd
// @Produce json
// @Security BearerAuth
// @Param id path string true "The application" format(uuid)
// @Param type formData string true "The type of the document" Enums(NATIONAL_ID,PASSPORT,DRIVERS_LICENSE,SELFIE,PROOF_OF_ADDRESS,UTILITY_BILL,BANK_STATEMENT,SOURCE_OF_FUNDS,CORPORATE_DOCUMENT)
// @Param file formData file true "The document"
// @Success 201 {object} models.APIResponse{info=kyc.Document}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "kyc_application_not_found"
// @Failure 409 {object} models.APIResponse "kyc_application_submitted, kyc_documents_exceeded, or row_unverified"
// @Failure 413 {object} models.APIResponse "validation_failed: the request is larger than a document"
// @Failure 503 {object} models.APIResponse "kyc_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/kyc/applications/{id}/documents [post]
func (h *UserHandler) UploadKYCDocument(c *gin.Context) {
	userHK, ok := h.kycCaller(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, kyc.MaxDocumentSize+1<<20)
	file, _, err := c.Request.FormFile("file")
	if err != nil {
		var tooLarge *http.MaxBytesError
		status := http.StatusBadRequest
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		helpers.SendError(c, status, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"),
			gin.H{"errors": map[string]string{"file": "is required, at most 10 MB"}})
		return
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, kyc.MaxDocumentSize+1))
	if err != nil {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"),
			gin.H{"errors": map[string]string{"file": "could not be read"}})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	d, err := h.KYC.Upload(ctx, userHK, c.Param("id"), kyc.DocumentType(c.Request.FormValue("type")), content, audit.FromRequest(c))
	if err != nil {
		sendKYCError(c, userHK, err)
		return
	}
	helpers.SendResponse(c, http.StatusCreated, i18n.M("KYC_DOCUMENT_ADDED"), d)
}

// @Summary Submit a KYC application
// @Description Sends the user's pending application for review (#72), once it has an identity document (NATIONAL_ID, PASSPORT, or DRIVERS_LICENSE), a SELFIE, and a proof of address (PROOF_OF_ADDRESS, UTILITY_BILL, or BANK_STATEMENT); 409 kyc_documents_missing names the groups that it lacks.
// @Tags user-kyc
// @Produce json
// @Security BearerAuth
// @Param id path string true "The application" format(uuid)
// @Success 200 {object} models.APIResponse{info=kyc.Application}
// @Failure 401 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "kyc_application_not_found"
// @Failure 409 {object} models.APIResponse "kyc_documents_missing (with info.missing), kyc_application_submitted, or row_unverified"
// @Failure 503 {object} models.APIResponse "kyc_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/kyc/applications/{id}/submit [post]
func (h *UserHandler) SubmitKYC(c *gin.Context) {
	userHK, ok := h.kycCaller(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	a, err := h.KYC.Submit(ctx, userHK, c.Param("id"), audit.FromRequest(c))
	if err != nil {
		sendKYCError(c, userHK, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("KYC_APPLICATION_SUBMITTED"), a)
}

func sendKYCError(c *gin.Context, userHK string, err error) {
	var invalid kyc.Invalid
	var missing *kyc.MissingDocuments
	switch {
	case errors.As(err, &invalid):
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": invalid})
	case errors.As(err, &missing):
		helpers.SendError(c, http.StatusConflict, codeKYCDocumentsIncomplete, i18n.M("KYC_DOCUMENTS_MISSING"), gin.H{"missing": missing.Missing})
	case errors.Is(err, kyc.ErrNotFound):
		helpers.SendError(c, http.StatusNotFound, codeKYCNotFound, i18n.M("KYC_APPLICATION_NOT_FOUND"), nil)
	case errors.Is(err, kyc.ErrLevelRequired):
		helpers.SendError(c, http.StatusForbidden, codeKYCLevelRequired, i18n.M("KYC_LEVEL_REQUIRED"), nil)
	case errors.Is(err, kyc.ErrAlreadyVerified):
		helpers.SendError(c, http.StatusConflict, codeKYCAlreadyVerified, i18n.M("KYC_ALREADY_VERIFIED"), nil)
	case errors.Is(err, kyc.ErrOpenApplication):
		helpers.SendError(c, http.StatusConflict, codeKYCOpenApplication, i18n.M("KYC_APPLICATION_OPEN"), nil)
	case errors.Is(err, kyc.ErrNotPending):
		helpers.SendError(c, http.StatusConflict, codeKYCNotPending, i18n.M("KYC_APPLICATION_SUBMITTED_ALREADY"), nil)
	case errors.Is(err, kyc.ErrTooManyDocuments):
		helpers.SendError(c, http.StatusConflict, codeKYCTooManyDocuments, i18n.M("KYC_DOCUMENTS_EXCEEDED"), nil)
	case errors.Is(err, kyc.ErrUnavailable):
		log.Printf("KYC is unavailable for user %s: %v", userHK, err)
		helpers.SendError(c, http.StatusServiceUnavailable, codeKYCUnavailable, i18n.M("KYC_UNAVAILABLE"), nil)
	case errors.Is(err, rowseal.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, "row_unverified", i18n.M("ROW_UNVERIFIED"), nil)
	default:
		log.Printf("Failed to handle a KYC application of user %s: %v", userHK, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("INTERNAL_ERROR"), nil)
	}
}
