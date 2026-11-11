//go:build unix

package app

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestServe_StopsGracefullyOnSIGTERM sends SIGTERM to the test process, which
// stopSignals catches as it does in main, while a request is in flight.
func TestServe_StopsGracefullyOnSIGTERM(t *testing.T) {
	ctx, stop := stopSignals()
	defer stop()
	checkGracefulStop(t, ctx, func() {
		require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGTERM))
	})
}
