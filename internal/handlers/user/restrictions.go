package user

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"

	"github.com/gin-gonic/gin"
)

// RestrictionList is the restrictions that apply to the signed-in user.
type RestrictionList struct {
	Items []restrictions.PublicRestriction `json:"items"`
	// SecurityHoldUntil is when the security hold ends (#251): after a
	// change of a contact or of the second factor, withdrawals and the
	// other operations that a second factor protects answer 403
	// security_hold until then. It is absent when no hold applies.
	SecurityHoldUntil *time.Time `json:"security_hold_until,omitempty"`
}

// ListRestrictions returns the restrictions that apply to the signed-in user
// (#220), so that a client can disable what they block ahead of time.
//
// @Summary List my restrictions
// @Description The restrictions that apply now to the signed-in user: the operations they block, on every currency or on one, and when they end when that is shown. Never the reason. A frozen account cannot sign in, so it is not listed here: its requests answer 403 account_frozen. security_hold_until is when the hold that follows a change of a contact or of the second factor ends; until then, withdrawals answer 403 security_hold.
// @Tags user
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=RestrictionList}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/restrictions [get]
func (h *UserHandler) ListRestrictions(c *gin.Context) {
	userID, ok := callerID(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, "Unauthorized", nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	active, err := restrictions.Active(ctx, h.PostgresDB.DB, userID.String())
	if err != nil {
		log.Printf("Failed to list the restrictions of a user: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to list restrictions.", nil)
		return
	}
	hold, err := restrictions.SecurityHold(ctx, h.PostgresDB.DB, userID.String())
	if err != nil {
		log.Printf("Failed to read the security hold of a user: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to list restrictions.", nil)
		return
	}
	list := RestrictionList{Items: make([]restrictions.PublicRestriction, 0, len(active))}
	if hold != nil {
		utc := hold.UTC()
		list.SecurityHoldUntil = &utc
	}
	for _, r := range active {
		list.Items = append(list.Items, restrictions.Public(r))
	}
	helpers.SendResponse(c, http.StatusOK, "Restrictions retrieved successfully.", list)
}
