package public

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/denylink"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/gin-gonic/gin"
)

// Denier uses the "this was not me" links (#374): denylink.Service.
type Denier interface {
	Deny(ctx context.Context, token string, origin audit.Event) (denylink.Outcome, error)
}

// codeDenyLinkInvalid answers a token that is not of a usable link.
const codeDenyLinkInvalid = "deny_link_invalid"

// Deny uses a "this was not me" link.
//
// @Summary Use a "this was not me" link
// @Description Every message of a withdrawal confirmation code carries a link to a page of the exchange's site with a token. Opening the link changes nothing: the page asks the user and posts the token here. The link can only stop, never approve. It denies the confirmation, which can no longer be confirmed; cancels every withdrawal of the user whose payouts have not started, with its funds released; ends every session of the user; and starts the security hold. The user then gets a notice by email. A token is used once and expires. An unknown, expired, or used token answers 404 deny_link_invalid, which names no account. The route is rate-limited per IP.
// @Tags user-security
// @Produce json
// @Param token path string true "The token of the link: 12 letters and digits"
// @Success 200 {object} models.APIResponse{info=denylink.Outcome} "What the link did"
// @Failure 404 {object} models.APIResponse "deny_link_invalid"
// @Failure 429 {object} models.APIResponse "A rate limit"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/deny/{token} [post]
func (h *PublicHandler) Deny(c *gin.Context) {
	if h.Denials == nil {
		helpers.SendError(c, http.StatusNotFound, codeDenyLinkInvalid, i18n.M("DENY_LINK_INVALID"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	out, err := h.Denials.Deny(ctx, c.Param("token"), audit.FromRequest(c))
	switch {
	case errors.Is(err, denylink.ErrInvalid):
		helpers.SendError(c, http.StatusNotFound, codeDenyLinkInvalid, i18n.M("DENY_LINK_INVALID"), nil)
	case err != nil:
		log.Printf("Failed to use a deny link: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("DENY_LINK_FAILED"), nil)
	default:
		helpers.SendResponse(c, http.StatusOK, i18n.M("DENY_LINK_USED"), out)
	}
}
