package smsrouting

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const adminHK = "5f0c9b4e-2d1a-4c3b-9e8f-7a6b5c4d3e2f"

func TestStore_KeepsTheRoutingSealedAndAudited(t *testing.T) {
	ctx := context.Background()
	conn := pgtest.Migrated(t, "testdb_smsrouting")
	sealtest.Use(t, conn, rowseal.ModeEnforce)
	s := Store{DB: conn}

	routing, err := s.SMSRouting(ctx)
	require.NoError(t, err)
	assert.Equal(t, notify.SMSRouting{Mode: notify.SMSRoutingAuto}, routing, "none set is auto")

	manual := notify.SMSRouting{Mode: notify.SMSRoutingManual, Order: []string{"smsir", "kavenegar"}}
	require.NoError(t, s.Set(ctx, manual, adminHK, audit.Event{IP: "203.0.113.9"}))
	routing, err = s.SMSRouting(ctx)
	require.NoError(t, err)
	assert.Equal(t, manual, routing)

	var changes []byte
	require.NoError(t, conn.QueryRow(`SELECT changes FROM audit_events WHERE action = $1 AND actor_id = $2 AND target_id = 'sms'`,
		audit.ActionNotificationSMSRoutingSet, adminHK).Scan(&changes))
	var c audit.Changes
	require.NoError(t, json.Unmarshal(changes, &c))
	assert.Equal(t, "auto", c.Before["mode"])
	assert.Equal(t, "manual", c.After["mode"])
	assert.Equal(t, []any{"smsir", "kavenegar"}, c.After["order"])

	// An order changed in SQL fails the seal: the auto mode applies.
	_, err = conn.Exec(`UPDATE sms_routing SET provider_order = 'kavenegar' WHERE id = 1`)
	require.NoError(t, err)
	routing, err = s.SMSRouting(ctx)
	require.NoError(t, err)
	assert.Equal(t, notify.SMSRouting{Mode: notify.SMSRoutingAuto}, routing)

	require.NoError(t, s.Set(ctx, notify.SMSRouting{Mode: notify.SMSRoutingAuto}, adminHK, audit.Event{}))
	routing, err = s.SMSRouting(ctx)
	require.NoError(t, err)
	assert.Equal(t, notify.SMSRoutingAuto, routing.Mode)
	assert.Empty(t, routing.Order)
}
