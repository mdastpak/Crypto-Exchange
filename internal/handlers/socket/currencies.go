package socket

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/google/uuid"
)

// maxEventBytes bounds an event from the broker. A currency event is a few
// hundred bytes.
const maxEventBytes = 16 << 10

// removalStatuses are the statuses that take a currency out of the public
// view, which a delete event reports.
var removalStatuses = map[string]bool{
	models.CurrencyStatusPending: true,
	models.CurrencyStatusSuspend: true,
	models.CurrencyStatusDeleted: true,
}

// handleCurrencyUpdate forwards a currency event from RabbitMQ to every
// connected client, rebuilt from the fields of the schema with the server
// time added. Anything that can publish to the exchange could otherwise push
// any content to every client, so an event that does not fit the schema is
// dropped and logged. The consumer acknowledges it either way: delivering a
// malformed event again cannot succeed.
func (h *WebSocketHandler) handleCurrencyUpdate(body []byte) {
	data, err := sanitizeCurrencyEvent(body, time.Now())
	if err != nil {
		log.Printf("Dropped an event from the broker: %v", err)
		return
	}
	h.Broadcast(data)
}

// sanitizeCurrencyEvent checks body against the currency event schema and
// returns the event that clients receive. Only the fields of the schema are
// kept: unknown fields, in the event and in its info, are dropped.
func sanitizeCurrencyEvent(body []byte, now time.Time) ([]byte, error) {
	if len(body) > maxEventBytes {
		return nil, fmt.Errorf("the event has %d bytes, more than %d", len(body), maxEventBytes)
	}
	var event models.CurrencyEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return nil, fmt.Errorf("the event is not a currency event: %w", err)
	}
	if event.Entity != models.CurrencyEventEntity {
		return nil, fmt.Errorf("unknown entity %s", quoted(event.Entity))
	}
	if id, err := uuid.Parse(event.HK); err != nil || id.String() != event.HK {
		return nil, fmt.Errorf("hk %s is not a UUID in canonical form", quoted(event.HK))
	}

	var info interface{}
	switch event.Action {
	case models.CurrencyEventCreate, models.CurrencyEventUpdate:
		var currency models.CurrencyInfo
		if err := decodeInfo(event.Info, &currency); err != nil {
			return nil, fmt.Errorf("%s event of %s: %w", event.Action, event.HK, err)
		}
		info = currency
	case models.CurrencyEventDelete:
		var removal models.CurrencyRemoval
		if err := decodeInfo(event.Info, &removal); err != nil {
			return nil, fmt.Errorf("delete event of %s: %w", event.HK, err)
		}
		if !removalStatuses[removal.Status] {
			return nil, fmt.Errorf("delete event of %s: unknown status %s", event.HK, quoted(removal.Status))
		}
		info = removal
	default:
		return nil, fmt.Errorf("unknown action %s", quoted(event.Action))
	}

	infoJSON, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	return json.Marshal(models.CurrencyEvent{
		Entity:     event.Entity,
		Action:     event.Action,
		HK:         event.HK,
		Info:       infoJSON,
		ServerTime: now.UTC().Format(time.RFC3339),
	})
}

// decodeInfo decodes the info of an event, which must be a JSON object.
func decodeInfo(raw json.RawMessage, into interface{}) error {
	if len(raw) == 0 || raw[0] != '{' {
		return errors.New("info is missing or not an object")
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("invalid info: %w", err)
	}
	return nil
}

// quoted quotes a value from an event for the log, shortened, so that a
// hostile event cannot flood or forge log lines.
func quoted(value string) string {
	const limit = 64
	if runes := []rune(value); len(runes) > limit {
		return fmt.Sprintf("%q...", string(runes[:limit]))
	}
	return fmt.Sprintf("%q", value)
}
