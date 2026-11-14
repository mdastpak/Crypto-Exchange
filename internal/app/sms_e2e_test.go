package app

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/smsrouting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// e2eSMSProvider is an SMS provider that records what it sends.
type e2eSMSProvider struct {
	name       string
	withdrawal bool
	mu         sync.Mutex
	sent       int
}

func (p *e2eSMSProvider) Name() string { return p.name }

func (p *e2eSMSProvider) Supports(purpose string) bool {
	return purpose == "" || (purpose == notify.PurposeWithdrawal && p.withdrawal)
}

func (p *e2eSMSProvider) Send(context.Context, notify.OTPMessage) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent++
	return "", nil
}

func (p *e2eSMSProvider) Reports(context.Context, []string) (map[string]notify.DeliveryState, error) {
	return nil, nil
}

func (p *e2eSMSProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent
}

// TestAdminSMSRouting walks through the switch of #371: an administrator
// sees the providers, puts them in a manual order, which every message
// then follows, and goes back to the automatic failover; an order that
// would leave withdrawal codes without their template is refused.
func TestAdminSMSRouting(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	enrollment, err := env.accounts.Create(ctx, "sms-ops@example.com")
	require.NoError(t, err)
	token := env.signInAdmin(t, enrollment)
	const route = "/api/v1/admin/notifications/sms"

	status, raw := env.call(t, http.MethodGet, route, nil, token)
	assert.Equal(t, http.StatusNotFound, status, string(raw))
	assert.Contains(t, string(raw), "sms_not_configured")

	kavenegar, smsir := &e2eSMSProvider{name: "kavenegar", withdrawal: true}, &e2eSMSProvider{name: "smsir"}
	router := notify.NewSMSRouter([]notify.SMSProvider{kavenegar, smsir}, notify.SMSRouterOptions{})
	store := smsrouting.Store{DB: env.db}
	router.UseRouting(store)
	env.handlers.Admin.SMS, env.handlers.Admin.SMSRoutes = router, store

	read := func(raw []byte) notify.SMSStatus {
		t.Helper()
		var body struct {
			Info notify.SMSStatus `json:"info"`
		}
		require.NoError(t, json.Unmarshal(raw, &body), string(raw))
		return body.Info
	}
	status, raw = env.call(t, http.MethodGet, route, nil, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	st := read(raw)
	assert.Equal(t, notify.SMSRoutingAuto, st.Mode)
	assert.Equal(t, []string{"kavenegar", "smsir"}, st.Effective)
	require.Len(t, st.Providers, 2)
	assert.Equal(t, []string{"sign_in", "withdrawal"}, st.Providers[0].Templates)
	assert.Equal(t, []string{"sign_in"}, st.Providers[1].Templates)

	status, resp := env.put(t, route, map[string]any{"mode": "manual", "order": []string{"smsir"}}, token)
	assert.Equal(t, http.StatusBadRequest, status, "withdrawal codes would lose SMS")
	assert.Equal(t, "validation_failed", resp.Code)
	status, _ = env.put(t, route, map[string]any{"mode": "manual", "order": []string{"smsir", "pigeon"}}, token)
	assert.Equal(t, http.StatusBadRequest, status)

	status, raw = env.call(t, http.MethodPut, route, map[string]any{"mode": "manual", "order": []string{"smsir", "kavenegar"}}, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	st = read(raw)
	assert.Equal(t, notify.SMSRoutingManual, st.Mode)
	assert.Equal(t, []string{"smsir", "kavenegar"}, st.Effective)
	require.NoError(t, router.Deliver(ctx, notify.OTPMessage{ContactType: notify.ContactMobile, Contact: "09121234567", Code: "1"}))
	assert.Equal(t, 1, smsir.count(), "the codes follow the manual order")
	withdrawal := notify.OTPMessage{ContactType: notify.ContactMobile, Contact: "09121234567", Code: "2", Purpose: notify.PurposeWithdrawal}
	require.NoError(t, router.Deliver(ctx, withdrawal))
	assert.Equal(t, 1, kavenegar.count(), "a withdrawal code goes to the provider with its template")

	status, raw = env.call(t, http.MethodPut, route, map[string]any{"mode": "auto"}, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Equal(t, []string{"kavenegar", "smsir"}, read(raw).Effective)
	assert.Len(t, auditEvents(t, env.db, audit.ActionNotificationSMSRoutingSet), 2)
}
