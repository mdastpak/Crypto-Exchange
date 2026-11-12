package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/adminauth"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/contact"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/totp"

	"github.com/gin-gonic/gin"
)

// RouteClass says what an admin route needs beyond an admin token (#308).
type RouteClass int

const (
	// RouteUnclassified is a route that no table lists. A request that
	// changes something through one is refused.
	RouteUnclassified RouteClass = iota
	// RouteRead reads and changes nothing.
	RouteRead
	// RouteChange changes something an administrator's token is enough for.
	RouteChange
	// RouteSensitive changes what one stolen token must not be enough for:
	// it needs a step-up, a fresh authenticator code on the session.
	RouteSensitive
)

// CodeStepUpRequired answers a sensitive route on a session that has not
// stepped up lately.
const CodeStepUpRequired = "step_up_required"

// stepUpLifetime is admin.step_up_minutes, or its default where the
// configuration was not loaded from a file.
func stepUpLifetime(cfg *config.Configuration) time.Duration {
	minutes := cfg.Admin.StepUpMinutes
	if minutes < 1 {
		minutes = config.DefaultAdminStepUpMinutes
	}
	return time.Duration(minutes) * time.Minute
}

// RequireStepUp runs after RequireActiveAdmin on every admin route. classify
// returns the class of a route by its method and pattern. A sensitive route
// needs a session that signed in or stepped up within admin.step_up_minutes;
// a route that is not classified is refused unless it only reads, so that a
// new route cannot skip the decision.
func (h *AdminHandler) RequireStepUp(classify func(method, route string) RouteClass) gin.HandlerFunc {
	return func(c *gin.Context) {
		class := classify(c.Request.Method, c.FullPath())
		switch class {
		case RouteRead, RouteChange:
			c.Next()
			return
		case RouteUnclassified:
			if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
				c.Next()
				return
			}
			log.Printf("Refused %s %s: the admin route is not classified", c.Request.Method, c.FullPath())
			helpers.SendError(c, http.StatusForbidden, "route_not_classified", i18n.M("ADMIN_ROUTE_NOT_CLASSIFIED"), nil)
			c.Abort()
			return
		}
		claims, ok := auth.GetClaims(c)
		if !ok || h.Accounts == nil {
			helpers.SendError(c, http.StatusUnauthorized, helpers.CodeTokenInvalid, i18n.M("UNAUTHORIZED_INVALID_TOKEN"), nil)
			c.Abort()
			return
		}
		elevated, err := h.Accounts.Elevated(c.Request.Context(), claims.SessionID, claims.UUID)
		if err != nil {
			log.Printf("Failed to check the step-up of administrator %s: %v", claims.UUID, err)
			helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CHECK_ADMIN"), nil)
			c.Abort()
			return
		}
		if !elevated {
			helpers.SendError(c, http.StatusForbidden, CodeStepUpRequired, i18n.M("ADMIN_STEP_UP_REQUIRED"),
				gin.H{"step_up": helpers.APIPrefix + "/admin/step-up"})
			c.Abort()
			return
		}
		c.Next()
	}
}

type stepUpRequest struct {
	TOTP string `json:"totp"`
}

// StepUpInfo is the answer to a step-up.
type StepUpInfo struct {
	// ElevatedUntil is until when the session may use the sensitive routes.
	ElevatedUntil time.Time `json:"elevated_until"`
}

// StepUp elevates the session of the token with a fresh authenticator code.
//
// @Summary Step up with a fresh authenticator code
// @Description The sensitive admin routes (platform controls, user restrictions, delistings, currency migrations, capability overrides, and re-seals) need a session that gave an authenticator (TOTP) code within admin.step_up_minutes; without it they answer 403 step_up_required. A sign-in counts as one. Each code is accepted once. A wrong code answers 401 code_invalid, and after 5 wrong codes the next one ends the session. Audited as admin.step_up.
// @Tags admin sign-in
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body stepUpRequest true "The authenticator code"
// @Success 200 {object} models.APIResponse{info=StepUpInfo}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/step-up [post]
func (h *AdminHandler) StepUp(c *gin.Context) {
	claims, ok := auth.GetClaims(c)
	if !ok || h.Accounts == nil {
		helpers.SendError(c, http.StatusUnauthorized, helpers.CodeTokenInvalid, i18n.M("UNAUTHORIZED_INVALID_TOKEN"), nil)
		return
	}
	var req stepUpRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.TOTP == "" {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("TOTP_REQUIRED"), nil)
		return
	}
	// A code typed in Persian digits is the same code (#197).
	req.TOTP, _ = contact.Code(req.TOTP, totp.Digits)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	event := h.signInEvent(c, audit.ActionAdminStepUp, claims.UUID, audit.Success, "")
	event.TargetType, event.TargetID = audit.TargetAdminSession, claims.SessionID
	until, err := h.Accounts.StepUp(ctx, claims.SessionID, claims.UUID, req.TOTP, stepUpLifetime(h.settings()), event)
	switch {
	case errors.Is(err, adminauth.ErrInvalidCode), errors.Is(err, adminauth.ErrNotFound):
		helpers.SendError(c, http.StatusUnauthorized, "code_invalid", i18n.M("INVALID_OR_EXPIRED_CODE"), nil)
	case errors.Is(err, adminauth.ErrSessionEnded):
		c.Header("WWW-Authenticate", `Bearer`)
		helpers.SendError(c, http.StatusUnauthorized, "session_inactive", i18n.M("UNAUTHORIZED_SESSION_IS_NOT_ACTIVE"), nil)
	case err != nil:
		log.Printf("Failed to step up the session of administrator %s: %v", claims.UUID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_STEP_UP"), nil)
	default:
		helpers.SendResponse(c, http.StatusOK, i18n.M("STEPPED_UP_SUCCESSFULLY"), StepUpInfo{ElevatedUntil: until})
	}
}
