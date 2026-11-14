package notify

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/metrics"
)

// SMSProvider sends messages through one SMS gateway, such as Kavenegar or
// sms.ir, and reports whether they reached the phone (#371).
type SMSProvider interface {
	// Name is the provider's name in the configuration.
	Name() string
	// Supports reports whether the provider has a template for messages of
	// purpose: "" for sign-in codes, PurposeWithdrawal, or
	// PurposeAntiPhishingReminder.
	Supports(purpose string) bool
	// Send sends msg and returns the provider's ID of the message. A
	// failure of the provider, which another provider may not have, is a
	// *ProviderError; a message that no provider can send, such as one to
	// an invalid number, is permanent.
	Send(ctx context.Context, msg OTPMessage) (string, error)
	// Reports returns the delivery state of the messages with ids. An ID
	// that the provider does not report is StateUnknown.
	Reports(ctx context.Context, ids []string) (map[string]DeliveryState, error)
}

// DeliveryState is where an SMS is, by the report of its provider.
type DeliveryState int

// The delivery states.
const (
	// StatePending is a message on its way: queued, or with the carrier.
	StatePending DeliveryState = iota
	// StateDelivered reached the phone.
	StateDelivered
	// StateFailed did not reach the phone: refused by the carrier, the
	// phone off past the carrier's retries, or blocked.
	StateFailed
	// StateUnknown has no report: too old, an unknown ID, or a sandbox
	// message that the provider does not record.
	StateUnknown
)

func (s DeliveryState) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateDelivered:
		return "delivered"
	case StateFailed:
		return "failed"
	}
	return "unknown"
}

// ProviderError is a failure of an SMS provider rather than of the message:
// the provider is down, slow, out of credit, or refuses its key or
// template. The next provider may send the message.
type ProviderError struct {
	Provider string
	Err      error
}

func (e *ProviderError) Error() string { return e.Err.Error() }

func (e *ProviderError) Unwrap() error { return e.Err }

// providerError marks err as a failure of the provider name.
func providerError(name string, err error) error {
	return &ProviderError{Provider: name, Err: err}
}

// IsProviderError reports whether err is a failure of an SMS provider.
func IsProviderError(err error) bool {
	var p *ProviderError
	return errors.As(err, &p)
}

// The purposes of SMS, by the names that the status and the configuration
// use.
const (
	SMSPurposeSignIn     = "sign_in"
	SMSPurposeWithdrawal = "withdrawal"
	SMSPurposeReminder   = "anti_phishing_reminder"
)

// smsPurposes are the purposes of messages, by their names.
var smsPurposes = map[string]string{
	SMSPurposeSignIn: "", SMSPurposeWithdrawal: PurposeWithdrawal, SMSPurposeReminder: PurposeAntiPhishingReminder,
}

// SMSPurposeName is the name of the purpose of a message.
func SMSPurposeName(purpose string) string {
	for name, p := range smsPurposes {
		if p == purpose {
			return name
		}
	}
	return purpose
}

// The modes of SMS routing.
const (
	// SMSRoutingAuto sends through the configured order, with the
	// providers that fail or do not deliver moved behind the others.
	SMSRoutingAuto = "auto"
	// SMSRoutingManual sends through the order an administrator set.
	SMSRoutingManual = "manual"
)

// SMSRouting is how SMS is routed: by the configured order with health
// (auto), or by an administrator's order (manual).
type SMSRouting struct {
	Mode  string
	Order []string
}

// SMSRoutingSource reads the routing that administrators set; every
// instance reads the same.
type SMSRoutingSource interface {
	SMSRouting(ctx context.Context) (SMSRouting, error)
}

// SMSRouterOptions configures an SMSRouter. Zero values select the
// defaults.
type SMSRouterOptions struct {
	// Routing is the administrators' routing; nil is always auto.
	Routing SMSRoutingSource
	// Health keeps the health of the providers; nil keeps it in this
	// process.
	Health SMSHealth
	// MinDeliveryPercent and MinSamples: in auto mode, a provider whose
	// share of delivered messages, of at least MinSamples final reports
	// in HealthWindow, is below MinDeliveryPercent goes behind the others.
	// 50 and 20 by default; a negative MinDeliveryPercent turns it off.
	MinDeliveryPercent int
	MinSamples         int
	// Reports turns on the delivery reports: each message sent is checked
	// until it reaches the phone or fails.
	Reports bool
	// ResendFailed sends a message that failed again, once, through the
	// next provider, while its code is valid.
	ResendFailed bool
}

