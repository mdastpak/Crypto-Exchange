package notify

import (
	"context"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/metrics"
)

// reportSchedule is when a message sent is checked, after it was sent. A
// code lives minutes, so the first checks come fast; a message still on
// its way after the last is let go.
var reportSchedule = []time.Duration{
	5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute,
}

// maxTracked bounds the messages that one instance tracks; beyond it, new
// messages are sent without a check.
const maxTracked = 20000

// trackedSMS is a message sent and not yet reported final. It holds the
// message, its code included, in this process only, until it is: the code
// never reaches a store.
type trackedSMS struct {
	msg      OTPMessage
	provider SMSProvider
	id       string
	sentAt   time.Time
	// deadline is when the code expires: no message is sent again after
	// it.
	deadline time.Time
	// tried are the providers that sent the message.
	tried []string
	check int
}

func (t *trackedSMS) due(now time.Time) bool {
	return !now.Before(t.sentAt.Add(reportSchedule[t.check]))
}

// smsTracker checks the delivery of the messages that this instance sent,
// and sends one that failed again through the next provider. Each instance
// tracks its own messages; one that stops lets them go.
type smsTracker struct {
	router *SMSRouter
	tick   time.Duration

	mu    sync.Mutex
	items []*trackedSMS

	once     sync.Once
	stopping chan struct{}
	done     chan struct{}
}

func newSMSTracker(r *SMSRouter) *smsTracker {
	return &smsTracker{router: r, tick: time.Second, stopping: make(chan struct{}), done: make(chan struct{})}
}

// add tracks a message that p accepted as id. ctx carries the deadline of
// its code.
func (t *smsTracker) add(ctx context.Context, msg OTPMessage, p SMSProvider, id string, tried []string) {
	if id == "" {
		return
	}
	now := t.router.now()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = now.Add(reportSchedule[len(reportSchedule)-1])
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.items) >= maxTracked {
		return
	}
	t.items = append(t.items, &trackedSMS{msg: msg, provider: p, id: id, sentAt: now, deadline: deadline, tried: tried})
}

func (t *smsTracker) start() {
	go func() {
		defer close(t.done)
		ticker := time.NewTicker(t.tick)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				t.checkDue()
			case <-t.stopping:
				return
			}
		}
	}()
}

func (t *smsTracker) stop(ctx context.Context) error {
	t.once.Do(func() { close(t.stopping) })
	select {
	case <-t.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// checkDue reads the reports of the messages that are due, a batch per
// provider, and acts on them.
func (t *smsTracker) checkDue() {
	now := t.router.now()
	t.mu.Lock()
	byProvider := map[SMSProvider][]*trackedSMS{}
	rest := t.items[:0]
	for _, item := range t.items {
		if item.due(now) {
			byProvider[item.provider] = append(byProvider[item.provider], item)
		} else {
			rest = append(rest, item)
		}
	}
	t.items = rest
	t.mu.Unlock()

	var keep []*trackedSMS
	for p, items := range byProvider {
		ids := make([]string, len(items))
		for i, item := range items {
			ids[i] = item.id
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		states, err := p.Reports(ctx, ids)
		cancel()
		if err != nil {
			log.Printf("Failed to read the delivery reports of %s; the next check tries again: %v", p.Name(), err)
		}
		for _, item := range items {
			state, ok := states[item.id]
			if err != nil || !ok {
				state = StatePending
			}
			if t.settle(item, state, now) {
				continue
			}
			item.check++
			if item.check < len(reportSchedule) {
				keep = append(keep, item)
				continue
			}
			// Still on its way after the last check: let it go.
			metrics.SMSDeliveryReports.WithLabelValues(p.Name(), "pending").Inc()
		}
	}
	if len(keep) > 0 {
		t.mu.Lock()
		t.items = append(t.items, keep...)
		t.mu.Unlock()
	}
}

// settle acts on a final state of item and reports whether it was final.
func (t *smsTracker) settle(item *trackedSMS, state DeliveryState, now time.Time) bool {
	name := item.provider.Name()
	ctx := context.Background()
	switch state {
	case StatePending:
		return false
	case StateDelivered:
		t.router.healthStore().Record(ctx, name, SMSDelivered)
	case StateFailed:
		t.router.healthStore().Record(ctx, name, SMSUndelivered)
		t.resend(item, now)
	}
	metrics.SMSDeliveryReports.WithLabelValues(name, state.String()).Inc()
	return true
}

// resend sends a message that failed again, once, through the next
// provider of the effective order that has not sent it, while its code is
// valid.
func (t *smsTracker) resend(item *trackedSMS, now time.Time) {
	if !t.router.opts.ResendFailed || len(item.tried) > 1 || !now.Before(item.deadline) {
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), item.deadline)
	defer cancel()
	order := slices.DeleteFunc(t.router.Order(ctx, item.msg.Purpose), func(p SMSProvider) bool {
		return slices.Contains(item.tried, p.Name())
	})
	if len(order) == 0 {
		return
	}
	p, err := t.router.send(ctx, item.msg, order, item.tried)
	if err != nil {
		log.Printf("Failed to send again, through another provider, a message to %s that %s did not deliver: %s",
			item.msg.MaskedContact, item.provider.Name(), redact(err, item.msg))
		return
	}
	metrics.SMSFallbacks.WithLabelValues(item.provider.Name(), p.Name(), "delivery").Inc()
	log.Printf("Sent again through %s a message to %s that %s did not deliver", p.Name(), item.msg.MaskedContact, item.provider.Name())
}
