package admin

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/notify"

	"github.com/gin-gonic/gin"
)

// The routing of SMS through the providers (#371): administrators see the
// health of each provider and choose between the automatic failover and
// an order of their own.

// codeSMSNotConfigured answers on a server without SMS providers.
const codeSMSNotConfigured = "sms_not_configured"

// SMSRoutingRequest chooses how SMS goes through the providers.
type SMSRoutingRequest struct {
	// Mode is auto, the configured order with the providers that fail or
	// do not deliver moved behind the others, or manual, Order as it is.
	Mode string `json:"mode" enums:"auto,manual" example:"manual"`
	// Order is the providers in the order that messages try them, for the
	// manual mode; one provider is no failover.
	Order []string `json:"order,omitempty" example:"smsir,kavenegar"`
}

func (h *AdminHandler) smsConfigured(c *gin.Context) bool {
	if h.SMS == nil || h.SMSRoutes == nil {
		helpers.SendError(c, http.StatusNotFound, codeSMSNotConfigured, i18n.M("SMS_NOT_CONFIGURED"), nil)
		return false
	}
	return true
}

// @Summary Show how SMS is routed
// @Description The SMS providers (#371) and how messages go through them: the mode (auto or manual), the order that sign-in codes take now, and for each provider its templates, whether it is healthy, its breaker, and what it did in the last 15 minutes: messages accepted, provider errors, and the delivery reports, delivered and failed. In auto mode, a provider whose breaker is open, after notifications.sms.failover.breaker_failures failures in a row, or whose delivery rate is below min_delivery_percent, goes behind the others.
// @Tags admin notifications
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=notify.SMSStatus}
// @Failure 401 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "sms_not_configured"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/notifications/sms [get]
func (h *AdminHandler) GetSMSRouting(c *gin.Context) {
	if !h.smsConfigured(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	helpers.SendResponse(c, http.StatusOK, i18n.M("SMS_ROUTING_RETRIEVED"), h.SMS.Status(ctx))
}

// @Summary Choose how SMS is routed
// @Description Switches the SMS providers (#371). mode auto follows notifications.sms.providers and passes over the providers that fail or do not deliver, by their health. mode manual sends through order, a list of configured providers, as it is: a failure still moves on to the next in the list, and a list of one is no failover. The order must keep, for each kind of message that a configured provider has a template for (sign-in codes, withdrawal codes, anti-phishing reminders), a provider with that template. Every instance applies the change within five seconds. A sensitive route: it needs a step-up. Audited as notification.sms_routing_set, with the mode and order before and after.
// @Tags admin notifications
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body SMSRoutingRequest true "The routing"
// @Success 200 {object} models.APIResponse{info=notify.SMSStatus}
// @Failure 400 {object} models.APIResponse "validation_failed"
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse "step_up_required"
// @Failure 404 {object} models.APIResponse "sms_not_configured"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/notifications/sms [put]
func (h *AdminHandler) SetSMSRouting(c *gin.Context) {
	if !h.smsConfigured(c) {
		return
	}
	var req SMSRoutingRequest
	if !decodeStrict(c, &req) {
		return
	}
	routing := notify.SMSRouting{Mode: req.Mode, Order: req.Order}
	if err := h.SMS.CheckRouting(routing); err != nil {
		field := "order"
		if req.Mode != notify.SMSRoutingAuto && req.Mode != notify.SMSRoutingManual {
			field = "mode"
		}
		sendValidationErrors(c, fieldErrors{field: err.Error()})
		return
	}
	origin, adminHK, ok := adminOrigin(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if err := h.SMSRoutes.Set(ctx, routing, adminHK, origin); err != nil {
		log.Printf("Failed to set the SMS routing: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("SMS_ROUTING_SET_FAILED"), nil)
		return
	}
	h.SMS.Refresh()
	helpers.SendResponse(c, http.StatusOK, i18n.M("SMS_ROUTING_SET"), h.SMS.Status(ctx))
}
