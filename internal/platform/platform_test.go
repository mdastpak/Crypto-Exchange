package platform

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func feature(f Feature) *Feature { return &f }

func control(kind Kind, f Feature, start time.Time, end *time.Time) *Control {
	c := &Control{HK: "c", Kind: kind, StartsAt: start, EndsAt: end, AnnounceAt: start}
	if f != "" {
		c.Feature = feature(f)
	}
	return c
}

func at(t time.Time) *time.Time { return &t }

func TestCovers(t *testing.T) {
	cases := []struct {
		kind    Kind
		feature Feature
		use     Feature
		want    bool
	}{
		{KindMaintenance, "", UseService, true},
		{KindMaintenance, "", FeatureWithdrawals, true},
		{KindMaintenance, "", FeatureScheduler, false},
		{KindMaintenance, "", FeatureOutbox, false},
		{KindLockdown, FeatureWithdrawals, FeatureWithdrawals, true},
		{KindLockdown, FeatureWithdrawals, FeatureDeposits, false},
		{KindLockdown, FeatureWithdrawals, UseService, false},
		{KindLockdown, FeatureReadOnly, UseWrites, true},
		{KindLockdown, FeatureReadOnly, FeatureTrading, true},
		{KindLockdown, FeatureReadOnly, FeatureSignUp, true},
		{KindLockdown, FeatureReadOnly, FeatureSignIn, false},
		{KindLockdown, FeatureReadOnly, UseService, false},
		{KindLockdown, FeatureScheduler, FeatureScheduler, true},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, covers(c.kind, c.feature, c.use), "%s %s closes %s", c.kind, c.feature, c.use)
	}
}

func TestEvaluate(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)

	t.Run("a control applies from its start until its end or lift", func(t *testing.T) {
		ended := control(KindLockdown, FeatureTrading, past, at(now))
		scheduled := control(KindLockdown, FeatureTrading, future, nil)
		lifted := control(KindLockdown, FeatureTrading, past, nil)
		lifted.LiftedAt = at(past)
		assert.Nil(t, evaluate([]*Control{ended, scheduled, lifted}, now, FeatureTrading, Subject{}))
		active := control(KindLockdown, FeatureTrading, now, nil)
		l := evaluate([]*Control{active}, now, FeatureTrading, Subject{})
		require.NotNil(t, l)
		assert.Equal(t, CodeFeatureLocked, l.Code())
		assert.Equal(t, FeatureTrading, l.Feature)
	})

	t.Run("a maintenance comes first and names the use", func(t *testing.T) {
		lock := control(KindLockdown, FeatureWithdrawals, past, at(future.Add(time.Hour)))
		maint := control(KindMaintenance, "", past, at(future))
		maint.Message = Message{EN: "Upgrading"}
		l := evaluate([]*Control{lock, maint}, now, FeatureWithdrawals, Subject{})
		require.NotNil(t, l)
		assert.Equal(t, CodeMaintenance, l.Code())
		assert.Equal(t, FeatureWithdrawals, l.Feature)
		assert.Equal(t, future, *l.EndsAt)
		assert.Equal(t, "Upgrading", l.Message.EN)
	})

	t.Run("the refusal ends with the last control, or never", func(t *testing.T) {
		a := control(KindLockdown, FeatureTrading, past, at(future))
		b := control(KindLockdown, FeatureReadOnly, past, at(future.Add(time.Hour)))
		l := evaluate([]*Control{a, b}, now, FeatureTrading, Subject{})
		require.NotNil(t, l)
		assert.Equal(t, future.Add(time.Hour), *l.EndsAt)
		c := control(KindLockdown, FeatureTrading, past, nil)
		l = evaluate([]*Control{a, b, c}, now, FeatureTrading, Subject{})
		require.NotNil(t, l)
		assert.Nil(t, l.EndsAt)
	})

	t.Run("a maintenance lets its allowed networks and users through", func(t *testing.T) {
		maint := control(KindMaintenance, "", past, nil)
		maint.AllowNetworks = []string{"203.0.113.0/24", "2001:db8::/32"}
		maint.AllowUsers = []string{"0f1e2d3c-0000-4000-8000-000000000001"}
		require.NoError(t, maint.parseNetworks())
		controls := []*Control{maint}
		assert.Nil(t, evaluate(controls, now, UseService, Subject{IP: "203.0.113.9"}))
		assert.Nil(t, evaluate(controls, now, UseService, Subject{IP: "2001:db8::1"}))
		assert.Nil(t, evaluate(controls, now, UseService, Subject{UserHK: "0f1e2d3c-0000-4000-8000-000000000001", IP: "198.51.100.1"}))
		assert.NotNil(t, evaluate(controls, now, UseService, Subject{IP: "198.51.100.1"}))
		assert.NotNil(t, evaluate(controls, now, UseService, Subject{IP: "not an address"}))
	})
}

