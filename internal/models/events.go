package models

import "encoding/json"

// Currency events travel on the price updates exchange, one for each change
// of the public view of a currency. The admin handler writes them to the
// outbox in the transaction of the change, the outbox relay publishes them,
// and the WebSocket gateway forwards them to clients, after checking them
// against this schema: anything else in a message is dropped.
const (
	// CurrencyEventTopic is the outbox topic of currency events.
	CurrencyEventTopic  = "currencies"
	CurrencyEventEntity = "currencies"
	CurrencyEventCreate = "create"
	CurrencyEventUpdate = "update"
	CurrencyEventDelete = "delete"
)

// CurrencyEvent is a currency event as it is published and as clients
// receive it. Info is the currency's CurrencyInfo for create and update, and
// a CurrencyRemoval for delete.
type CurrencyEvent struct {
	// ID identifies the event, so that clients can drop a repeated one:
	// delivery is at least once. It is a UUID.
	ID     string          `json:"id,omitempty"`
	Entity string          `json:"entity"`
	Action string          `json:"action"`
	HK     string          `json:"hk"`
	Info   json.RawMessage `json:"info,omitempty"`
	// Lifecycle is the lifecycle of the currency after the change (#216),
	// on create and update events of a change that sets it. Without it, the
	// lifecycle stays as it was.
	Lifecycle  *CurrencyLifecycle `json:"lifecycle,omitempty"`
	ServerTime string             `json:"server_time"`
}

// CurrencyRemoval is the info of a delete event: the status that took the
// currency out of the public view.
type CurrencyRemoval struct {
	Status string `json:"status"`
}
