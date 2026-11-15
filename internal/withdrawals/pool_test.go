package withdrawals

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRequest_NeedsOneConnectionOfThePool runs both steps of a request with
// a pool of one connection (#405): a step that held its transaction while
// it read on another connection would wait for itself until its deadline.
func TestRequest_NeedsOneConnectionOfThePool(t *testing.T) {
	f := newFixture(t, "")
	f.DB.SetMaxOpenConns(1)
	t.Cleanup(func() { f.DB.SetMaxOpenConns(0) })
	// The steps run without a deadline of their own, so a regression
	// would wait forever: the test gives up after ten seconds instead.
	type result struct {
		w   Withdrawal
		err error
	}
	done := make(chan result, 1)
	go func() {
		w, err := f.withdraw(f.request("100", "pool-one"))
		done <- result{w, err}
	}()
	var got result
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a step of the request waits for a second connection of the pool")
	}
	require.NoError(t, got.err)
	assert.Equal(t, StatusQueued, got.w.Status)
	available, locked := f.balance()
	assert.Equal(t, "899", available)
	assert.Equal(t, "101", locked)
}

// TestRequest_MoreRequestsThanConnections runs more requests at once than
// the pool has connections: each needs one at a time, so they all
// complete, none at its deadline.
func TestRequest_MoreRequestsThanConnections(t *testing.T) {
	f := newFixture(t, "")
	f.DB.SetMaxOpenConns(2)
	t.Cleanup(func() { f.DB.SetMaxOpenConns(0) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const requests = 8
	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := f.Request(ctx, f.user, f.request("25", fmt.Sprintf("crowd-%d", i)), "client", origin)
			if err == nil && out.Challenge == nil {
				err = fmt.Errorf("request %d: no confirmation", i)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.NoError(t, ctx.Err(), "no request waited for its deadline")
}
