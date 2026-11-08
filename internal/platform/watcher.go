package platform

import (
	"context"
	"database/sql"
	"log"
	"reflect"
	"slices"
	"sync"
	"time"
)

// Defaults of a Watcher.
const (
	DefaultPollInterval = time.Second
	DefaultStaleAfter   = 30 * time.Second
)

// DefaultFailClosed are the features that an instance closes while it
// cannot read the controls: the ones where letting an operation through
// during an incident costs most.
var DefaultFailClosed = []Feature{FeatureDeposits, FeatureWithdrawals, FeatureTrading}

// DefaultApproval are the features whose active lockdown needs a second
// administrator to lift.
var DefaultApproval = []Feature{FeatureWithdrawals, FeatureTrading}

// Options configure a Watcher.
type Options struct {
	// PollInterval is how often the instance reads the version of the
	// controls, and evaluates them against the clock, so that a change
	// applies within it on every instance.
	PollInterval time.Duration
	// StaleAfter is how long the instance keeps applying the last controls
	// it read, when it cannot read them, before it closes FailClosed too.
	StaleAfter time.Duration
	// FailClosed are the features that are closed while the controls are
	// unknown: before the first read, and after StaleAfter without one.
	FailClosed []Feature
	Logger     *log.Logger
}

// Watcher keeps the controls in memory on one instance, so that the gates
// check them without a query, and tells listeners when what users see
// changes: a control that is created, starts, ends, is lifted, or is
// announced.
type Watcher struct {
	db         *sql.DB
	interval   time.Duration
	staleAfter time.Duration
	failClosed []Feature
	logger     *log.Logger
	now        func() time.Time

	mu        sync.RWMutex
	version   int64
	controls  []*Control
	loadedAt  time.Time
	listeners []func(Status)
	last      *Status
	staleSeen bool

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	started  bool
}

// NewWatcher returns a watcher of the controls in db.
func NewWatcher(db *sql.DB, opts Options) *Watcher {
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = DefaultStaleAfter
	}
	if opts.FailClosed == nil {
		opts.FailClosed = DefaultFailClosed
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	return &Watcher{db: db, interval: opts.PollInterval, staleAfter: opts.StaleAfter, failClosed: opts.FailClosed,
		logger: opts.Logger, now: time.Now, stop: make(chan struct{}), done: make(chan struct{})}
}

// OnChange adds a listener, called with the new status whenever what users
// see changes, from the watcher's goroutine. Add listeners before Start.
func (w *Watcher) OnChange(listener func(Status)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.listeners = append(w.listeners, listener)
}

// Start reads the controls once, then keeps them current until Stop. A
// failed first read is logged, not returned: the instance starts with
// FailClosed closed, and opens them at the first read that succeeds, so
// that it never starts silently open.
func (w *Watcher) Start(ctx context.Context) {
	if err := w.Refresh(ctx); err != nil {
		w.logger.Printf("Platform controls: the first read failed; %v stay closed until a read succeeds: %v", w.failClosed, err)
	}
	w.mu.Lock()
	w.started = true
	w.mu.Unlock()
	// The polls outlive ctx, which may be the startup's: Stop ends them.
	go w.run(context.WithoutCancel(ctx))
}

// Stop ends the watcher, and waits for it until ctx ends.
func (w *Watcher) Stop(ctx context.Context) error {
	w.stopOnce.Do(func() { close(w.stop) })
	w.mu.RLock()
	started := w.started
	w.mu.RUnlock()
	if !started {
		return nil
	}
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Watcher) run(base context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
		}
		ctx, cancel := context.WithTimeout(base, w.interval+5*time.Second)
		if err := w.poll(ctx); err != nil {
			w.mu.Lock()
			stale := w.staleLocked(w.now())
			first := stale && !w.staleSeen
			w.staleSeen = w.staleSeen || stale
			w.mu.Unlock()
			if first {
				w.logger.Printf("Platform controls: not read for over %v; closing %v until a read succeeds: %v", w.staleAfter, w.failClosed, err)
			}
		}
		cancel()
		w.notify()
	}
}

// poll reads the version, and the controls when it changed.
func (w *Watcher) poll(ctx context.Context) error {
	version, err := Version(ctx, w.db)
	if err != nil {
		return err
	}
	w.mu.RLock()
	same := version == w.version && !w.loadedAt.IsZero()
	w.mu.RUnlock()
	if same {
		w.mu.Lock()
		w.loadedAt = w.now()
		w.staleSeen = false
		w.mu.Unlock()
		return nil
	}
	return w.Refresh(ctx)
}

// Refresh reads the controls now. A server calls it after it changed a
// control, so that its own instance applies the change at once.
func (w *Watcher) Refresh(ctx context.Context) error {
	version, controls, err := Load(ctx, w.db)
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.version, w.controls, w.loadedAt, w.staleSeen = version, controls, w.now(), false
	w.mu.Unlock()
	w.notify()
	return nil
}

