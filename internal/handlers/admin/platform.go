package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/gin-gonic/gin"
)

// The platform controls (#235): administrators put the whole service under
// maintenance, or lock one feature, now or on a schedule, and lift them.

// CreateControlRequest is a maintenance or a lockdown to create.
type CreateControlRequest struct {
	Kind platform.Kind `json:"kind" enums:"maintenance,lockdown"`
	// Feature is the feature that a lockdown closes; a maintenance closes
	// everything and takes none.
	Feature platform.Feature `json:"feature,omitempty" enums:"sign_up,sign_in,profile_changes,deposits,withdrawals,trading,api_keys,websocket,read_only,scheduler,outbox,market_data"`
	// StartsAt is now when absent; EndsAt, when set, ends the control
	// without a lift.
	StartsAt *time.Time `json:"starts_at,omitempty"`
	EndsAt   *time.Time `json:"ends_at,omitempty"`
	// AnnounceAt is when users learn of the control, before its start; now
	// when absent.
	AnnounceAt *time.Time `json:"announce_at,omitempty"`
	// Message is shown to users, at most 500 characters per language.
	Message platform.Message `json:"message"`
	// Reason is for administrators, at most 1000 characters.
	Reason string `json:"reason"`
	// AllowNetworks and AllowUsers may still use the service during a
	// maintenance, at most 50 of each: addresses or CIDR networks, and the
	// UUIDs of users.
	AllowNetworks []string `json:"allow_networks,omitempty"`
	AllowUsers    []string `json:"allow_users,omitempty"`
}

// UpdateControlRequest changes the end or the message of a control.
type UpdateControlRequest struct {
	// EndsAt extends or shortens the control.
	EndsAt  *time.Time        `json:"ends_at,omitempty"`
	Message *platform.Message `json:"message,omitempty"`
}

// LiftControlRequest lifts a control.
type LiftControlRequest struct {
	// Note says why, for the audit log, at most 1000 characters.
	Note string `json:"note"`
}

// PlatformOverview is what users see now, and every control that applies
// now or later, with its reason and allow lists.
type PlatformOverview struct {
	Status   platform.Status     `json:"status"`
	Controls []*platform.Control `json:"controls"`
}

// ControlList is the controls, newest first.
type ControlList struct {
	Items []*platform.Control `json:"items"`
}

// refreshPlatform applies a change on this instance at once; the others
// apply it within platform.poll_interval_ms.
func (h *AdminHandler) refreshPlatform(ctx context.Context) {
	if h.Platform == nil {
		return
	}
	if err := h.Platform.Refresh(ctx); err != nil {
		log.Printf("Failed to read the platform controls after a change; the next poll retries: %v", err)
	}
}

// controlFailures are the messages of an unexpected failure, by action.
var controlFailures = map[string]i18n.Message{
	"create": i18n.M("CONTROL_CREATE_FAILED"),
	"change": i18n.M("CONTROL_CHANGE_FAILED"),
	"lift":   i18n.M("CONTROL_LIFT_FAILED"),
}

// sendControlError answers the errors of the changes of controls.
func sendControlError(c *gin.Context, err error, action string) {
	var invalid platform.Invalid
	var conflict *platform.VersionConflict
	switch {
	case errors.As(err, &invalid):
		sendValidationErrors(c, fieldErrors(invalid))
	case errors.Is(err, platform.ErrNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("CONTROL_NOT_FOUND"), nil)
	case errors.As(err, &conflict):
		helpers.SendError(c, http.StatusConflict, "version_conflict", i18n.M("CONTROL_VERSION_CONFLICT"),
			gin.H{"code": "version_conflict", "version": conflict.Current})
	case errors.Is(err, platform.ErrEnded):
		helpers.SendError(c, http.StatusConflict, "control_ended", i18n.M("CONTROL_ENDED"), nil)
	case errors.Is(err, platform.ErrSameApprover):
		helpers.SendError(c, http.StatusConflict, "second_approval_required",
			i18n.M("SECOND_APPROVAL_REQUIRED"), nil)
	case errors.Is(err, rowseal.ErrUnverified):
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
	default:
		log.Printf("Failed to %s a platform control: %v", action, err)
		helpers.SendResponse(c, http.StatusInternalServerError, controlFailures[action], nil)
	}
}

// adminOrigin is the audit origin of a change by the administrator of the
// request, and the administrator.
func adminOrigin(c *gin.Context) (audit.Event, string, bool) {
	return restrictionOrigin(c)
}

