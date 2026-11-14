package app

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/adminauth"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/handlers/admin"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAdminRoutes_EveryChangeIsClassified lists the admin routes of the real
// router. Each one that changes something must have a class in adminRoutes,
// so that a new route cannot skip the decision of whether it needs a step-up
// (#308), and the table must not name a route that is gone.
func TestAdminRoutes_EveryChangeIsClassified(t *testing.T) {
	env := newE2EEnv(t)
	registered := map[string]bool{}
	for _, route := range env.router.Routes() {
		path, ok := strings.CutPrefix(route.Path, adminRoutePrefix)
		// Sign-in is public: it has no token to step up.
		if !ok || strings.HasPrefix(path, "/auth") {
			continue
		}
		key := route.Method + " " + path
		registered[key] = true
		class := adminRouteClass(route.Method, route.Path)
		if route.Method == http.MethodGet {
			assert.Equal(t, admin.RouteRead, class, "%s only reads", key)
			continue
		}
		assert.NotEqual(t, admin.RouteUnclassified, class, "%s changes something and has no class in adminRoutes", key)
	}
	require.Greater(t, len(registered), 40, "the admin routes are registered")
	for key := range adminRoutes {
		assert.True(t, registered[key], "adminRoutes names %s, which is not a route", key)
	}

	// What the issue names: platform controls, restrictions, delistings,
	// migrations, capabilities, and seals; and the SMS routing (#371).
	sensitive := 0
	for key, class := range adminRoutes {
		if class != admin.RouteSensitive {
			continue
		}
		sensitive++
		named := false
		for _, part := range []string{"/platform/controls", "/restrictions", "/delistings", "/migrations", "/capabilities/", "/reseal", "/notifications/sms"} {
			named = named || strings.Contains(key, part)
		}
		assert.True(t, named, key)
	}
	assert.Equal(t, 13, sensitive)
	assert.Equal(t, admin.RouteUnclassified, adminRouteClass(http.MethodPost, adminRoutePrefix+"/new-route"))
}

// TestAdminStepUp_EndToEnd uses a sensitive route with a session whose
// step-up has passed, and steps up with an authenticator code (#308).
func TestAdminStepUp_EndToEnd(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	enrollment, err := env.accounts.Create(ctx, "step-up@example.com")
	require.NoError(t, err)
	token := env.signInAdmin(t, enrollment)

	// A restriction of a user who does not exist: past the step-up, the
	// route answers for itself.
	sensitive := func(token string) (int, string) {
		status, resp := env.post(t, "/api/v1/admin/users/00000000-0000-4000-8000-000000000000/restrictions",
			map[string]string{"kind": "freeze", "reason": "test"}, token)
		return status, resp.Code
	}
	lapse := func() {
		_, err := env.db.Exec(`UPDATE admin_sessions SET elevated_until = NOW() - INTERVAL '1 second' WHERE admin_id = $1`, enrollment.AdminID)
		require.NoError(t, err)
	}
	freshCode := func() string {
		_, err := env.db.Exec(`UPDATE users SET totp_last_step = NULL WHERE hk = $1`, enrollment.AdminID)
		require.NoError(t, err)
		return currentTOTP(t, enrollment)
	}

	status, code := sensitive(token)
	assert.NotEqual(t, http.StatusForbidden, status, "a sign-in counts as a step-up: %s", code)

	lapse()
	status, code = sensitive(token)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, admin.CodeStepUpRequired, code)
	status, resp := env.post(t, "/api/v1/admin/currencies", testCurrency("STP"), token)
	assert.Equal(t, http.StatusCreated, status, "an ordinary change needs no step-up: %s", resp.Message)
	assert.Equal(t, http.StatusOK, env.send(t, http.MethodGet, "/api/v1/admin/platform", nil, token, "").Code)

	status, resp = env.post(t, "/api/v1/admin/step-up", map[string]string{"totp": "000000"}, token)
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, "code_invalid", resp.Code)
	status, _ = env.post(t, "/api/v1/admin/step-up", map[string]string{}, token)
	assert.Equal(t, http.StatusBadRequest, status)
	status, code = sensitive(token)
	assert.Equal(t, http.StatusForbidden, status, code)

	status, resp = env.post(t, "/api/v1/admin/step-up", map[string]string{"totp": freshCode()}, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.NotEmpty(t, resp.Info["elevated_until"])
	status, code = sensitive(token)
	assert.NotEqual(t, http.StatusForbidden, status, code)

	status, resp = env.post(t, "/api/v1/admin/step-up", map[string]string{"totp": currentTOTP(t, enrollment)}, token)
	assert.Equal(t, http.StatusUnauthorized, status, "a code is accepted once: %s", resp.Message)

	outcome := func(e auditRow) string { return e.Outcome + "/" + e.Reason.String }
	events := auditEvents(t, env.db, audit.ActionAdminStepUp)
	require.Len(t, events, 3)
	assert.Equal(t, []string{"failure/authenticator_code_invalid", "success/", "failure/authenticator_code_invalid"},
		[]string{outcome(events[0]), outcome(events[1]), outcome(events[2])})

	t.Run("too many wrong codes end the session", func(t *testing.T) {
		for i := 2; i < adminauth.MaxStepUpFailures; i++ {
			status, _ := env.post(t, "/api/v1/admin/step-up", map[string]string{"totp": "000000"}, token)
			require.Equal(t, http.StatusUnauthorized, status)
		}
		// The count started again at the accepted code, so one wrong code
		// was counted before this subtest.
		for i := 0; i < 2; i++ {
			env.post(t, "/api/v1/admin/step-up", map[string]string{"totp": "000000"}, token)
		}
		status, resp := env.post(t, "/api/v1/admin/step-up", map[string]string{"totp": freshCode()}, token)
		assert.Equal(t, http.StatusUnauthorized, status)
		assert.Equal(t, "session_inactive", resp.Code, "the session ended")
	})
}
