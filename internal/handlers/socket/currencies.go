package socket

import (
	"encoding/json"
	"log"
	"time"
)

// handleCurrencyUpdate forwards a currency event from RabbitMQ to every
// connected client, with the server time added.
func (h *WebSocketHandler) handleCurrencyUpdate(body []byte) {
	var update map[string]interface{}
	if err := json.Unmarshal(body, &update); err != nil {
		log.Printf("Error unmarshaling currency update: %v", err)
		return
	}

	update["server_time"] = time.Now().UTC().Format(time.RFC3339)

	data, err := json.Marshal(update)
	if err != nil {
		log.Printf("Error marshaling broadcast: %v", err)
		return
	}

	h.Broadcast(data)
}
