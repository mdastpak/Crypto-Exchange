package keys

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sealedValue is a value that one instance sealed, with its place.
type sealedValue struct {
	by     string
	aad    []byte
	sealed []byte
	plain  []byte
}

// TestLive_TwoInstancesRotateWithoutAFailure runs two instances that seal
// and open values continuously while an operator rotates the field-enc key
// (#233). Every value that either instance seals must open on both, at
// every moment: before, during, and after the activation of the new
// version. A third instance, started after the rotation, must read the
// values of the other two, and they its values.
func TestLive_TwoInstancesRotateWithoutAFailure(t *testing.T) {
	ctx := context.Background()
	provider := localProvider(t)
	operator, conn := newTestStore(t, provider)
	operator.ActivationDelay = 1500 * time.Millisecond
	quiet := log.New(&bytes.Buffer{}, "", 0)
	options := LiveOptions{ReloadInterval: 200 * time.Millisecond, Logger: quiet}

	start := func(name string) *Live {
		live, err := NewLive(ctx, &Store{DB: conn, Provider: provider}, options)
		require.NoError(t, err, name)
		live.Start()
		t.Cleanup(func() { _ = live.Stop(context.Background()) })
		return live
	}
	instances := map[string]*Live{"a": start("a"), "b": start("b")}

	var (
		mu       sync.Mutex
		values   []sealedValue
		failures []string
		versions = map[int]bool{}
	)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var counter atomic.Int64
	for name, live := range instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				n := counter.Add(1)
				aad := []byte(fmt.Sprintf("users.email:%d", n))
				plain := []byte(fmt.Sprintf("user%d@example.com", n))
				sealed, err := live.SealField(aad, plain)
				mu.Lock()
				if err != nil {
					failures = append(failures, name+" sealed: "+err.Error())
				} else {
					values = append(values, sealedValue{by: name, aad: aad, sealed: sealed, plain: plain})
					var version int
					_, _ = fmt.Sscanf(string(sealed), "%d:", &version)
					versions[version] = true
				}
				// Every instance opens the newest values of every other
				// instance, as a request routed anywhere would.
				recent := values[max(0, len(values)-8):]
				mu.Unlock()
				for _, v := range recent {
					for other, reader := range instances {
						got, err := reader.OpenField(v.aad, v.sealed)
						if err != nil || !bytes.Equal(got, v.plain) {
							mu.Lock()
							failures = append(failures, fmt.Sprintf("%s could not open a value of %s: %v", other, v.by, err))
							mu.Unlock()
						}
					}
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}

	time.Sleep(300 * time.Millisecond)
	version, err := operator.Rotate(ctx, PurposeFieldEnc)
	require.NoError(t, err)
	require.Equal(t, 2, version)
	late := start("c")
	time.Sleep(2500 * time.Millisecond) // past the activation
	close(stop)
	wg.Wait()

	assert.Empty(t, failures)
	assert.True(t, versions[1] && versions[2], "the instances wrote with both versions: %v", versions)
	for name, live := range instances {
		key, err := live.Current(PurposeFieldEnc)
		require.NoError(t, err)
		assert.Equal(t, 2, key.Version(), "%s writes with the new version after the activation", name)
	}

	// The instance started after the rotation and the ones started before
	// it read each other's values, of both versions.
	instances["c"] = late
	for _, v := range values {
		got, err := late.OpenField(v.aad, v.sealed)
		require.NoError(t, err)
		assert.Equal(t, v.plain, got)
	}
	sealed, err := late.SealField([]byte("x"), []byte("from c"))
	require.NoError(t, err)
	for name, live := range instances {
		got, err := live.OpenField([]byte("x"), sealed)
		require.NoError(t, err, name)
		assert.Equal(t, []byte("from c"), got)
	}
}

// TestLive_ReadsAVersionItHasNotLoadedYet opens a value sealed with a
// version that the instance has not loaded, because its periodic reload is
// far away: it reloads at once, and opens it.
func TestLive_ReadsAVersionItHasNotLoadedYet(t *testing.T) {
	ctx := context.Background()
	provider := localProvider(t)
	operator, conn := newTestStore(t, provider)
	slow, err := NewLive(ctx, &Store{DB: conn, Provider: provider}, LiveOptions{ReloadInterval: time.Hour})
	require.NoError(t, err)

	_, err = operator.Rotate(ctx, PurposeFieldEnc) // active at once: no delay
	require.NoError(t, err)
	fresh, err := NewLive(ctx, &Store{DB: conn, Provider: provider}, LiveOptions{})
	require.NoError(t, err)
	sealed, err := fresh.SealField([]byte("aad"), []byte("secret"))
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(sealed, []byte("2:")))

	_, err = slow.Keyring().OpenField([]byte("aad"), sealed)
	require.ErrorIs(t, err, ErrNoKey, "the loaded keyring does not hold v2")
	got, err := slow.OpenField([]byte("aad"), sealed)
	require.NoError(t, err)
	assert.Equal(t, []byte("secret"), got)

	key, err := slow.Version(ctx, PurposeFieldEnc, 2)
	require.NoError(t, err)
	assert.Equal(t, 2, key.Version())

	// A made-up version reloads at most once within the gap.
	_, err = slow.Version(ctx, PurposeFieldEnc, 99)
	assert.ErrorIs(t, err, ErrNoKey)
}