func TestLocked_RetryAfter(t *testing.T) {
	now := time.Now()
	assert.Zero(t, (&Locked{}).RetryAfter(now), "no end, no Retry-After")
	assert.Equal(t, time.Second, (&Locked{EndsAt: at(now.Add(-time.Second))}).RetryAfter(now))
	assert.Equal(t, 91*time.Second, (&Locked{EndsAt: at(now.Add(90*time.Second + 300*time.Millisecond))}).RetryAfter(now))
}

func TestStatusOf(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	maint := control(KindMaintenance, "", now.Add(-time.Minute), at(now.Add(time.Hour)))
	withdrawals := control(KindLockdown, FeatureWithdrawals, now.Add(-time.Minute), nil)
	scheduler := control(KindLockdown, FeatureScheduler, now.Add(-time.Minute), nil)
	announced := control(KindLockdown, FeatureTrading, now.Add(time.Hour), nil)
	announced.AnnounceAt = now.Add(-time.Minute)
	secret := control(KindLockdown, FeatureDeposits, now.Add(2*time.Hour), nil)
	secret.AnnounceAt = now.Add(time.Hour)

	s := StatusOf([]*Control{maint, withdrawals, scheduler, announced, secret}, now)
	assert.Equal(t, ModeMaintenance, s.Mode)
	assert.Equal(t, []Feature{FeatureWithdrawals}, s.Lockdowns, "the scheduler lockdown is internal")
	assert.Len(t, s.Active, 2)
	require.Len(t, s.Scheduled, 1, "a control is shown from its announcement")
	assert.Equal(t, FeatureTrading, *s.Scheduled[0].Feature)

	empty := StatusOf(nil, now)
	assert.Equal(t, ModeOpen, empty.Mode)
	assert.NotNil(t, empty.Lockdowns)
	assert.NotNil(t, empty.Active)
	assert.NotNil(t, empty.Scheduled)
}

func TestNewValidate(t *testing.T) {
	now := time.Now()
	t.Run("valid controls are normalized", func(t *testing.T) {
		n := New{Kind: KindMaintenance, Reason: "  Upgrade  ", StartsAt: at(now.Add(-10 * time.Second)),
			AllowNetworks: []string{"203.0.113.7", "10.1.2.3/8"}, Message: Message{EN: " Soon "}}
		require.NoError(t, n.Validate(now))
		assert.Nil(t, n.StartsAt, "a start in the recent past is now")
		assert.Equal(t, "Upgrade", n.Reason)
		assert.Equal(t, "Soon", n.Message.EN)
		assert.Equal(t, []string{"203.0.113.7/32", "10.0.0.0/8"}, n.AllowNetworks)
	})
	t.Run("invalid controls report every field", func(t *testing.T) {
		long := make([]rune, MaxMessage+1)
		for i := range long {
			long[i] = 'x'
		}
		n := New{Kind: KindLockdown, Feature: "everything", StartsAt: at(now.Add(-time.Hour)), EndsAt: at(now.Add(-2 * time.Hour)),
			Message: Message{FA: string(long)}, AllowUsers: []string{"x"}}
		err := n.Validate(now)
		var invalid Invalid
		require.ErrorAs(t, err, &invalid)
		for _, field := range []string{"feature", "starts_at", "ends_at", "message.fa", "reason", "allow", "allow_users"} {
			assert.Contains(t, invalid, field)
		}
	})
	t.Run("a maintenance takes no feature, a lockdown no allow list", func(t *testing.T) {
		n := New{Kind: KindMaintenance, Feature: FeatureTrading, Reason: "r"}
		assert.ErrorAs(t, n.Validate(now), new(Invalid))
		n = New{Kind: KindLockdown, Feature: FeatureTrading, Reason: "r", AllowNetworks: []string{"10.0.0.0/8"}}
		assert.ErrorAs(t, n.Validate(now), new(Invalid))
		n = New{Kind: "pause", Reason: "r"}
		assert.ErrorAs(t, n.Validate(now), new(Invalid))
	})
	t.Run("an announcement comes before the start", func(t *testing.T) {
		start := now.Add(time.Hour)
		n := New{Kind: KindLockdown, Feature: FeatureTrading, Reason: "r", StartsAt: &start, AnnounceAt: at(start.Add(time.Minute))}
		assert.ErrorAs(t, n.Validate(now), new(Invalid))
	})
}

