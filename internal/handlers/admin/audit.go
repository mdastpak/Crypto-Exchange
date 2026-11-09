package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/pagination"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var (
	auditActionPattern = regexp.MustCompile(`^[a-z_]+(\.[a-z_]+)+$`)
	auditTargetPattern = regexp.MustCompile(`^[a-z_]+$`)
	auditOutcomes      = map[string]bool{audit.Success: true, audit.Failure: true, audit.Denied: true}
)

// ListAuditEvents returns the audit log, newest first, a page at a time.
//
// @Summary Query the audit log
// @Description The events that the filters select, newest first. Every filter is optional, and the filters combine. Pages follow the pagination convention of docs/api.md.
// @Tags admin audit
// @Produce json
// @Security BearerAuth
// @Param actor_id query string false "The hk of the user or administrator" format(uuid)
// @Param action query string false "An action, such as currency.update"
// @Param target_type query string false "A target type, such as currency"
// @Param target_id query string false "A target id, such as the hk of a currency"
// @Param outcome query string false "The outcome" Enums(success, failure, denied)
// @Param since query string false "Events at or after this time, in RFC 3339" format(date-time)
// @Param until query string false "Events before this time, in RFC 3339" format(date-time)
// @Param limit query int false "The number of events, 50 by default" minimum(1) maximum(200)
// @Param cursor query string false "The next_cursor of the previous page"
// @Success 200 {object} models.APIResponse{info=audit.EntryPage}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/audit [get]
func (h *AdminHandler) ListAuditEvents(c *gin.Context) {
	page, errs := pagination.Parse(c)
	filter, filterErrs := auditFilter(c)
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
	result, err := audit.List(ctx, h.PostgresDB.DB, filter, page)
	if errors.Is(err, pagination.ErrInvalidCursor) {
		sendValidationErrors(c, fieldErrors{"cursor": "is not a cursor of this list"})
		return
	}
	if err != nil {
		log.Printf("Error querying the audit log: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_QUERY_AUDIT_LOG"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("AUDIT_EVENTS_RETRIEVED_SUCCESSFULLY"), result)
}

// auditFilter reads the filters of the audit query, and the problems of
// those that are invalid.
func auditFilter(c *gin.Context) (audit.Filter, fieldErrors) {
	var f audit.Filter
	errs := fieldErrors{}
	if raw := c.Query("actor_id"); raw != "" {
		if id, err := uuid.Parse(raw); err != nil {
			errs["actor_id"] = "must be a UUID"
		} else {
			f.ActorID = id.String()
		}
	}
	if raw := c.Query("action"); raw != "" {
		if !auditActionPattern.MatchString(raw) {
			errs["action"] = "must be an action, such as currency.update"
		}
		f.Action = raw
	}
	if raw := c.Query("target_type"); raw != "" {
		if !auditTargetPattern.MatchString(raw) {
			errs["target_type"] = "must be a target type, such as currency"
		}
		f.TargetType = raw
	}
	if raw := c.Query("target_id"); raw != "" {
		if utf8.RuneCountInString(raw) > 64 {
			errs["target_id"] = "must be at most 64 characters"
		}
		f.TargetID = raw
	}
	if raw := c.Query("outcome"); raw != "" {
		if !auditOutcomes[raw] {
			errs["outcome"] = "must be success, failure, or denied"
		}
		f.Outcome = raw
	}
	for name, at := range map[string]*time.Time{"since": &f.Since, "until": &f.Until} {
		if raw := c.Query(name); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				errs[name] = "must be a time in RFC 3339, such as 2026-11-07T10:00:00Z"
			}
			*at = t
		}
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && !f.Since.Before(f.Until) {
		errs["until"] = "must be after since"
	}
	if len(errs) == 0 {
		return f, nil
	}
	return f, errs
}
