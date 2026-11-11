package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/notify"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slowSender delivers like capturingSender, after a delay that stands for
// the confirm of the broker.
type slowSender struct {
	*capturingSender
	delay time.Duration
}

func (s slowSender) SendOTP(ctx context.Context, msg notify.OTPMessage) error {
	time.Sleep(s.delay)
	return s.capturingSender.SendOTP(ctx, msg)
}

// The answer to an admin sign-in request must not tell an administrator's
// address from an unknown one, by its content, its limits, or its
// duration (#190).
func TestAdminSignIn_DoesNotRevealAdministrators(t *testing.T) {
	env := newE2EEnv(t)
	const admin, unknown = "timing-admin@example.com", "timing-nobody@example.com"
	_, err := env.accounts.Create(context.Background(), admin)
	require.NoError(t, err)

	t.Run("both take the same minimum time", func(t *testing.T) {
		const sendDelay = 100 * time.Millisecond
		env.handlers.Admin.OTPSender = slowSender{env.sender, sendDelay}
		t.Cleanup(func() { env.handlers.Admin.OTPSender = env.sender })
		t.Cleanup(func() { config.Config.Admin.SignInMinResponseMS = 0 })

		timed := func(email string) time.Duration {
			start := time.Now()
			status, resp := env.post(t, "/api/v1/admin/auth", map[string]string{"email": email}, "")
			elapsed := time.Since(start)
			require.Equal(t, http.StatusOK, status, resp.Message)
			assert.Equal(t, "If this address belongs to an administrator, a sign-in code was sent to it", resp.Message)
			assert.NotEmpty(t, resp.Info["uuid"])
			return elapsed
		}
		// The padding hides the work only when the minimum is above it. On a
		// machine busy with the other test packages, the work can take
		// longer than a fixed minimum (#342), so the minimum follows the
		// work of this run: twice an unpadded request of the administrator.
		config.Config.Admin.SignInMinResponseMS = 0
		ms := int(max(300*time.Millisecond, 2*timed(admin)) / time.Millisecond)
		config.Config.Admin.SignInMinResponseMS = ms
		minimum := time.Duration(ms) * time.Millisecond

		adminTime, unknownTime := timed(admin), timed(unknown)
		assert.GreaterOrEqual(t, adminTime, minimum)
		assert.GreaterOrEqual(t, unknownTime, minimum)
		// Without the padding, the administrator's answer would take at
		// least sendDelay longer.
		diff := adminTime - unknownTime
		if diff < 0 {
			diff = -diff
		}
		assert.Less(t, diff, sendDelay/2, "administrator %v, unknown %v, minimum %v", adminTime, unknownTime, minimum)
		assert.Equal(t, 2, env.sender.count(admin), "the unpadded request and the padded one")
		assert.Zero(t, env.sender.count(unknown), "an unknown address gets no email")
	})

	t.Run("both reach the request limit after the same number of requests", func(t *testing.T) {
		limit := config.Config.OTP.Auth.ContactRequestsPerHour
		if limit == 0 {
			limit = 5 // otp.DefaultContactRequestsPerHour
		}
		const admin2, unknown2 = "limit-admin@example.com", "limit-nobody@example.com"
		_, err := env.accounts.Create(context.Background(), admin2)
		require.NoError(t, err)
		statuses := func(email string) []int {
			var got []int
			for i := 0; i <= limit; i++ {
				status, _ := env.post(t, "/api/v1/admin/auth", map[string]string{"email": email}, "")
				got = append(got, status)
			}
			return got
		}
		adminStatuses := statuses(admin2)
		assert.Equal(t, http.StatusTooManyRequests, adminStatuses[limit])
		assert.Equal(t, adminStatuses, statuses(unknown2))
	})

	t.Run("the session of an unknown address leads nowhere", func(t *testing.T) {
		status, resp := env.post(t, "/api/v1/admin/auth", map[string]string{"email": "another-nobody@example.com"}, "")
		require.Equal(t, http.StatusOK, status, resp.Message)
		status, _ = env.post(t, "/api/v1/admin/auth/"+resp.Info["uuid"].(string), map[string]string{"otp": "123456", "totp": "123456"}, "")
		assert.Equal(t, http.StatusUnauthorized, status)
	})
}
