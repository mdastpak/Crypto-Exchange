package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/pagination"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/gin-gonic/gin"
)

// FindingPage is a page of integrity findings. It describes
// pagination.Page[rowseal.Finding] in the OpenAPI document.
type FindingPage struct {
	Items []rowseal.Finding `json:"items"`
	// NextCursor asks for the next page. It is left out on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// ResealRequest is the body of a re-seal.
type ResealRequest struct {
	// Reason says why the current values of the row are right, such as the
	// reviewed data fix that changed it.
	Reason string `json:"reason" binding:"required" maxLength:"1000"`
}

// findingPosition is the cursor of the findings, newest first.
type findingPosition struct {
	ID int64 `json:"id"`
}

// ListIntegrityFindings lists the rows that did not match their seal.
//
// @Summary List the integrity findings
// @Description The protected rows (accounts, contacts, sessions) that did not match their seal (#139), found by a request or by the background verifier, newest first: open ones only, unless all is true. Pages follow the pagination convention of docs/api.md.
// @Tags admin integrity
// @Produce json
// @Security BearerAuth
// @Param all query bool false "Include the resolved findings"
// @Param limit query int false "The number of findings, 50 by default" minimum(1) maximum(200)
// @Param cursor query string false "The next_cursor of the previous page"
// @Success 200 {object} models.APIResponse{info=FindingPage}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/integrity/findings [get]
func (h *AdminHandler) ListIntegrityFindings(c *gin.Context) {
	page, errs := pagination.Parse(c)
	if errs == nil {
		errs = map[string]string{}
	}
	all := false
	if raw := c.Query("all"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			errs["all"] = "must be true or false"
		}
		all = v
	}
	var after findingPosition
	if _, err := page.After(&after); err != nil || after.ID < 0 {
		errs["cursor"] = "is not a cursor of this list"
	}
	if len(errs) > 0 {
		sendValidationErrors(c, errs)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	findings, err := h.Findings.List(ctx, rowseal.ListOptions{All: all, BeforeID: after.ID, Limit: page.Limit + 1})
	if err != nil {
		log.Printf("Failed to list the integrity findings: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LIST_INTEGRITY_FINDINGS"), nil)
		return
	}
	result, err := pagination.NewPage(findings, page, func(f rowseal.Finding) interface{} { return findingPosition{ID: f.ID} })
	if err != nil {
		log.Printf("Failed to page the integrity findings: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LIST_INTEGRITY_FINDINGS"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("INTEGRITY_FINDINGS_RETRIEVED_SUCCESSFULLY"), result)
}

// ResealIntegrityFinding seals the row of a finding again after a review.
//
// @Summary Re-seal the row of an integrity finding
// @Description After a review found the current values of a row that did not match its seal right, as after a reviewed data fix, seals the row again with the current row-mac key and closes the finding. It needs two administrators: the first request records the reason and answers 202 with the finding and who asked; the same request by another administrator within 24 hours re-seals the row and answers 200. The approval needs the sealed integrity.reseal_request audit event of the request; a request without it, such as one written in SQL, counts as none, and the call asks again with 202. The administrator who asked cannot approve (403 second_approval_required), and neither step is open to the administrator whose account the row belongs to (403 own_row). The reason is required. Audited as integrity.reseal_request and integrity.reseal, with the values sealed now, secret ones masked. A finding that is already resolved answers 409 already_resolved; without data keys, 503. The route needs a step-up.
// @Tags admin integrity
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "The finding's id" minimum(1)
// @Param body body ResealRequest true "Why the row is right"
// @Success 200 {object} models.APIResponse{info=rowseal.Finding}
// @Success 202 {object} models.APIResponse{info=rowseal.Finding}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/integrity/findings/{id}/reseal [post]
func (h *AdminHandler) ResealIntegrityFinding(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		sendValidationErrors(c, fieldErrors{"id": "must be a positive number"})
		return
	}
	var req ResealRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" || utf8.RuneCountInString(req.Reason) > rowseal.MaxReasonLength || strings.ContainsRune(req.Reason, 0) {
		sendValidationErrors(c, fieldErrors{"reason": "is required, at most 1000 characters"})
		return
	}
	origin, _, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	sealer := rowseal.Active()
	if sealer == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("ROW_SEALS_NOT_CONFIGURED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	finding, pending, err := sealer.ReviewReseal(ctx, id, origin, req.Reason)
	switch {
	case errors.Is(err, rowseal.ErrSameApprover):
		helpers.SendError(c, http.StatusForbidden, "second_approval_required", i18n.M("RESEAL_SECOND_APPROVAL_REQUIRED"), nil)
	case errors.Is(err, rowseal.ErrOwnRow):
		helpers.SendError(c, http.StatusForbidden, "own_row", i18n.M("RESEAL_OWN_ROW"), nil)
	case errors.Is(err, rowseal.ErrNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("INTEGRITY_FINDING_NOT_FOUND"), nil)
	case errors.Is(err, rowseal.ErrResolved):
		helpers.SendError(c, http.StatusConflict, "already_resolved", i18n.M("INTEGRITY_FINDING_ALREADY_RESOLVED"), nil)
	case err != nil:
		log.Printf("Failed to re-seal the row of finding %d: %v", id, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_RESEAL_ROW"), nil)
	case pending:
		helpers.SendResponse(c, http.StatusAccepted, i18n.M("RESEAL_REQUESTED"), finding)
	default:
		helpers.SendResponse(c, http.StatusOK, i18n.M("ROW_RESEALED_SUCCESSFULLY"), finding)
	}
}