// TestLive_KeepsItsKeysWhenAReloadFails changes the table so that a reload
// cannot unwrap a key: the instance keeps writing and reading with the
// keys it has, and reports the failure.
func TestLive_KeepsItsKeysWhenAReloadFails(t *testing.T) {
	ctx := context.Background()
	provider := localProvider(t)
	_, conn := newTestStore(t, provider)
	live, err := NewLive(ctx, &Store{DB: conn, Provider: provider}, LiveOptions{})
	require.NoError(t, err)
	sealed, err := live.SealField([]byte("aad"), []byte("v1 value"))
	require.NoError(t, err)

	// Another KEK wraps the next version, which this instance cannot unwrap.
	_, err = (&Store{DB: conn, Provider: localProvider(t)}).Rotate(ctx, PurposeFieldEnc)
	require.NoError(t, err)
	reloaded, err := live.Reload(ctx)
	assert.False(t, reloaded)
	require.Error(t, err)

	key, err := live.Current(PurposeFieldEnc)
	require.NoError(t, err)
	assert.Equal(t, 1, key.Version())
	got, err := live.OpenField([]byte("aad"), sealed)
	require.NoError(t, err)
	assert.Equal(t, []byte("v1 value"), got)
}

// TestLive_ReloadsOnlyOnAChange checks that an unchanged table loads
// nothing, so the key service is only called for a change.
func TestLive_ReloadsOnlyOnAChange(t *testing.T) {
	ctx := context.Background()
	provider := localProvider(t)
	operator, conn := newTestStore(t, provider)
	live, err := NewLive(ctx, &Store{DB: conn, Provider: provider}, LiveOptions{})
	require.NoError(t, err)
	assert.Equal(t, DefaultReloadInterval, live.Interval())

	reloaded, err := live.Reload(ctx)
	require.NoError(t, err)
	assert.False(t, reloaded, "the first keys that NewLive created count as loaded")

	_, err = operator.Rotate(ctx, PurposeRowMAC)
	require.NoError(t, err)
	reloaded, err = live.Reload(ctx)
	require.NoError(t, err)
	assert.True(t, reloaded)

	require.NoError(t, operator.Retire(ctx, PurposeRowMAC, 1))
	reloaded, err = live.Reload(ctx)
	require.NoError(t, err)
	assert.True(t, reloaded, "a retirement is a change")
	_, err = live.Keyring().Version(PurposeRowMAC, 1)
	assert.ErrorIs(t, err, ErrNoKey, "a retired version is dropped")

	require.NoError(t, live.Stop(ctx), "stopping a keyring that never started")
}

// TestStore_ActivationDelay checks the two phases in the store: a rotated
// version reads at once and writes after the delay, the first version
// writes at once, and the version that writes cannot be retired.
func TestStore_ActivationDelay(t *testing.T) {
	ctx := context.Background()
	provider := localProvider(t)
	store, _ := newTestStore(t, provider)
	store.ActivationDelay = time.Hour

	version, err := store.Rotate(ctx, PurposeBlindIndex)
	require.NoError(t, err)
	require.Equal(t, 1, version)
	ring, err := store.Load(ctx)
	require.NoError(t, err)
	key, err := ring.Current(PurposeBlindIndex)
	require.NoError(t, err, "a first version writes at once")
	assert.Equal(t, 1, key.Version())

	_, err = store.Rotate(ctx, PurposeBlindIndex)
	require.NoError(t, err)
	ring, err = store.Load(ctx)
	require.NoError(t, err)
	key, err = ring.Current(PurposeBlindIndex)
	require.NoError(t, err)
	assert.Equal(t, 1, key.Version(), "v2 waits for its activation")
	_, err = ring.Version(PurposeBlindIndex, 2)
	assert.NoError(t, err, "v2 reads at once")

	ring.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	key, err = ring.Current(PurposeBlindIndex)
	require.NoError(t, err)
	assert.Equal(t, 2, key.Version(), "after the delay, v2 writes")

	err = store.Retire(ctx, PurposeBlindIndex, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still writes")

	infos, err := store.List(ctx)
	require.NoError(t, err)
	for _, info := range infos {
		if info.Purpose == PurposeBlindIndex {
			assert.Equal(t, info.Version == 1, info.Current, "v%d", info.Version)
		}
	}

	store.ActivationDelay = -time.Second
	_, err = store.Rotate(ctx, PurposeBlindIndex)
	assert.Error(t, err)
}
