package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// The restrictions of users (#220): administrators freeze an account, block
// operations, or hold one currency of a user, for a court order, an AML
// review, or another case, and lift them.

const (
	maxCaseRef = 64
	maxNote    = 1000
	// maxRestrictionList bounds a list of the restrictions of one user.
	maxRestrictionList = 200
)

// codeRowUnverified refuses a change of a row that fails its seal check
// (#337): an administrator reviews its integrity finding first.
const codeRowUnverified = "row_unverified"

// CreateRestrictionRequest is a restriction to create.
type CreateRestrictionRequest struct {
	Scope restrictions.Scope `json:"scope" enums:"account,operation,asset"`
	// Operations are the blocked operations of an operation or asset
	// restriction; an account freeze blocks everything and takes none.
	Operations []restrictions.Operation `json:"operations" enums:"deposit,withdraw,convert,buy,sell,trade"`
	// CurrencyHK is the held currency of an asset restriction.
	CurrencyHK string              `json:"currency_hk,omitempty" format:"uuid"`
	Reason     restrictions.Reason `json:"reason" enums:"court_order,aml_review,sanctions,fraud,security,user_request,other"`
	// CaseRef is the reference of the case, at most 64 characters.
	CaseRef string `json:"case_ref,omitempty" example:"AML-2026-0042"`
	// Note is for administrators, at most 1000 characters.
	Note string `json:"note,omitempty"`
	// EndsAt, when set, ends the restriction without a lift.
	EndsAt *time.Time `json:"ends_at,omitempty"`
}

// LiftRestrictionRequest lifts a restriction.
type LiftRestrictionRequest struct {
	// Note says why, for the audit log, at most 1000 characters.
	Note string `json:"note"`
}

// CreatedRestriction answers a create.
type CreatedRestriction struct {
	Restriction restrictions.Restriction `json:"restriction"`
	// EndedSessions is the number of sessions that a freeze ended.
	EndedSessions int `json:"ended_sessions" example:"2"`
}

// RestrictionList is the restrictions of a user, newest first.
type RestrictionList struct {
	Items []restrictions.Restriction `json:"items"`
}

func (r CreateRestrictionRequest) validate(now time.Time) fieldErrors {
	errs := fieldErrors{}
	switch r.Scope {
	case restrictions.ScopeAccount:
		if len(r.Operations) > 0 {
			errs["operations"] = "must be empty for an account freeze, which blocks everything"
		}
	case restrictions.ScopeOperation, restrictions.ScopeAsset:
		if len(r.Operations) == 0 {
			errs["operations"] = "must name at least one operation"
		}
		seen := map[restrictions.Operation]bool{}
		for _, op := range r.Operations {
			if !slices.Contains(restrictions.Operations, op) || seen[op] {
				errs["operations"] = "must be distinct operations among deposit, withdraw, convert, buy, sell, and trade"
				break
			}
			seen[op] = true
		}
	default:
		errs["scope"] = "must be account, operation, or asset"
	}
	if r.Scope == restrictions.ScopeAsset {
		if _, err := uuid.Parse(r.CurrencyHK); err != nil {
			errs["currency_hk"] = "must be the UUID of a currency"
		}
	} else if r.CurrencyHK != "" {
		errs["currency_hk"] = "is only for an asset restriction"
	}
	if !slices.Contains(restrictions.Reasons, r.Reason) {
		errs["reason"] = "must be one of court_order, aml_review, sanctions, fraud, security, user_request, and other"
	}
	if utf8.RuneCountInString(r.CaseRef) > maxCaseRef || strings.ContainsRune(r.CaseRef, 0) {
		errs["case_ref"] = "must be at most 64 characters"
	}
	if utf8.RuneCountInString(r.Note) > maxNote || strings.ContainsRune(r.Note, 0) {
		errs["note"] = "must be at most 1000 characters"
	}
	if r.EndsAt != nil && !r.EndsAt.After(now) {
		errs["ends_at"] = "must be in the future"
	}
	return errs
}

// restrictionOrigin is the audit origin of a change by the administrator of
// the request.
func restrictionOrigin(c *gin.Context) (audit.Event, string, bool) {
	e := audit.FromRequest(c)
	claims, ok := auth.GetClaims(c)
	if !ok || claims.UUID == "" {
		return e, "", false
	}
	e.ActorType, e.ActorID = audit.ActorAdmin, claims.UUID
	return e, claims.UUID, true
}

// userParam reads the user hk of the path.
func userParam(c *gin.Context) (string, bool) {
	hk := c.Param("hk")
	if _, err := uuid.Parse(hk); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_USER_ID"), nil)
		return "", false
	}
	return hk, true
}