// routingCacheTTL is how long an instance keeps the routing and the
// health it read, so that a message does not wait for the database.
const routingCacheTTL = 5 * time.Second

// SMSRouter delivers SMS through several providers (#371). It tries them in
// the effective order and moves on to the next when one fails, but never
// when the message itself is refused. It is the SMS Deliverer of Channels.
type SMSRouter struct {
	providers []SMSProvider
	opts      SMSRouterOptions
	health    SMSHealth
	tracker   *smsTracker
	now       func() time.Time

	mu        sync.Mutex
	routing   SMSRouting
	routingAt time.Time
	snapshot  map[string]ProviderHealth
	snapAt    time.Time
}

// NewSMSRouter returns a router over providers, in their configured order.
func NewSMSRouter(providers []SMSProvider, opts SMSRouterOptions) *SMSRouter {
	if opts.MinDeliveryPercent == 0 {
		opts.MinDeliveryPercent = 50
	}
	if opts.MinSamples <= 0 {
		opts.MinSamples = 20
	}
	r := &SMSRouter{providers: providers, opts: opts, health: opts.Health, now: time.Now,
		routing: SMSRouting{Mode: SMSRoutingAuto}}
	if r.health == nil {
		r.health = NewMemorySMSHealth(SMSBreaker{})
	}
	if opts.Reports {
		r.tracker = newSMSTracker(r)
	}
	return r
}

// UseHealth keeps the health in h, such as Redis shared by the instances.
// It is called before the router delivers.
func (r *SMSRouter) UseHealth(h SMSHealth) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health, r.snapshot = h, nil
}

// UseRouting reads the administrators' routing from source. It is called
// before the router delivers.
func (r *SMSRouter) UseRouting(source SMSRoutingSource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opts.Routing, r.routingAt = source, time.Time{}
}

// Names returns the names of the configured providers, in their order.
func (r *SMSRouter) Names() []string {
	names := make([]string, len(r.providers))
	for i, p := range r.providers {
		names[i] = p.Name()
	}
	return names
}

// Supports reports whether a configured provider has a template for
// purpose.
func (r *SMSRouter) Supports(purpose string) bool {
	return slices.ContainsFunc(r.providers, func(p SMSProvider) bool { return p.Supports(purpose) })
}