// staleLocked reports whether the controls are unknown at now.
func (w *Watcher) staleLocked(now time.Time) bool {
	return w.loadedAt.IsZero() || now.Sub(w.loadedAt) > w.staleAfter
}

// Locked returns what closes use for subject now, or nil.
func (w *Watcher) Locked(use Feature, subject Subject) *Locked {
	now := w.now()
	w.mu.RLock()
	defer w.mu.RUnlock()
	if locked := evaluate(w.controls, now, use, subject); locked != nil {
		return locked
	}
	if w.staleLocked(now) && slices.ContainsFunc(w.failClosed, func(f Feature) bool { return covers(KindLockdown, f, use) }) {
		return &Locked{Kind: KindLockdown, Feature: use, StateUnknown: true}
	}
	return nil
}

// Paused reports whether a lockdown pauses the background work feature,
// FeatureScheduler or FeatureOutbox. Unknown controls pause nothing, so
// that an outage of the database does not also stop the work that
// recovers from it.
func (w *Watcher) Paused(feature Feature) bool {
	now := w.now()
	w.mu.RLock()
	defer w.mu.RUnlock()
	return evaluate(w.controls, now, feature, Subject{}) != nil
}

// Status returns what users see now.
func (w *Watcher) Status() Status {
	now := w.now()
	w.mu.RLock()
	defer w.mu.RUnlock()
	return StatusOf(w.controls, now)
}

// notify calls the listeners when the status changed since the last call.
func (w *Watcher) notify() {
	status := w.Status()
	w.mu.Lock()
	if w.last != nil && reflect.DeepEqual(*w.last, status) {
		w.mu.Unlock()
		return
	}
	w.last = &status
	listeners := slices.Clone(w.listeners)
	w.mu.Unlock()
	for _, listener := range listeners {
		listener(status)
	}
}

// Status is what users see of the controls: the controls that apply now and
// those that are announced, without reasons, allow lists, or the controls
// of background work.
type Status struct {
	// Mode is maintenance during a full maintenance, and open otherwise.
	Mode string `json:"mode" enums:"open,maintenance"`
	// Lockdowns are the features that are closed now.
	Lockdowns []Feature `json:"lockdowns" enums:"sign_up,sign_in,profile_changes,deposits,withdrawals,trading,api_keys,websocket,read_only"`
	// Active and Scheduled are the controls that apply now, and those that
	// are announced and start later, by start.
	Active    []PublicControl `json:"active"`
	Scheduled []PublicControl `json:"scheduled"`
}

// Status modes.
const (
	ModeOpen        = "open"
	ModeMaintenance = "maintenance"
)

// PublicControl is a control as users see it.
type PublicControl struct {
	Kind     Kind       `json:"kind" enums:"maintenance,lockdown"`
	Feature  *Feature   `json:"feature,omitempty" enums:"sign_up,sign_in,profile_changes,deposits,withdrawals,trading,api_keys,websocket,read_only"`
	StartsAt time.Time  `json:"starts_at"`
	EndsAt   *time.Time `json:"ends_at,omitempty"`
	Message  Message    `json:"message"`
}

// StatusOf returns what users see of controls at now.
func StatusOf(controls []*Control, now time.Time) Status {
	s := Status{Mode: ModeOpen, Lockdowns: []Feature{}, Active: []PublicControl{}, Scheduled: []PublicControl{}}
	for _, c := range controls {
		if !c.visible() || c.endedAt(now) || now.Before(c.AnnounceAt) {
			continue
		}
		p := PublicControl{Kind: c.Kind, Feature: c.Feature, StartsAt: c.StartsAt.UTC(), Message: c.Message}
		if c.EndsAt != nil {
			end := c.EndsAt.UTC()
			p.EndsAt = &end
		}
		if !c.activeAt(now) {
			s.Scheduled = append(s.Scheduled, p)
			continue
		}
		s.Active = append(s.Active, p)
		if c.Kind == KindMaintenance {
			s.Mode = ModeMaintenance
		} else if !slices.Contains(s.Lockdowns, c.feature()) {
			s.Lockdowns = append(s.Lockdowns, c.feature())
		}
	}
	slices.Sort(s.Lockdowns)
	return s
}

// Fixed returns a watcher that holds controls and never reads a database,
// for tests and tools that run without one. Start and Refresh must not be
// called on it.
func Fixed(controls ...*Control) (*Watcher, error) {
	for _, c := range controls {
		if err := c.parseNetworks(); err != nil {
			return nil, err
		}
	}
	w := NewWatcher(nil, Options{StaleAfter: 100 * 365 * 24 * time.Hour})
	w.controls, w.loadedAt = controls, w.now()
	return w, nil
}