// @Summary Restrict a user
// @Description Freezes the account (scope account), blocks operations on every currency (scope operation), or holds one currency (scope asset). A freeze ends every session of the user in the same transaction, so that their tokens stop working on every instance at once, and refuses sign-in with 403 account_frozen. The restriction applies until it is lifted or reaches ends_at. The reason, case, and note are never shown to the user. Audited as user.restriction_add, and each ended session as user.session_revoke with the reason account_frozen.
// @Tags admin users
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The user's UUID" format(uuid)
// @Param body body CreateRestrictionRequest true "The restriction"
// @Success 201 {object} models.APIResponse{info=CreatedRestriction}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "not_a_user: an administrator or a deleted user"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/users/{hk}/restrictions [post]
func (h *AdminHandler) CreateRestriction(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	userHK, ok := userParam(c)
	if !ok {
		return
	}
	var req CreateRestrictionRequest
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	req.CaseRef, req.Note = strings.TrimSpace(req.CaseRef), strings.TrimSpace(req.Note)
	if errs := req.validate(time.Now()); len(errs) > 0 {
		sendValidationErrors(c, errs)
		return
	}
	origin, adminHK, ok := restrictionOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}

	created, err := restrictions.Create(ctx, h.PostgresDB.DB, restrictions.New{
		UserHK: userHK, Scope: req.Scope, Operations: req.Operations, CurrencyHK: req.CurrencyHK,
		Reason: req.Reason, CaseRef: req.CaseRef, Note: req.Note, EndsAt: req.EndsAt, CreatedBy: adminHK,
	}, origin)
	switch {
	case errors.Is(err, restrictions.ErrUserNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("USER_NOT_FOUND"), nil)
	case errors.Is(err, restrictions.ErrNotUser):
		helpers.SendError(c, http.StatusConflict, "not_a_user", i18n.M("ONLY_USERS_CAN_BE_RESTRICTED"), nil)
	case errors.Is(err, restrictions.ErrCurrencyNotFound):
		sendValidationErrors(c, fieldErrors{"currency_hk": "is not a currency"})
	case err != nil:
		log.Printf("Failed to restrict a user: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_RESTRICT_USER"), nil)
	default:
		helpers.SendResponse(c, http.StatusCreated, i18n.M("USER_RESTRICTED"), CreatedRestriction{
			Restriction: created.Restriction, EndedSessions: created.EndedSessions,
		})
	}
}

// @Summary List the restrictions of a user
// @Description The restrictions of a user, newest first, at most 200: every one, lifted and ended ones included, or with active=true only those that apply now. With the reasons, cases, and notes.
// @Tags admin users
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The user's UUID" format(uuid)
// @Param active query bool false "Only the restrictions that apply now"
// @Success 200 {object} models.APIResponse{info=RestrictionList}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/users/{hk}/restrictions [get]
func (h *AdminHandler) ListRestrictions(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	userHK, ok := userParam(c)
	if !ok {
		return
	}
	activeOnly := false
	if raw, present := c.GetQuery("active"); present {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			sendValidationErrors(c, fieldErrors{"active": "must be true or false"})
			return
		}
		activeOnly = v
	}
	list, err := restrictions.List(ctx, h.PostgresDB.DB, userHK, activeOnly, maxRestrictionList)
	if err != nil {
		log.Printf("Failed to list the restrictions of a user: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LIST_USER_RESTRICTIONS"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("RESTRICTIONS_RETRIEVED_SUCCESSFULLY"), RestrictionList{Items: list})
}

// @Summary Lift a restriction of a user
// @Description Ends a restriction now. A lifted freeze lets the user sign in again; the sessions it ended stay ended. With version, the lift is refused with 409 version_conflict when the restriction has changed since. A lifted restriction answers 409 already_lifted, and one changed outside the backend 409 row_unverified, in enforce mode. Audited as user.restriction_lift.
// @Tags admin users
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param hk path string true "The user's UUID" format(uuid)
// @Param id path string true "The restriction's id" format(uuid)
// @Param version query int false "The version the lift is based on" minimum(1)
// @Param body body LiftRestrictionRequest true "Why"
// @Success 200 {object} models.APIResponse{info=restrictions.Restriction}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/users/{hk}/restrictions/{id}/lift [post]
func (h *AdminHandler) LiftRestriction(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	userHK, ok := userParam(c)
	if !ok {
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_RESTRICTION_ID"), nil)
		return
	}
	expected, ok := versionQuery(c)
	if !ok {
		sendValidationErrors(c, fieldErrors{"version": "must be a number of at least 1"})
		return
	}
	var req LiftRestrictionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	req.Note = strings.TrimSpace(req.Note)
	if req.Note == "" || utf8.RuneCountInString(req.Note) > maxNote || strings.ContainsRune(req.Note, 0) {
		sendValidationErrors(c, fieldErrors{"note": "is required, at most 1000 characters"})
		return
	}
	origin, adminHK, ok := restrictionOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}

	r, err := restrictions.Lift(ctx, h.PostgresDB.DB, userHK, id, adminHK, req.Note, expected, origin)
	var conflict *restrictions.VersionConflict
	switch {
	case errors.Is(err, restrictions.ErrUserNotFound), errors.Is(err, restrictions.ErrNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("RESTRICTION_NOT_FOUND"), nil)
	case errors.As(err, &conflict):
		helpers.SendError(c, http.StatusConflict, "version_conflict", i18n.M("RESTRICTION_VERSION_CONFLICT"),
			gin.H{"code": "version_conflict", "version": conflict.Current})
	case errors.Is(err, restrictions.ErrAlreadyLifted):
		helpers.SendError(c, http.StatusConflict, "already_lifted", i18n.M("RESTRICTION_ALREADY_LIFTED"), nil)
	case errors.Is(err, rowseal.ErrUnverified):
		// A restriction changed in SQL is lifted only after its finding is
		// resolved (#337).
		helpers.SendError(c, http.StatusConflict, codeRowUnverified, i18n.M("ROW_UNVERIFIED"), nil)
	case err != nil:
		log.Printf("Failed to lift a restriction: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LIFT_RESTRICTION"), nil)
	default:
		helpers.SendResponse(c, http.StatusOK, i18n.M("RESTRICTION_LIFTED"), r)
	}
}
