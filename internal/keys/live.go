package keys

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultReloadInterval is how often Live checks data_keys for changes.
const DefaultReloadInterval = time.Minute

// minForcedReloadGap bounds the reloads that a read of an unknown version
// starts, so that values with made-up versions cannot make every request
// call the key management service.
const minForcedReloadGap = 5 * time.Second

// Live is the keyring of a running server, kept current with data_keys
// (#233). Every ReloadInterval it reads a signature of the table, and loads
// the keys again when it changed: a rotation, a retirement, or a re-wrap.
// So a version that one instance creates is readable on every instance
// within one interval, without a restart, and with the activation delay of
// Store.Rotate no instance writes with it before every instance can read it.
// A read of a version it does not hold also reloads at once, at most every
// few seconds, to cover an instance whose reload was late.
//
// A failed reload keeps the keys it has, and logs the failure: an instance
// never loses keys because the key management service is unreachable for a
// moment.
type Live struct {
	store    *Store
	interval time.Duration
	logger   *log.Logger

	ring atomic.Pointer[Keyring]

	// mu serializes reloads, and guards signature and lastForced.
	mu         sync.Mutex
	signature  string
	lastForced time.Time

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
	started  atomic.Bool
}

// LiveOptions configure a Live keyring.
type LiveOptions struct {
	// ReloadInterval is how often the table is checked; zero means
	// DefaultReloadInterval.
	ReloadInterval time.Duration
	// Logger receives reload failures; nil means the standard logger.
	Logger *log.Logger
}

// NewLive loads the keys once, creating the first version of each purpose
// if needed, and returns the keyring. It fails as Store.Load fails. Start
// keeps it current.
func NewLive(ctx context.Context, store *Store, opts LiveOptions) (*Live, error) {
	l := &Live{
		store:    store,
		interval: opts.ReloadInterval,
		logger:   opts.Logger,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	if l.interval <= 0 {
		l.interval = DefaultReloadInterval
	}
	if l.logger == nil {
		l.logger = log.Default()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.reloadLocked(ctx, true); err != nil {
		return nil, err
	}
	return l, nil
}

// Keyring returns the keys as last loaded. It never changes; a reload
// replaces it.
func (l *Live) Keyring() *Keyring { return l.ring.Load() }

// Interval returns the reload interval.
func (l *Live) Interval() time.Duration { return l.interval }

// Reload loads the keys again if data_keys changed since the last load, and
// reports whether it did.
func (l *Live) Reload(ctx context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reloadLocked(ctx, false)
}

// reloadLocked reads the signature of data_keys and loads the keys when it
// changed, or always when force is set. The caller holds mu.
func (l *Live) reloadLocked(ctx context.Context, force bool) (bool, error) {
	signature, err := l.store.signature(ctx)
	if err != nil {
		return false, err
	}
	if !force && signature == l.signature && l.ring.Load() != nil {
		return false, nil
	}
	ring, err := l.store.Load(ctx)
	if err != nil {
		return false, err
	}
	// Load may have created a first key, which changed the table; read
	// the signature again, so that the next check does not load twice.
	if signature, err = l.store.signature(ctx); err != nil {
		return false, err
	}
	l.ring.Store(ring)
	l.signature = signature
	return true, nil
}

// Start checks data_keys every interval until Stop.
func (l *Live) Start() {
	if !l.started.CompareAndSwap(false, true) {
		return
	}
	go l.run()
}

// Stop ends the reloads, and waits for one in progress until ctx ends.
func (l *Live) Stop(ctx context.Context) error {
	l.stopOnce.Do(func() { close(l.stop) })
	if !l.started.Load() {
		return nil
	}
	select {
	case <-l.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Live) run() {
	defer close(l.done)
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	failing := false
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), l.interval)
		reloaded, err := l.Reload(ctx)
		cancel()
		switch {
		case err != nil && !failing:
			l.logger.Printf("Data keys: reloading failed; keeping the keys loaded before: %v", err)
			failing = true
		case err == nil && failing:
			l.logger.Printf("Data keys: reloading works again")
			failing = false
		}
		if reloaded {
			l.logger.Printf("Data keys: loaded a change of data_keys; current versions: %s", l.currentVersions())
		}
	}
}

// currentVersions names the version of each purpose that writes now.
func (l *Live) currentVersions() string {
	ring := l.Keyring()
	out := ""
	for i, purpose := range Purposes {
		if i > 0 {
			out += ", "
		}
		if key, err := ring.Current(purpose); err == nil {
			out += key.String()
		} else {
			out += string(purpose) + "/none"
		}
	}
	return out
}

// Current returns the key of purpose that writes now; see Keyring.Current.
func (l *Live) Current(purpose Purpose) (Key, error) { return l.Keyring().Current(purpose) }

// Versions is the Versions of the loaded keyring.
func (l *Live) Versions(purpose Purpose) []Key { return l.Keyring().Versions(purpose) }

// Version returns one version of purpose. A version it does not hold makes
// it reload at once, at most every few seconds, and look again: another
// instance may have written with a version that this instance has not
// loaded yet.
func (l *Live) Version(ctx context.Context, purpose Purpose, version int) (Key, error) {
	key, err := l.Keyring().Version(purpose, version)
	if !errors.Is(err, ErrNoKey) || !l.forceReload(ctx) {
		return key, err
	}
	return l.Keyring().Version(purpose, version)
}

// SealField seals with the current field-enc key; see Keyring.SealField.
func (l *Live) SealField(aad, plaintext []byte) ([]byte, error) {
	return l.Keyring().SealField(aad, plaintext)
}

// OpenField opens a sealed value; see Keyring.OpenField. A value sealed
// with a version it does not hold makes it reload, as Version does.
func (l *Live) OpenField(aad, sealed []byte) ([]byte, error) {
	plaintext, err := l.Keyring().OpenField(aad, sealed)
	if !errors.Is(err, ErrNoKey) {
		return plaintext, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !l.forceReload(ctx) {
		return nil, err
	}
	return l.Keyring().OpenField(aad, sealed)
}

// forceReload reloads unless one was forced within minForcedReloadGap, and
// reports whether the keys changed.
func (l *Live) forceReload(ctx context.Context) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if time.Since(l.lastForced) < minForcedReloadGap {
		return false
	}
	l.lastForced = time.Now()
	reloaded, err := l.reloadLocked(ctx, false)
	if err != nil {
		l.logger.Printf("Data keys: reloading for an unknown version failed: %v", err)
	}
	return reloaded
}

// signature summarizes data_keys: every row, its KEK, activation, and
// retirement. It changes with every rotation, retirement, and re-wrap.
func (s *Store) signature(ctx context.Context) (string, error) {
	var signature string
	err := s.DB.QueryRowContext(ctx, `
		SELECT COALESCE(string_agg(purpose || '/' || version || '/' || kek_id || '/' || md5(wrapped_key) || '/' ||
		       extract(epoch FROM active_from) || '/' || COALESCE(extract(epoch FROM retired_at)::text, '-'),
		       ',' ORDER BY purpose, version), '')
		FROM data_keys`).Scan(&signature)
	if err != nil {
		return "", fmt.Errorf("reading the data keys: %w", err)
	}
	return signature, nil
}
