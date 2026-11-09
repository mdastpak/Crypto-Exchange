package socket

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/platform"
)

// CloseTryAgainLater ends the connections that a maintenance or a
// WebSocket lockdown closes (#235): RFC 6455 code 1013, with the error code
// of the REST routes as the reason.
const CloseTryAgainLater = 1013

// platformCloseDelay lets the status event reach a client before its
// connection closes.
const platformCloseDelay = time.Second

// platformLocked returns what closes the WebSocket for subject, or nil.
func (h *WebSocketHandler) platformLocked(subject platform.Subject) *platform.Locked {
	if locked := h.Platform.Locked(platform.UseService, subject); locked != nil {
		return locked
	}
	return h.Platform.Locked(platform.FeatureWebSocket, subject)
}

// rejectLocked answers an upgrade that a control refuses, as the REST routes
// answer: 503, the code, and Retry-After when the control has an end.
func rejectLocked(w http.ResponseWriter, locale i18n.Negotiated, locked *platform.Locked) {
	if wait := locked.RetryAfter(time.Now()); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)))
	}
	message := i18n.M("MAINTENANCE")
	if locked.Kind != platform.KindMaintenance {
		message = i18n.M("LIVE_UPDATES_UNAVAILABLE")
	}
	text, language := locale.Render(message)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Language", language)
	w.Header().Add("Vary", i18n.VaryHeaders)
	w.WriteHeader(http.StatusServiceUnavailable)
	info := platform.LockedInfo{Kind: locked.Kind, Feature: locked.Feature, Message: locked.Message, StateUnknown: locked.StateUnknown}
	if locked.EndsAt != nil {
		end := locked.EndsAt.UTC()
		info.EndsAt = &end
	}
	_ = json.NewEncoder(w).Encode(models.APIResponse{
		Status: http.StatusServiceUnavailable, Message: text, Code: locked.Code(),
		RequestID: w.Header().Get(requestIDHeader), Info: info,
	})
}

// StatusEvent is the event that every connection receives when what users
// see of the platform controls changes: a maintenance or lockdown that is
// announced, starts, ends, or is lifted.
type StatusEvent struct {
	Event  string          `json:"event" example:"platform.status"`
	Status platform.Status `json:"status"`
}

// ApplyPlatform tells every connection of this instance the new status,
// and then closes the connections that a maintenance or a WebSocket
// lockdown now closes, with 1013. Each instance applies it to its own
// connections, from its own copy of the controls, so it needs no broker.
func (h *WebSocketHandler) ApplyPlatform(status platform.Status) {
	data, err := json.Marshal(StatusEvent{Event: "platform.status", Status: status})
	if err != nil {
		log.Printf("WebSocket: encoding the platform status: %v", err)
		return
	}
	h.Broadcast(data)
	if h.Platform == nil {
		return
	}
	// Under Mu, as registerClient, so that Shutdown never waits for a
	// goroutine that starts after it.
	h.Mu.Lock()
	select {
	case <-h.ShutdownCh:
		h.Mu.Unlock()
		return
	default:
	}
	h.Wg.Add(1)
	h.Mu.Unlock()
	go func() {
		defer h.Wg.Done()
		select {
		case <-time.After(platformCloseDelay):
		case <-h.ShutdownCh:
			return
		}
		h.closeLocked()
	}()
}

// closeLocked closes the connections that a control closes now.
func (h *WebSocketHandler) closeLocked() {
	h.Mu.RLock()
	clients := make([]*Client, 0, len(h.Clients))
	for client := range h.Clients {
		clients = append(clients, client)
	}
	h.Mu.RUnlock()
	closed := 0
	for _, client := range clients {
		_, user := client.principal()
		if locked := h.platformLocked(platform.Subject{UserHK: user, IP: client.addr}); locked != nil {
			h.closeClient(client, CloseTryAgainLater, locked.Code())
			closed++
		}
	}
	if closed > 0 {
		log.Printf("WebSocket: closed %d connections for a platform control", closed)
	}
}
