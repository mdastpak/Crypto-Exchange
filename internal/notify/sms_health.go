package notify

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// HealthWindow is the time that the health of an SMS provider covers.
const HealthWindow = 15 * time.Minute

// SMSEvent is what happened to a message of an SMS provider.
type SMSEvent int

// The events of an SMS provider.
const (
	// SMSAccepted: the provider accepted a message.
	SMSAccepted SMSEvent = iota
	// SMSProviderFailed: the provider failed to take a message.
	SMSProviderFailed
	// SMSDelivered: a message reached the phone.
	SMSDelivered
	// SMSUndelivered: a message did not reach the phone.
	SMSUndelivered
)

var smsEventFields = map[SMSEvent]string{
	SMSAccepted: "accepted", SMSProviderFailed: "provider_errors", SMSDelivered: "delivered", SMSUndelivered: "failed",
}

// ProviderHealth is what an SMS provider did in HealthWindow.
type ProviderHealth struct {
	// Accepted is the messages it accepted, and ProviderErrors the ones it
	// failed to take.
	Accepted       int64 `json:"accepted"`
	ProviderErrors int64 `json:"provider_errors"`
	// Delivered and Failed are the delivery reports that reached a final
	// state.
	Delivered int64 `json:"delivered"`
	Failed    int64 `json:"failed"`
	// BreakerOpenUntil is when the provider is tried again in auto mode,
	// after failing breaker_failures times in a row; absent when it is
	// closed.
	BreakerOpenUntil *time.Time `json:"breaker_open_until,omitempty"`
}

// SMSBreaker configures the breaker of each provider: Failures failures in
// a row open it for Open. 3 and one minute by default.
type SMSBreaker struct {
	Failures int
	Open     time.Duration
}

func (b SMSBreaker) withDefaults() SMSBreaker {
	if b.Failures <= 0 {
		b.Failures = 3
	}
	if b.Open <= 0 {
		b.Open = time.Minute
	}
	return b
}

// SMSHealth keeps the health of the SMS providers. RedisSMSHealth shares it
// between the instances; MemorySMSHealth keeps it in one.
type SMSHealth interface {
	// Record records an event of provider. A failure counts towards its
	// breaker, and an accepted message closes it.
	Record(ctx context.Context, provider string, e SMSEvent)
	// Snapshot returns the health of providers in HealthWindow.
	Snapshot(ctx context.Context, providers []string) map[string]ProviderHealth
}

// MemorySMSHealth keeps the health in this process, in minute buckets.
type MemorySMSHealth struct {
	breaker SMSBreaker
	now     func() time.Time

	mu       sync.Mutex
	buckets  map[string]map[int64]*ProviderHealth // provider, minute
	failures map[string]int
	openTill map[string]time.Time
}

// NewMemorySMSHealth returns a health store in this process.
func NewMemorySMSHealth(breaker SMSBreaker) *MemorySMSHealth {
	return &MemorySMSHealth{breaker: breaker.withDefaults(), now: time.Now,
		buckets: map[string]map[int64]*ProviderHealth{}, failures: map[string]int{}, openTill: map[string]time.Time{}}
}

func (m *MemorySMSHealth) Record(_ context.Context, provider string, e SMSEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	minute := now.Unix() / 60
	byMinute := m.buckets[provider]
	if byMinute == nil {
		byMinute = map[int64]*ProviderHealth{}
		m.buckets[provider] = byMinute
	}
	for old := range byMinute {
		if old <= minute-int64(HealthWindow/time.Minute) {
			delete(byMinute, old)
		}
	}
	b := byMinute[minute]
	if b == nil {
		b = &ProviderHealth{}
		byMinute[minute] = b
	}
	switch e {
	case SMSAccepted:
		b.Accepted++
		m.failures[provider] = 0
		delete(m.openTill, provider)
	case SMSProviderFailed:
		b.ProviderErrors++
		m.failures[provider]++
		if m.failures[provider] >= m.breaker.Failures {
			m.openTill[provider] = now.Add(m.breaker.Open)
			m.failures[provider] = 0
		}
	case SMSDelivered:
		b.Delivered++
	case SMSUndelivered:
		b.Failed++
	}
}

