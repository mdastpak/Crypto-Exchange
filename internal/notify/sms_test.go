package notify

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/testutil/redistest"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProvider is an SMS provider that answers with fail, and reports the
// states in states.
type fakeProvider struct {
	name      string
	templates map[string]bool // purposes besides sign-in

	mu     sync.Mutex
	fail   error
	states map[string]DeliveryState
	sent   []OTPMessage
	next   int
}

func newFake(name string, purposes ...string) *fakeProvider {
	f := &fakeProvider{name: name, templates: map[string]bool{}, states: map[string]DeliveryState{}}
	for _, p := range purposes {
		f.templates[p] = true
	}
	return f
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Supports(purpose string) bool { return purpose == "" || f.templates[purpose] }

func (f *fakeProvider) Send(_ context.Context, msg OTPMessage) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return "", f.fail
	}
	f.next++
	f.sent = append(f.sent, msg)
	return f.name + "-" + strconv.Itoa(f.next), nil
}

func (f *fakeProvider) Reports(_ context.Context, ids []string) (map[string]DeliveryState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]DeliveryState{}
	for _, id := range ids {
		if s, ok := f.states[id]; ok {
			out[id] = s
		}
	}
	return out, nil
}

func (f *fakeProvider) set(fail error) {
	f.mu.Lock()
	f.fail = fail
	f.mu.Unlock()
}

func (f *fakeProvider) report(id string, s DeliveryState) {
	f.mu.Lock()
	f.states[id] = s
	f.mu.Unlock()
}

func (f *fakeProvider) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

var (
	errDown    = providerError("x", errors.New("connection refused"))
	errCredit  = providerError("x", permanent(errors.New("insufficient credit")))
	errInvalid = permanent(errors.New("invalid recipient"))
)

// staticRouting is an administrators' routing.
type staticRouting struct{ routing SMSRouting }

func (s *staticRouting) SMSRouting(context.Context) (SMSRouting, error) { return s.routing, nil }

func names(ps []SMSProvider) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name()
	}
	return out
}

func TestSMSRouter_FailsOverOnlyOnFailuresOfTheProvider(t *testing.T) {
	ctx := context.Background()
	first, second := newFake("kavenegar"), newFake("smsir")
	r := NewSMSRouter([]SMSProvider{first, second}, SMSRouterOptions{})

	require.NoError(t, r.Deliver(ctx, testSMSMessage))
	assert.Equal(t, 1, first.count())
	assert.Zero(t, second.count())

	first.set(errDown)
	require.NoError(t, r.Deliver(ctx, testSMSMessage), "the second provider sends when the first is down")
	assert.Equal(t, 1, second.count())

	first.set(errInvalid)
	err := r.Deliver(ctx, testSMSMessage)
	require.Error(t, err)
	assert.True(t, IsPermanent(err))
	assert.Equal(t, 1, second.count(), "an invalid number is not tried elsewhere")

	first.set(errDown)
	second.set(errCredit)
	err = r.Deliver(ctx, testSMSMessage)
	require.Error(t, err)
	assert.False(t, IsPermanent(err), "one failure may pass, so the worker retries")
	assert.Contains(t, err.Error(), "connection refused")
	assert.Contains(t, err.Error(), "insufficient credit")

	first.set(errCredit)
	err = r.Deliver(ctx, testSMSMessage)
	require.Error(t, err)
	assert.True(t, IsPermanent(err), "no provider can send now")
}

func TestSMSRouter_PassesOverProvidersWithoutTheTemplate(t *testing.T) {
	ctx := context.Background()
	plain, withdrawals := newFake("kavenegar"), newFake("smsir", PurposeWithdrawal)
	r := NewSMSRouter([]SMSProvider{plain, withdrawals}, SMSRouterOptions{})
	msg := testSMSMessage
	msg.Purpose = PurposeWithdrawal
	require.NoError(t, r.Deliver(ctx, msg))
	assert.Equal(t, 1, withdrawals.count(), "a withdrawal code never goes out with the sign-in template")
	assert.Zero(t, plain.count())

	msg.Purpose = PurposeAntiPhishingReminder
	err := r.Deliver(ctx, msg)
	require.Error(t, err)
	assert.True(t, IsPermanent(err))
	assert.True(t, r.Supports(PurposeWithdrawal))
	assert.False(t, r.Supports(PurposeAntiPhishingReminder))
}

