//go:build unix

package tronsigner

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockFile takes an exclusive lock on path, which another process that
// holds it refuses (#551). The lock lasts while the file stays open.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: the state path of the configuration, given by the operator.
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s is held", ErrRunning, path)
		}
		return nil, err
	}
	return f, nil
}
