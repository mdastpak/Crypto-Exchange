// Package smsrouting keeps how administrators route SMS through the
// providers (#371): the auto mode, which follows the configured order with
// the unhealthy providers last, or a manual order. Every instance reads
// the same row. It is sealed (#139): a routing changed in SQL is ignored,
// and the auto mode applies.
package smsrouting

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// Store keeps the routing.
type Store struct {
	DB *sql.DB
}

var _ notify.SMSRoutingSource = Store{}

// SMSRouting returns the routing: auto when none was set, or when the row
// fails its seal in enforce mode, whose finding is then recorded.
func (s Store) SMSRouting(ctx context.Context) (notify.SMSRouting, error) {
	var mode, order string
	row := rowseal.SMSRouting.NewRow()
	err := s.DB.QueryRowContext(ctx, `SELECT mode, provider_order, `+rowseal.SMSRouting.Fields("")+` FROM sms_routing WHERE id = 1`).
		Scan(append([]any{&mode, &order}, row.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return notify.SMSRouting{Mode: notify.SMSRoutingAuto}, nil
	}
	if err != nil {
		return notify.SMSRouting{}, fmt.Errorf("read the SMS routing: %w", err)
	}
	if err := rowseal.Check(ctx, rowseal.SMSRouting, row); err != nil {
		if errors.Is(err, rowseal.ErrUnverified) {
			return notify.SMSRouting{Mode: notify.SMSRoutingAuto}, nil
		}
		return notify.SMSRouting{}, err
	}
	routing := notify.SMSRouting{Mode: mode}
	if order != "" {
		routing.Order = strings.Split(order, ",")
	}
	return routing, nil
}

// Set records routing, which the caller checked, as the choice of the
// administrator adminHK, with origin, the audit event of the request.
func (s Store) Set(ctx context.Context, routing notify.SMSRouting, adminHK string, origin audit.Event) error {
	before, err := s.SMSRouting(ctx)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	order := strings.Join(routing.Order, ",")
	if _, err := tx.ExecContext(ctx, `INSERT INTO sms_routing (id, mode, provider_order, updated_by) VALUES (1, $1, $2, $3)
		ON CONFLICT (id) DO UPDATE SET mode = EXCLUDED.mode, provider_order = EXCLUDED.provider_order,
			updated_by = EXCLUDED.updated_by, updated_at = NOW()`, routing.Mode, order, adminHK); err != nil {
		return fmt.Errorf("record the SMS routing: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.SMSRouting, "id = $1", 1); err != nil {
		return err
	}
	event := origin
	event.ActorType, event.ActorID = audit.ActorAdmin, adminHK
	event.Action, event.TargetType, event.TargetID = audit.ActionNotificationSMSRoutingSet, audit.TargetNotificationChannel, "sms"
	event.Outcome = audit.Success
	event.Changes = &audit.Changes{
		Before: map[string]any{"mode": before.Mode, "order": before.Order},
		After:  map[string]any{"mode": routing.Mode, "order": routing.Order},
	}
	if err := audit.Record(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit()
}
