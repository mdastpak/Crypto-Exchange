package user

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/pagination"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// sessionTimeout bounds a request of the session routes.
const sessionTimeout = 5 * time.Second

// RevokedSessions is the answer to revoking the other sessions.
type RevokedSessions struct {
	Revoked int `json:"revoked" example:"2"`
}

// sessionCaller returns the user and the session of the access token.
func sessionCaller(c *gin.Context) (*auth.Claims, bool) {
	claims, ok := auth.GetClaims(c)
	if !ok || claims.SessionID == "" {
		helpers.SendError(c, http.StatusUnauthorized, helpers.CodeTokenInvalid, i18n.M("UNAUTHORIZED_INVALID_TOKEN"), nil)
		return nil, false
	}
	return claims, true
}

// ListSessions returns the active sessions of the signed-in user, newest
// first, a page at a time. The session of the request is marked current.
//
// @Summary List the signed-in user's sessions
// @Description The active sessions, newest first, with the session of the request marked current. Pages follow the pagination convention of docs/api.md.
// @Tags user sessions
// @Produce json
// @Security BearerAuth
// @Param limit query int false "The number of sessions, 50 by default" minimum(1) maximum(200)
// @Param cursor query string false "The next_cursor of the previous page"
// @Success 200 {object} models.APIResponse{info=auth.SessionPageDoc}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/sessions [get]
func (h *UserHandler) ListSessions(c *gin.Context) {
	claims, ok := sessionCaller(c)
	if !ok {
		return
	}
	page, errs := pagination.Parse(c)
	if errs != nil {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": errs})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	result, err := h.Sessions.List(ctx, claims.UUID, page)
	if errors.Is(err, pagination.ErrInvalidCursor) {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"),
			gin.H{"errors": fieldErrors{"cursor": "is not a cursor of this list"}})
		return
	}
	if err != nil {
		log.Printf("Failed to list the sessions of user %s: %v", claims.UUID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LIST_SESSIONS"), nil)
		return
	}
	for i := range result.Items {
		result.Items[i].Current = result.Items[i].ID == claims.SessionID
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("SESSIONS_RETRIEVED_SUCCESSFULLY"), result)
}

// RevokeSession ends one session of the signed-in user. Its access and
// refresh tokens stop working at once. Ending the current session signs the
// request's client out.
//
// @Summary End one of the signed-in user's sessions
// @Tags user sessions
// @Produce json
// @Security BearerAuth
// @Param id path string true "The session, from the session list" format(uuid)
// @Success 200 {object} models.APIResponse
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/sessions/{id} [delete]
func (h *UserHandler) RevokeSession(c *gin.Context) {
	claims, ok := sessionCaller(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"),
			gin.H{"errors": fieldErrors{"id": "must be a UUID"}})
		return
	}
	sessionID := id.String()
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	event := auth.SessionEvent(c, audit.ActionUserSessionRevoke, claims.UUID, sessionID)
	err = h.Sessions.RevokeOwn(ctx, sessionID, claims.UUID, event)
	switch {
	case errors.Is(err, auth.ErrSessionInvalid):
		// Also for a session of another user, which is not told apart.
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("SESSION_NOT_FOUND"), nil)
		return
	case err != nil:
		log.Printf("Failed to revoke session %s of user %s: %v", sessionID, claims.UUID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_END_SESSION"), nil)
		return
	}
	if sessionID == claims.SessionID {
		auth.ClearRefreshTokenCookie(c)
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("SESSION_ENDED"), nil)
}

// RevokeOtherSessions ends every session of the signed-in user but the
// session of the request, for example after a device was lost. It is
// DELETE on the collection rather than a path under it, which the route of
// one session would also match.
//
// @Summary End every session of the signed-in user but the current one
// @Tags user sessions
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=RevokedSessions}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/sessions [delete]
func (h *UserHandler) RevokeOtherSessions(c *gin.Context) {
	claims, ok := sessionCaller(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), sessionTimeout)
	defer cancel()
	event := auth.SessionEvent(c, audit.ActionUserSessionRevoke, claims.UUID, "")
	revoked, err := h.Sessions.RevokeOthers(ctx, claims.UUID, claims.SessionID, event)
	if err != nil {
		log.Printf("Failed to revoke the other sessions of user %s: %v", claims.UUID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_END_OTHER_SESSIONS"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("OTHER_SESSIONS_ENDED"), RevokedSessions{Revoked: revoked})
}
