package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mdastpak/crypto-exchange/internal/aml"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// The AML rules and their alerts (#73). The rules come from
// config/aml_rules.yaml, which the server syncs at startup; the
// administrators read them here, and work the alerts: assign one to
// themselves to investigate it, escalate it, and resolve or dismiss it with
// a resolution.

const (
	codeAMLUnavailable            = "aml_unavailable"
	codeAMLAlertNotFound          = "aml_alert_not_found"
	codeAMLAlertTransitionInvalid = "aml_alert_transition_invalid"
)

// amlTransition moves an alert.
type amlTransition struct {
	// Status is the new status: INVESTIGATING assigns the alert to the
	// administrator.
	Status aml.Status `json:"status" binding:"required" enums:"INVESTIGATING,RESOLVED,DISMISSED,ESCALATED"`
	// Resolution, 1 to 1000 characters, is required to resolve or dismiss
	// the alert, and refused otherwise.
	Resolution string `json:"resolution,omitempty" example:"The deposits are the user's salary; documents on file."`
}

func (h *AdminHandler) amlConfigured(c *gin.Context) bool {
	if h.AML == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codeAMLUnavailable, i18n.M("AML_UNAVAILABLE"), nil)
		return false
	}
	return true
}

// @Summary List the AML rules
// @Description Every AML rule as the server synced it from config/aml_rules.yaml, with its version and whether it is retired (#73), by code. A rule changed in SQL answers 409 row_unverified, and no withdrawal is evaluated until it is reviewed.
// @Tags admin aml
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=[]aml.Stored}
// @Failure 401 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "row_unverified"
// @Failure 503 {object} models.APIResponse "aml_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/aml/rules [get]
func (h *AdminHandler) ListAMLRules(c *gin.Context) {
	if !h.amlConfigured(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	rules, err := h.AML.Rules(ctx)
	if err != nil {
		sendAMLError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("AML_RULES_RETRIEVED"), rules)
}

// @Summary List AML alerts
// @Description The AML alerts, newest first: of a status, of a user, or all; at most limit (100). Each names its rule and the version of the rule, the operation that raised it, if any, and the action that was taken. An alert changed in SQL answers 409 row_unverified.
// @Tags admin aml
// @Produce json
// @Security BearerAuth
// @Param status query string false "A status" Enums(OPEN,INVESTIGATING,RESOLVED,DISMISSED,ESCALATED)
// @Param user_hk query string false "A user" format(uuid)
// @Param limit query int false "1 to 100; 100 by default"
// @Success 200 {object} models.APIResponse{info=[]aml.Alert}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "row_unverified"
// @Failure 503 {object} models.APIResponse "aml_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/aml/alerts [get]
func (h *AdminHandler) ListAMLAlerts(c *gin.Context) {
	if !h.amlConfigured(c) {
		return
	}
	problems := fieldErrors{}
	status := aml.Status(c.Query("status"))
	if status != "" && !status.Valid() {
		problems["status"] = "is not a status of an alert"
	}
	user := c.Query("user_hk")
	if _, err := uuid.Parse(user); user != "" && err != nil {
		problems["user_hk"] = "must be a UUID"
	}
	limit := aml.MaxList
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > aml.MaxList {
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
	list, err := h.AML.Alerts(ctx, status, user, limit)
	if err != nil {
		sendAMLError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("AML_ALERTS_RETRIEVED"), list)
}

// @Summary Get an AML alert
// @Description An AML alert, with what its rule saw (trigger_data).
// @Tags admin aml
// @Produce json
// @Security BearerAuth
// @Param id path string true "The alert" format(uuid)
// @Success 200 {object} models.APIResponse{info=aml.Alert}
// @Failure 401 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "aml_alert_not_found"
// @Failure 409 {object} models.APIResponse "row_unverified"
// @Failure 503 {object} models.APIResponse "aml_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/aml/alerts/{id} [get]
func (h *AdminHandler) GetAMLAlert(c *gin.Context) {
	if !h.amlConfigured(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	a, err := h.AML.Alert(ctx, c.Param("id"))
	if err != nil {
		sendAMLError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("AML_ALERT_RETRIEVED"), a)
}

// @Summary Move an AML alert
// @Description Moves an alert to another status (#73). OPEN may become INVESTIGATING, which assigns it to the administrator, ESCALATED, RESOLVED, or DISMISSED; INVESTIGATING may become ESCALATED, RESOLVED, or DISMISSED; ESCALATED may become INVESTIGATING, RESOLVED, or DISMISSED. RESOLVED and DISMISSED are final and need a resolution. An alert open for engine.alert_escalation_hours is escalated by the server. Moving an alert releases nothing: a withdrawal held for review is approved or rejected on its own route, and a freeze is lifted on the user's restrictions. Audited as aml.alert_status.
// @Tags admin aml
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "The alert" format(uuid)
// @Param body body amlTransition true "The new status"
// @Success 200 {object} models.APIResponse{info=aml.Alert}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "aml_alert_not_found"
// @Failure 409 {object} models.APIResponse "aml_alert_transition_invalid or row_unverified"
// @Failure 503 {object} models.APIResponse "aml_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/aml/alerts/{id}/transition [post]
func (h *AdminHandler) TransitionAMLAlert(c *gin.Context) {
	if !h.amlConfigured(c) {
		return
	}
	var req amlTransition
	if !decodeStrict(c, &req) {
		return
	}
	if !req.Status.Valid() || req.Status == aml.StatusOpen {
		sendValidationErrors(c, fieldErrors{"status": "must be INVESTIGATING, RESOLVED, DISMISSED, or ESCALATED"})
		return
	}
	origin, adminHK, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	a, err := h.AML.Transition(ctx, c.Param("id"), req.Status, adminHK, req.Resolution, origin)
	if err != nil {
		sendAMLError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("AML_ALERT_UPDATED"), a)
}

// @Summary Get the state of the sanctions list
// @Description The version of the OFAC list that the sanctions rules screen against (#73): when it was downloaded, when a download last found it current, its entries, and whether it is stale, too old or missing, which holds every operation that a sanctions rule screens for review. 503 aml_unavailable while the provider or the AML rules are off.
// @Tags admin aml
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=sanctions.Status}
// @Failure 401 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "row_unverified"
// @Failure 503 {object} models.APIResponse "aml_unavailable"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/aml/sanctions [get]
func (h *AdminHandler) GetSanctionsStatus(c *gin.Context) {
	if h.Sanctions == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codeAMLUnavailable, i18n.M("AML_UNAVAILABLE"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	st, err := h.Sanctions.Status(ctx)
	if err != nil {
		sendAMLError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("SANCTIONS_LIST_RETRIEVED"), st)
}

func sendAMLError(c *gin.Context, err error) {
	var transition *aml.ErrTransition
	switch {
	case errors.Is(err, aml.ErrResolution):
		sendValidationErrors(c, fieldErrors{"resolution": "is required, 1 to 1000 characters, to resolve or dismiss an alert, and only then"})
	case errors.Is(err, aml.ErrNotFound):
		helpers.SendError(c, http.StatusNotFound, codeAMLAlertNotFound, i18n.M("AML_ALERT_NOT_FOUND"), nil)
	case errors.As(err, &transition):
		helpers.SendError(c, http.StatusConflict, codeAMLAlertTransitionInvalid, i18n.M("AML_ALERT_TRANSITION_INVALID"),
			gin.H{"from": transition.From, "to": transition.To})
	case errors.Is(err, rowseal.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
	default:
		log.Printf("Failed to handle the AML alerts for an administrator: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("INTERNAL_ERROR"), nil)
	}
}