func (m *MemorySMSHealth) Snapshot(_ context.Context, providers []string) map[string]ProviderHealth {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	minute := now.Unix() / 60
	out := map[string]ProviderHealth{}
	for _, p := range providers {
		var h ProviderHealth
		for at, b := range m.buckets[p] {
			if at > minute-int64(HealthWindow/time.Minute) {
				h.Accepted += b.Accepted
				h.ProviderErrors += b.ProviderErrors
				h.Delivered += b.Delivered
				h.Failed += b.Failed
			}
		}
		if until, ok := m.openTill[p]; ok && now.Before(until) {
			u := until.UTC()
			h.BreakerOpenUntil = &u
		}
		out[p] = h
	}
	return out
}

// RedisSMSHealth keeps the health in Redis, which every instance shares, so
// that a provider that fails on one instance is skipped on all. While
// Redis is away, each instance keeps its own in memory.
type RedisSMSHealth struct {
	client   redis.UniversalClient
	breaker  SMSBreaker
	fallback *MemorySMSHealth
	now      func() time.Time
	// Prefix starts every key: "notify:sms:" by default.
	Prefix string
}

// NewRedisSMSHealth returns a health store in Redis.
func NewRedisSMSHealth(client redis.UniversalClient, breaker SMSBreaker) *RedisSMSHealth {
	breaker = breaker.withDefaults()
	return &RedisSMSHealth{client: client, breaker: breaker, fallback: NewMemorySMSHealth(breaker), now: time.Now, Prefix: "notify:sms:"}
}

func (r *RedisSMSHealth) bucketKey(provider string, minute int64) string {
	return r.Prefix + "health:" + provider + ":" + strconv.FormatInt(minute, 10)
}

func (r *RedisSMSHealth) Record(ctx context.Context, provider string, e SMSEvent) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	now := r.now()
	key := r.bucketKey(provider, now.Unix()/60)
	failures, open := r.Prefix+"breaker:"+provider+":failures", r.Prefix+"breaker:"+provider+":open"
	_, err := r.client.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HIncrBy(ctx, key, smsEventFields[e], 1)
		p.Expire(ctx, key, HealthWindow+5*time.Minute)
		switch e {
		case SMSAccepted:
			p.Del(ctx, failures, open)
		case SMSProviderFailed:
			p.Incr(ctx, failures)
			p.Expire(ctx, failures, 10*time.Minute)
		}
		return nil
	})
	if err == nil && e == SMSProviderFailed {
		var n int64
		if n, err = r.client.Get(ctx, failures).Int64(); err == nil && n >= int64(r.breaker.Failures) {
			_, err = r.client.TxPipelined(ctx, func(p redis.Pipeliner) error {
				p.Set(ctx, open, now.Add(r.breaker.Open).Unix(), r.breaker.Open)
				p.Del(ctx, failures)
				return nil
			})
		}
	}
	if err != nil {
		r.fallback.Record(ctx, provider, e)
	}
}

func (r *RedisSMSHealth) Snapshot(ctx context.Context, providers []string) map[string]ProviderHealth {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	minute := r.now().Unix() / 60
	window := int64(HealthWindow / time.Minute)
	buckets := map[string][]*redis.MapStringStringCmd{}
	opens := map[string]*redis.StringCmd{}
	_, err := r.client.Pipelined(ctx, func(p redis.Pipeliner) error {
		for _, name := range providers {
			for m := minute - window + 1; m <= minute; m++ {
				buckets[name] = append(buckets[name], p.HGetAll(ctx, r.bucketKey(name, m)))
			}
			opens[name] = p.Get(ctx, r.Prefix+"breaker:"+name+":open")
		}
		return nil
	})
	if err != nil && err != redis.Nil {
		return r.fallback.Snapshot(ctx, providers)
	}
	out := map[string]ProviderHealth{}
	for _, name := range providers {
		var h ProviderHealth
		for _, cmd := range buckets[name] {
			fields := cmd.Val()
			for _, add := range []struct {
				field string
				into  *int64
			}{{"accepted", &h.Accepted}, {"provider_errors", &h.ProviderErrors}, {"delivered", &h.Delivered}, {"failed", &h.Failed}} {
				if n, err := strconv.ParseInt(fields[add.field], 10, 64); err == nil {
					*add.into += n
				}
			}
		}
		if until, err := opens[name].Int64(); err == nil {
			u := time.Unix(until, 0).UTC()
			h.BreakerOpenUntil = &u
		}
		out[name] = h
	}
	return out
}