func TestParseFeatures(t *testing.T) {
	got, err := parseFeatures("k", nil, DefaultFailClosed)
	require.NoError(t, err)
	assert.Equal(t, DefaultFailClosed, got)
	got, err = parseFeatures("k", []string{"none"}, DefaultFailClosed)
	require.NoError(t, err)
	assert.Empty(t, got)
	got, err = parseFeatures("k", []string{"withdrawals", "sign_up"}, DefaultFailClosed)
	require.NoError(t, err)
	assert.Equal(t, []Feature{FeatureWithdrawals, FeatureSignUp}, got)
	_, err = parseFeatures("platform.fail_closed", []string{"withdrawls"}, nil)
	assert.ErrorContains(t, err, `platform.fail_closed: "withdrawls" is not a feature`)
}

// staticWatcher returns a watcher that holds controls, read now.
func staticWatcher(controls ...*Control) *Watcher {
	w := NewWatcher(nil, Options{})
	w.controls, w.loadedAt = controls, time.Now()
	return w
}

func TestGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	serve := func(g *Gate, method string, handlers ...gin.HandlerFunc) *httptest.ResponseRecorder {
		r := gin.New()
		r.Handle(method, "/x", append(handlers, func(c *gin.Context) { c.Status(http.StatusNoContent) })...)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, "/x", nil))
		return w
	}

	t.Run("a nil gate lets everything through", func(t *testing.T) {
		var g *Gate
		assert.Equal(t, http.StatusNoContent, serve(g, http.MethodPost, g.Require(UseService), g.Writes()).Code)
		assert.Nil(t, g.Locked(UseService, Subject{}))
	})

	t.Run("a maintenance answers 503 with Retry-After", func(t *testing.T) {
		g := &Gate{Watcher: staticWatcher(control(KindMaintenance, "", time.Now().Add(-time.Minute), at(time.Now().Add(2*time.Minute))))}
		w := serve(g, http.MethodGet, g.Require(UseService))
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Contains(t, w.Body.String(), `"code":"maintenance"`)
		assert.NotEmpty(t, w.Header().Get("Retry-After"))
	})

	t.Run("read-only closes writes but not reads", func(t *testing.T) {
		g := &Gate{Watcher: staticWatcher(control(KindLockdown, FeatureReadOnly, time.Now().Add(-time.Minute), nil))}
		assert.Equal(t, http.StatusNoContent, serve(g, http.MethodGet, g.Writes()).Code)
		w := serve(g, http.MethodDelete, g.Writes())
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Contains(t, w.Body.String(), `"code":"feature_locked"`)
		assert.Empty(t, w.Header().Get("Retry-After"), "a lockdown without an end has no Retry-After")
	})

	t.Run("unknown controls close the fail-closed features only", func(t *testing.T) {
		w := NewWatcher(nil, Options{FailClosed: []Feature{FeatureWithdrawals}})
		g := &Gate{Watcher: w}
		l := g.Locked(FeatureWithdrawals, Subject{})
		require.NotNil(t, l)
		assert.True(t, l.StateUnknown)
		assert.Nil(t, g.Locked(FeatureTrading, Subject{}))
		assert.Nil(t, g.Locked(UseService, Subject{}))
		assert.False(t, w.Paused(FeatureScheduler), "unknown controls pause no background work")
		resp := serve(g, http.MethodPost, g.Require(FeatureWithdrawals))
		assert.Equal(t, http.StatusServiceUnavailable, resp.Code)
		assert.Contains(t, resp.Body.String(), `"state_unknown":true`)
		assert.Equal(t, "5", resp.Header().Get("Retry-After"))
	})
}
