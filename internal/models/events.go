package models

import "encoding/json"

// Currency events travel on the price updates exchange, one for each change
// of the public view of a currency. The admin handler publishes them and the
// WebSocket gateway forwards them to clients, after checking them against
// this schema: anything else in a message is dropped.
const (
	CurrencyEventEntity = "currencies"
	CurrencyEventCreate = "create"
	CurrencyEventUpdate = "update"
	CurrencyEventDelete = "delete"
)

// CurrencyEvent is a currency event as it is published and as clients
// receive it. Info is the currency's CurrencyInfo for create and update, and
// a CurrencyRemoval for delete.
type CurrencyEvent struct {
	Entity     string          `json:"entity"`
	Action     string          `json:"action"`
	HK         string          `json:"hk"`
	Info       json.RawMessage `json:"info,omitempty"`
	ServerTime string          `json:"server_time"`
}

// CurrencyRemoval is the info of a delete event: the status that took the
// currency out of the public view.
type CurrencyRemoval struct {
	Status string `json:"status"`
}