func TestSMSRouter_AutoModePutsUnhealthyProvidersLast(t *testing.T) {
	ctx := context.Background()
	first, second := newFake("kavenegar"), newFake("smsir")
	health := NewMemorySMSHealth(SMSBreaker{Failures: 2, Open: time.Minute})
	r := NewSMSRouter([]SMSProvider{first, second}, SMSRouterOptions{Health: health, MinSamples: 4})
	now := time.Now()
	r.now, health.now = func() time.Time { return now }, func() time.Time { return now }

	assert.Equal(t, []string{"kavenegar", "smsir"}, names(r.Order(ctx, "")))
	first.set(errDown)
	for range 2 {
		require.NoError(t, r.Deliver(ctx, testSMSMessage))
	}
	r.Refresh()
	assert.Equal(t, []string{"smsir", "kavenegar"}, names(r.Order(ctx, "")), "its breaker is open")
	st := r.Status(ctx)
	require.NotNil(t, st.Providers[0].BreakerOpenUntil)
	assert.False(t, st.Providers[0].Healthy)
	assert.Equal(t, []string{"smsir", "kavenegar"}, st.Effective)

	// The breaker closes after its time; a poor delivery rate moves the
	// provider back, once there are enough reports.
	now = now.Add(2 * time.Minute)
	first.set(nil)
	r.Refresh()
	assert.Equal(t, []string{"kavenegar", "smsir"}, names(r.Order(ctx, "")))
	for _, e := range []SMSEvent{SMSUndelivered, SMSUndelivered, SMSDelivered} {
		health.Record(ctx, "kavenegar", e)
	}
	r.Refresh()
	assert.Equal(t, []string{"kavenegar", "smsir"}, names(r.Order(ctx, "")), "three reports are too few to judge")
	health.Record(ctx, "kavenegar", SMSUndelivered)
	r.Refresh()
	assert.Equal(t, []string{"smsir", "kavenegar"}, names(r.Order(ctx, "")), "a quarter delivered is below half")
	st = r.Status(ctx)
	assert.Equal(t, int64(1), st.Providers[0].Delivered)
	assert.Equal(t, int64(3), st.Providers[0].Failed)
}

func TestSMSRouter_ManualModeKeepsTheAdministratorsOrder(t *testing.T) {
	ctx := context.Background()
	first, second := newFake("kavenegar"), newFake("smsir")
	routing := &staticRouting{SMSRouting{Mode: SMSRoutingManual, Order: []string{"smsir"}}}
	health := NewMemorySMSHealth(SMSBreaker{Failures: 1})
	r := NewSMSRouter([]SMSProvider{first, second}, SMSRouterOptions{Routing: routing, Health: health})

	require.NoError(t, r.Deliver(ctx, testSMSMessage))
	assert.Equal(t, 1, second.count())
	second.set(errDown)
	require.Error(t, r.Deliver(ctx, testSMSMessage), "a list of one is no failover")
	assert.Zero(t, first.count())

	routing.routing.Order = []string{"smsir", "kavenegar"}
	r.Refresh()
	assert.Equal(t, []string{"smsir", "kavenegar"}, names(r.Order(ctx, "")), "health does not reorder the manual mode")
	require.NoError(t, r.Deliver(ctx, testSMSMessage), "failures still go down the list")
	assert.Equal(t, 1, first.count())

	st := r.Status(ctx)
	assert.Equal(t, SMSRoutingManual, st.Mode)
	assert.Equal(t, []string{"smsir", "kavenegar"}, st.Order)
}

func TestSMSRouter_CheckRouting(t *testing.T) {
	r := NewSMSRouter([]SMSProvider{newFake("kavenegar", PurposeWithdrawal), newFake("smsir")}, SMSRouterOptions{})
	require.NoError(t, r.CheckRouting(SMSRouting{Mode: SMSRoutingAuto}))
	require.NoError(t, r.CheckRouting(SMSRouting{Mode: SMSRoutingManual, Order: []string{"kavenegar"}}))
	require.NoError(t, r.CheckRouting(SMSRouting{Mode: SMSRoutingManual, Order: []string{"smsir", "kavenegar"}}))
	for want, routing := range map[string]SMSRouting{
		"not auto or manual":                     {Mode: "random"},
		"only for the manual mode":               {Mode: SMSRoutingAuto, Order: []string{"smsir"}},
		"at least one provider":                  {Mode: SMSRoutingManual},
		"not a configured provider":              {Mode: SMSRoutingManual, Order: []string{"pigeon"}},
		"twice":                                  {Mode: SMSRoutingManual, Order: []string{"smsir", "smsir"}},
		"no provider with a withdrawal template": {Mode: SMSRoutingManual, Order: []string{"smsir"}},
	} {
		err := r.CheckRouting(routing)
		require.Error(t, err, want)
		assert.Contains(t, err.Error(), want)
	}
}

// track sends msg through r and returns the time it was sent.
func deliverAt(t *testing.T, r *SMSRouter, msg OTPMessage, deadline time.Time) {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	require.NoError(t, r.Deliver(ctx, msg))
}

