package socket

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/platform"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixedGate(t *testing.T, controls ...*platform.Control) *platform.Gate {
	t.Helper()
	w, err := platform.Fixed(controls...)
	require.NoError(t, err)
	return &platform.Gate{Watcher: w}
}

func maintenance(allowUsers ...string) *platform.Control {
	end := time.Now().Add(time.Hour)
	return &platform.Control{HK: "m", Kind: platform.KindMaintenance, StartsAt: time.Now().Add(-time.Minute),
		EndsAt: &end, AnnounceAt: time.Now().Add(-time.Minute), AllowUsers: allowUsers}
}

// TestServe_RefusesUpgradesThatAControlCloses refuses the upgrade during a
// maintenance and a WebSocket lockdown, in the envelope of the REST routes,
// and lets an allowed user through (#235).
func TestServe_RefusesUpgradesThatAControlCloses(t *testing.T) {
	websocketLockdown := platform.FeatureWebSocket
	for name, tc := range map[string]struct {
		control  *platform.Control
		header   http.Header
		wantCode string
	}{
		"maintenance":         {maintenance(), nil, platform.CodeMaintenance},
		"websocket lockdown":  {&platform.Control{Kind: platform.KindLockdown, Feature: &websocketLockdown, StartsAt: time.Now().Add(-time.Minute)}, nil, platform.CodeFeatureLocked},
		"allowed user passes": {maintenance("user-1"), http.Header{"Authorization": {"Bearer good"}}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			h := newTestHandler()
			h.Authenticate = fakeAuth
			h.Platform = fixedGate(t, tc.control)
			_, resp, err := dial(t, gateway(t, h), tc.header)
			require.NotNil(t, resp)
			if tc.wantCode == "" {
				require.NoError(t, err)
				assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
				return
			}
			require.Error(t, err)
			assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			var body struct {
				Code string `json:"code"`
			}
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
			assert.Equal(t, tc.wantCode, body.Code)
			if tc.control.EndsAt != nil {
				assert.NotEmpty(t, resp.Header.Get("Retry-After"))
			}
			assert.Equal(t, 0, h.clientCount())
		})
	}
}

// TestApplyPlatform_TellsClientsAndClosesTheLockedOnes sends every client
// the status, and then closes those that a maintenance closes with 1013,
// keeping an allowed user.
func TestApplyPlatform_TellsClientsAndClosesTheLockedOnes(t *testing.T) {
	h := newTestHandler()
	h.Authenticate = fakeAuth
	url := gateway(t, h)
	anonymous, _, err := dial(t, url, nil)
	require.NoError(t, err)
	allowed, _, err := dial(t, url, http.Header{"Authorization": {"Bearer good"}})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return h.clientCount() == 2 }, 2*time.Second, 5*time.Millisecond)

	gate := fixedGate(t, maintenance("user-1"))
	h.Platform = gate
	h.ApplyPlatform(gate.Watcher.Status())

	for _, conn := range []*websocket.Conn{anonymous, allowed} {
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
		var event StatusEvent
		require.NoError(t, conn.ReadJSON(&event))
		assert.Equal(t, "platform.status", event.Event)
		assert.Equal(t, platform.ModeMaintenance, event.Status.Mode)
	}
	_, _, err = anonymous.ReadMessage()
	var closeErr *websocket.CloseError
	require.ErrorAs(t, err, &closeErr)
	assert.Equal(t, CloseTryAgainLater, closeErr.Code)
	assert.Equal(t, platform.CodeMaintenance, closeErr.Text)

	require.Eventually(t, func() bool { return h.clientCount() == 1 }, 3*time.Second, 10*time.Millisecond)
	assert.Equal(t, "user-1", onlyClient(t, h).UserID, "the allowed user stays connected")
}