func (r *SMSRouter) provider(name string) SMSProvider {
	for _, p := range r.providers {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

// currentRouting returns the routing, read at most every routingCacheTTL.
// When it cannot be read, the last one read stays.
func (r *SMSRouter) currentRouting(ctx context.Context) SMSRouting {
	r.mu.Lock()
	cached, source := r.routing, r.opts.Routing
	if source == nil || r.now().Sub(r.routingAt) < routingCacheTTL {
		r.mu.Unlock()
		return cached
	}
	// The others go on with the cached routing while this one reads, so
	// that a slow database holds up no message.
	r.routingAt = r.now()
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	routing, err := source.SMSRouting(ctx)
	if err != nil {
		return cached
	}
	r.mu.Lock()
	r.routing = routing
	r.mu.Unlock()
	return routing
}

// Refresh drops the cached routing, so that a change applies at once on
// this instance; the others apply it within routingCacheTTL.
func (r *SMSRouter) Refresh() {
	r.mu.Lock()
	r.routingAt, r.snapAt = time.Time{}, time.Time{}
	r.mu.Unlock()
}

// healthSnapshot returns the health of the providers, read at most every
// routingCacheTTL.
func (r *SMSRouter) healthSnapshot(ctx context.Context) map[string]ProviderHealth {
	r.mu.Lock()
	cached, health := r.snapshot, r.health
	if cached != nil && r.now().Sub(r.snapAt) < routingCacheTTL {
		r.mu.Unlock()
		return cached
	}
	r.snapAt = r.now()
	r.mu.Unlock()
	snapshot := health.Snapshot(ctx, r.Names())
	r.mu.Lock()
	r.snapshot = snapshot
	r.mu.Unlock()
	return snapshot
}

// healthy reports whether, in auto mode, a provider keeps its place: its
// breaker is closed, and its delivery rate is good enough or unknown.
func (r *SMSRouter) healthy(h ProviderHealth, now time.Time) bool {
	if h.BreakerOpenUntil != nil && now.Before(*h.BreakerOpenUntil) {
		return false
	}
	final := h.Delivered + h.Failed
	if r.opts.MinDeliveryPercent < 0 || final < int64(r.opts.MinSamples) {
		return true
	}
	return h.Delivered*100 >= int64(r.opts.MinDeliveryPercent)*final
}

// Order returns the providers that messages of purpose go through, in
// order: the administrators' order in manual mode, or else the configured
// order with the unhealthy providers last. A provider without a template
// for purpose is left out.
func (r *SMSRouter) Order(ctx context.Context, purpose string) []SMSProvider {
	routing := r.currentRouting(ctx)
	var candidates []SMSProvider
	if routing.Mode == SMSRoutingManual && len(routing.Order) > 0 {
		for _, name := range routing.Order {
			if p := r.provider(name); p != nil {
				candidates = append(candidates, p)
			}
		}
	} else {
		candidates = slices.Clone(r.providers)
	}
	candidates = slices.DeleteFunc(candidates, func(p SMSProvider) bool { return !p.Supports(purpose) })
	if routing.Mode == SMSRoutingManual || len(candidates) < 2 {
		return candidates
	}
	health, now := r.healthSnapshot(ctx), r.now()
	var healthy, unhealthy []SMSProvider
	for _, p := range candidates {
		if r.healthy(health[p.Name()], now) {
			healthy = append(healthy, p)
		} else {
			unhealthy = append(unhealthy, p)
		}
	}
	return append(healthy, unhealthy...)
}

// Deliver sends msg through the first provider of the effective order that
// accepts it. A failure of a provider moves on to the next; a refusal of
// the message stops. When every provider failed, the error is permanent
// only if each failure was, so that the worker retries the others.
func (r *SMSRouter) Deliver(ctx context.Context, msg OTPMessage) error {
	order := r.Order(ctx, msg.Purpose)
	if len(order) == 0 {
		return permanent(fmt.Errorf("no SMS provider has a template for %s messages", SMSPurposeName(msg.Purpose)))
	}
	_, err := r.send(ctx, msg, order, nil)
	return err
}

// send sends msg through the first of order that accepts it, and tracks
// it. tried are the providers that sent it before, whose names it keeps.
func (r *SMSRouter) send(ctx context.Context, msg OTPMessage, order []SMSProvider, tried []string) (SMSProvider, error) {
	var texts []string
	allPermanent := true
	for i, p := range order {
		id, err := p.Send(ctx, msg)
		if err == nil {
			r.healthStore().Record(ctx, p.Name(), SMSAccepted)
			metrics.SMSSends.WithLabelValues(p.Name(), "accepted").Inc()
			if i > 0 {
				metrics.SMSFallbacks.WithLabelValues(order[0].Name(), p.Name(), "send").Inc()
			}
			if r.tracker != nil {
				r.tracker.add(ctx, msg, p, id, append(slices.Clone(tried), p.Name()))
			}
			return p, nil
		}
		if !IsProviderError(err) {
			// The message itself, or the end of ctx: no other provider
			// would do better.
			metrics.SMSSends.WithLabelValues(p.Name(), "refused").Inc()
			return nil, err
		}
		r.healthStore().Record(ctx, p.Name(), SMSProviderFailed)
		metrics.SMSSends.WithLabelValues(p.Name(), "provider_error").Inc()
		texts = append(texts, err.Error())
		allPermanent = allPermanent && IsPermanent(err)
		if ctx.Err() != nil {
			break
		}
	}
	// A new error of the texts: joined, the errors of the providers would
	// make the whole permanent through any one of them.
	err := fmt.Errorf("every SMS provider failed: %s", strings.Join(texts, "; "))
	if allPermanent && len(texts) == len(order) {
		return nil, permanent(err)
	}
	return nil, err
}

// Start starts the delivery reports, when they are on.
func (r *SMSRouter) Start() {
	if r.tracker != nil {
		r.tracker.start()
	}
}

// Stop ends the delivery reports and waits for a check in progress, until
// ctx ends. The messages still tracked are not checked again: their
// providers accepted them.
func (r *SMSRouter) Stop(ctx context.Context) error {
	if r.tracker == nil {
		return nil
	}
	return r.tracker.stop(ctx)
}

// SMSStatus is how SMS is routed now, for administrators.
type SMSStatus struct {
	// Mode is auto or manual.
	Mode string `json:"mode" enums:"auto,manual" example:"auto"`
	// Order is the administrators' order in manual mode.
	Order []string `json:"order,omitempty" example:"smsir,kavenegar"`
	// Effective is the order that sign-in codes go through now.
	Effective []string `json:"effective_order" example:"kavenegar,smsir"`
	// Providers are the configured providers, in their configured order.
	Providers []SMSProviderStatus `json:"providers"`
	// WindowMinutes is the time that the counts cover.
	WindowMinutes int `json:"window_minutes" example:"15"`
	// Reports says whether the delivery of each message is checked.
	Reports bool `json:"delivery_reports"`
}

// SMSProviderStatus is a provider and its health.
type SMSProviderStatus struct {
	Name string `json:"name" example:"kavenegar"`
	// Templates are the purposes it has a template for.
	Templates []string `json:"templates" example:"sign_in,withdrawal"`
	// Healthy is false while its breaker is open, or its delivery rate is
	// below the bound.
	Healthy bool `json:"healthy"`
	ProviderHealth
}

// Status returns the routing and the health of the providers.
func (r *SMSRouter) Status(ctx context.Context) SMSStatus {
	r.Refresh()
	routing := r.currentRouting(ctx)
	health, now := r.healthSnapshot(ctx), r.now()
	st := SMSStatus{Mode: routing.Mode, WindowMinutes: int(HealthWindow / time.Minute), Reports: r.tracker != nil}
	if routing.Mode == SMSRoutingManual {
		st.Order = routing.Order
	}
	for _, p := range r.Order(ctx, "") {
		st.Effective = append(st.Effective, p.Name())
	}
	for _, p := range r.providers {
		ps := SMSProviderStatus{Name: p.Name(), ProviderHealth: health[p.Name()]}
		for _, name := range []string{SMSPurposeSignIn, SMSPurposeWithdrawal, SMSPurposeReminder} {
			if p.Supports(smsPurposes[name]) {
				ps.Templates = append(ps.Templates, name)
			}
		}
		ps.Healthy = r.healthy(ps.ProviderHealth, now)
		st.Providers = append(st.Providers, ps)
	}
	return st
}

// CheckRouting checks an order that administrators set: known providers,
// each once, and for each purpose that a configured provider has a
// template for, one in the order that has it too, so that no kind of
// message loses SMS.
func (r *SMSRouter) CheckRouting(routing SMSRouting) error {
	switch routing.Mode {
	case SMSRoutingAuto:
		if len(routing.Order) > 0 {
			return errors.New("an order is only for the manual mode")
		}
		return nil
	case SMSRoutingManual:
	default:
		return fmt.Errorf("mode %q is not auto or manual", routing.Mode)
	}
	if len(routing.Order) == 0 {
		return errors.New("the manual mode needs an order of at least one provider")
	}
	seen := map[string]bool{}
	for _, name := range routing.Order {
		if r.provider(name) == nil {
			return fmt.Errorf("%q is not a configured provider; they are %s", name, strings.Join(r.Names(), ", "))
		}
		if seen[name] {
			return fmt.Errorf("%q is in the order twice", name)
		}
		seen[name] = true
	}
	for _, name := range []string{SMSPurposeSignIn, SMSPurposeWithdrawal, SMSPurposeReminder} {
		purpose := smsPurposes[name]
		if r.Supports(purpose) && !slices.ContainsFunc(routing.Order, func(n string) bool { return r.provider(n).Supports(purpose) }) {
			return fmt.Errorf("the order has no provider with a %s template", name)
		}
	}
	return nil
}

// healthStore returns where the health is kept.
func (r *SMSRouter) healthStore() SMSHealth {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.health
}
