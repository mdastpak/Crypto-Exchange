package currencies

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/outbox"

	"github.com/google/uuid"
)

// PublicEvent returns the event of a change to currency in the public view,
// which only approved currencies are in, or nil when the view does not
// change:
//
//   - a currency that becomes public is announced as created,
//   - a public currency that changes is announced as updated,
//   - a currency that stops being public, by a status change or deletion,
//     is announced as deleted,
//   - changes to a currency that stays hidden are not announced at all.
func PublicEvent(wasPublic bool, currency models.Currency) ([]byte, error) {
	isPublic := currency.IsPublic()
	var action string
	var info interface{} = currency.Info
	switch {
	case isPublic && !wasPublic:
		action = models.CurrencyEventCreate
	case isPublic && wasPublic:
		action = models.CurrencyEventUpdate
	case !isPublic && wasPublic:
		action = models.CurrencyEventDelete
		info = models.CurrencyRemoval{Status: currency.Status}
	default:
		return nil, nil
	}
	// The lifecycle travels with the events of the currencies that stay or
	// become public, when the change knows it.
	lifecycle := currency.Lifecycle
	if !isPublic {
		lifecycle = nil
	}
	infoJSON, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	// Version 7 UUIDs grow with time, so the IDs also sort the events.
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	return json.Marshal(models.CurrencyEvent{
		ID:         id.String(),
		Entity:     models.CurrencyEventEntity,
		Action:     action,
		HK:         currency.HK,
		Info:       infoJSON,
		Lifecycle:  lifecycle,
		ServerTime: time.Now().UTC().Format(time.RFC3339),
	})
}

// EnqueueEvent writes the event of a change, if it has one, to the
// outbox in tx, so that it is published if and only if the change commits.
// It reports whether the change touches the public view.
func EnqueueEvent(ctx context.Context, tx *sql.Tx, wasPublic bool, currency models.Currency) (bool, error) {
	event, err := PublicEvent(wasPublic, currency)
	if err != nil || event == nil {
		return false, err
	}
	return true, outbox.Enqueue(ctx, tx, models.CurrencyEventTopic, event)
}