// @Summary Show the platform controls
// @Description What users see now (the status of GET /api/v1/public/status), and every maintenance and lockdown that applies now or starts later, with its reason and allow lists. Read from the database, so it includes changes that the instance has not applied yet.
// @Tags admin platform
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=PlatformOverview}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/platform [get]
func (h *AdminHandler) GetPlatform(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	controls, err := platform.Open(ctx, h.PostgresDB.DB)
	if errors.Is(err, rowseal.ErrUnverified) {
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
		return
	}
	if err != nil {
		log.Printf("Failed to read the platform controls: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_READ_CONTROLS"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("CONTROLS_RETRIEVED_SUCCESSFULLY"),
		PlatformOverview{Status: platform.StatusOf(controls, time.Now()), Controls: controls})
}

// @Summary List the platform controls
// @Description Every maintenance and lockdown, newest first, at most 200, lifted and ended ones included.
// @Tags admin platform
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=ControlList}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/platform/controls [get]
func (h *AdminHandler) ListControls(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	controls, err := platform.List(ctx, h.PostgresDB.DB, 0)
	if err != nil {
		log.Printf("Failed to list the platform controls: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LIST_CONTROLS"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("CONTROLS_RETRIEVED_SUCCESSFULLY"), ControlList{Items: controls})
}

// @Summary Put the service under maintenance, or lock a feature
// @Description Creates a full maintenance (kind maintenance), which closes every route of users with 503 maintenance except the status endpoint and sign-out, and closes their WebSocket connections; or a lockdown of one feature (kind lockdown), which refuses it with 503 feature_locked. Administrators are never closed out, and the sessions of users are kept. The control applies from starts_at (now when absent) until it is lifted or reaches ends_at, on every instance within platform.poll_interval_ms. Users see it on the status endpoint and over the WebSocket from announce_at. Audited as platform.control_create.
// @Tags admin platform
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body CreateControlRequest true "The control"
// @Success 201 {object} models.APIResponse{info=platform.Control}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/platform/controls [post]
func (h *AdminHandler) CreateControl(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	var req CreateControlRequest
	if !decodeStrict(c, &req) {
		return
	}
	origin, adminHK, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	created, err := platform.Create(ctx, h.PostgresDB.DB, platform.New{
		Kind: req.Kind, Feature: req.Feature, StartsAt: req.StartsAt, EndsAt: req.EndsAt, AnnounceAt: req.AnnounceAt,
		Message: req.Message, Reason: req.Reason, AllowNetworks: req.AllowNetworks, AllowUsers: req.AllowUsers,
		CreatedBy: adminHK,
	}, origin)
	if err != nil {
		sendControlError(c, err, "create")
		return
	}
	h.refreshPlatform(ctx)
	helpers.SendResponse(c, http.StatusCreated, i18n.M("CONTROL_CREATED"), created)
}

// @Summary Change the end or the message of a platform control
// @Description Extends or shortens a control that has not ended, or changes its message; the status endpoint, the WebSocket, and Retry-After follow. With version, the change is refused with 409 version_conflict when the control has changed since. An ended or lifted control answers 409 control_ended. Audited as platform.control_update.
// @Tags admin platform
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "The control's id" format(uuid)
// @Param version query int false "The version the change is based on" minimum(1)
// @Param body body UpdateControlRequest true "The changes"
// @Success 200 {object} models.APIResponse{info=platform.Control}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/platform/controls/{id} [patch]
func (h *AdminHandler) UpdateControl(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	expected, ok := versionQuery(c)
	if !ok {
		sendValidationErrors(c, fieldErrors{"version": "must be a number of at least 1"})
		return
	}
	var req UpdateControlRequest
	if !decodeStrict(c, &req) {
		return
	}
	origin, _, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	updated, err := platform.Update(ctx, h.PostgresDB.DB, c.Param("id"),
		platform.Changes{EndsAt: req.EndsAt, Message: req.Message}, expected, origin)
	if err != nil {
		sendControlError(c, err, "change")
		return
	}
	h.refreshPlatform(ctx)
	helpers.SendResponse(c, http.StatusOK, i18n.M("CONTROL_CHANGED"), updated)
}

// @Summary Lift a platform control
// @Description Ends a control now, or cancels one that has not started. An active lockdown of a feature in platform.lift_approval (withdrawals and trading by default) needs two administrators: the first call records the request and answers 202, and a call by another administrator lifts it; the same administrator gets 409 second_approval_required. The approval needs the sealed audit event of the request; a request without it, such as one written in SQL, counts as none, and the call asks again with 202. A control changed outside the backend answers 409 row_unverified, in enforce mode. With version, the lift is refused with 409 version_conflict when the control has changed since. An ended or lifted control answers 409 control_ended. Audited as platform.control_lift_request and platform.control_lift.
// @Tags admin platform
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "The control's id" format(uuid)
// @Param version query int false "The version the lift is based on" minimum(1)
// @Param body body LiftControlRequest true "Why"
// @Success 200 {object} models.APIResponse{info=platform.Control}
// @Success 202 {object} models.APIResponse{info=platform.Control} "The lift waits for another administrator"
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/platform/controls/{id}/lift [post]
func (h *AdminHandler) LiftControl(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	expected, ok := versionQuery(c)
	if !ok {
		sendValidationErrors(c, fieldErrors{"version": "must be a number of at least 1"})
		return
	}
	var req LiftControlRequest
	if !decodeStrict(c, &req) {
		return
	}
	origin, adminHK, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	result, err := platform.Lift(ctx, h.PostgresDB.DB, platform.LiftRequest{
		HK: c.Param("id"), Note: req.Note, By: adminHK, Expected: expected, Approval: h.LiftApproval,
	}, origin)
	if err != nil {
		sendControlError(c, err, "lift")
		return
	}
	if result.Pending {
		helpers.SendResponse(c, http.StatusAccepted, i18n.M("LIFT_REQUESTED"), result.Control)
		return
	}
	h.refreshPlatform(ctx)
	helpers.SendResponse(c, http.StatusOK, i18n.M("CONTROL_LIFTED"), result.Control)
}