func TestSMSTracker_SendsAFailedMessageAgainThroughTheNextProvider(t *testing.T) {
	first, second := newFake("kavenegar"), newFake("smsir")
	health := NewMemorySMSHealth(SMSBreaker{})
	r := NewSMSRouter([]SMSProvider{first, second}, SMSRouterOptions{Health: health, Reports: true, ResendFailed: true})
	now := time.Now()
	r.now, health.now = func() time.Time { return now }, func() time.Time { return now }

	deliverAt(t, r, testSMSMessage, now.Add(2*time.Minute))
	r.tracker.checkDue()
	assert.Len(t, r.tracker.items, 1, "not due before five seconds")

	now = now.Add(5 * time.Second)
	r.tracker.checkDue()
	assert.Len(t, r.tracker.items, 1, "no report yet: it stays")

	first.report("kavenegar-1", StateFailed)
	now = now.Add(10 * time.Second)
	r.tracker.checkDue()
	require.Equal(t, 1, second.count(), "the failed message goes again through the next provider")
	assert.Equal(t, "482913", second.sent[0].Code, "with the same code")
	require.Len(t, r.tracker.items, 1, "and the new message is tracked")
	assert.Equal(t, []string{"kavenegar", "smsir"}, r.tracker.items[0].tried)

	second.report("smsir-1", StateFailed)
	now = now.Add(5 * time.Second)
	r.tracker.checkDue()
	assert.Equal(t, 1, first.count(), "only once")
	assert.Empty(t, r.tracker.items)

	h := health.Snapshot(context.Background(), []string{"kavenegar", "smsir"})
	assert.Equal(t, int64(1), h["kavenegar"].Failed)
	assert.Equal(t, int64(1), h["smsir"].Failed)
}

func TestSMSTracker_DeliveredOrExpiredCodesAreNotSentAgain(t *testing.T) {
	first, second := newFake("kavenegar"), newFake("smsir")
	health := NewMemorySMSHealth(SMSBreaker{})
	r := NewSMSRouter([]SMSProvider{first, second}, SMSRouterOptions{Health: health, Reports: true, ResendFailed: true})
	now := time.Now()
	r.now, health.now = func() time.Time { return now }, func() time.Time { return now }

	deliverAt(t, r, testSMSMessage, now.Add(10*time.Second))
	deliverAt(t, r, testSMSMessage, now.Add(time.Minute))
	first.report("kavenegar-1", StateFailed)
	first.report("kavenegar-2", StateDelivered)
	now = now.Add(15 * time.Second)
	r.tracker.checkDue()
	assert.Zero(t, second.count(), "the first code expired, the second arrived")
	assert.Empty(t, r.tracker.items)
	h := health.Snapshot(context.Background(), []string{"kavenegar"})
	assert.Equal(t, int64(1), h["kavenegar"].Delivered)

	// A message still on its way after the last check is let go.
	deliverAt(t, r, testSMSMessage, now.Add(time.Minute))
	for range reportSchedule {
		now = now.Add(5 * time.Minute)
		r.tracker.checkDue()
	}
	assert.Empty(t, r.tracker.items)

	// Without resending, a failure is only counted.
	r.opts.ResendFailed = false
	deliverAt(t, r, testSMSMessage, now.Add(time.Minute))
	first.report("kavenegar-4", StateFailed)
	now = now.Add(5 * time.Second)
	r.tracker.checkDue()
	assert.Zero(t, second.count())
}

func TestRedisSMSHealth_IsSharedByTheInstances(t *testing.T) {
	ctx := context.Background()
	client := redis.NewClient(&redis.Options{Addr: redistest.Addr, DB: redistest.Notify})
	t.Cleanup(func() { client.Close() })
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis is not reachable: %v", err)
	}
	prefix := "test:sms:" + strconv.FormatInt(time.Now().UnixNano(), 10) + ":"
	one, two := NewRedisSMSHealth(client, SMSBreaker{Failures: 2, Open: time.Minute}), NewRedisSMSHealth(client, SMSBreaker{Failures: 2, Open: time.Minute})
	one.Prefix, two.Prefix = prefix, prefix

	one.Record(ctx, "kavenegar", SMSProviderFailed)
	two.Record(ctx, "kavenegar", SMSProviderFailed)
	h := one.Snapshot(ctx, []string{"kavenegar", "smsir"})
	require.NotNil(t, h["kavenegar"].BreakerOpenUntil, "two failures on two instances open the breaker on both")
	assert.Equal(t, int64(2), h["kavenegar"].ProviderErrors)
	assert.Nil(t, h["smsir"].BreakerOpenUntil)

	two.Record(ctx, "kavenegar", SMSAccepted)
	one.Record(ctx, "kavenegar", SMSDelivered)
	h = two.Snapshot(ctx, []string{"kavenegar"})
	assert.Nil(t, h["kavenegar"].BreakerOpenUntil, "an accepted message closes it")
	assert.Equal(t, int64(1), h["kavenegar"].Accepted)
	assert.Equal(t, int64(1), h["kavenegar"].Delivered)

	// Without Redis, each instance keeps its own.
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond})
	t.Cleanup(func() { down.Close() })
	alone := NewRedisSMSHealth(down, SMSBreaker{Failures: 1})
	alone.Record(ctx, "smsir", SMSProviderFailed)
	assert.NotNil(t, alone.Snapshot(ctx, []string{"smsir"})["smsir"].BreakerOpenUntil)
}
