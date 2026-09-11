//go:build !windows

package graphify

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrLocked is returned when another process is already rebuilding.
var ErrLocked = errors.New("graphify: another rebuild holds the coordinator lock")

// Lock is an exclusive OS lock on one file.
//
// flock on this side, LockFileEx on Windows. Both are properties of an open
// file description, so both are released by the kernel when the holder dies.
// The coordinator is specified as one per repository on Windows; this exists so
// the package builds and can be tested on the Linux host that runs Hermes, not
// because a second platform is a supported deployment.
type Lock struct {
	f *os.File
}

// AcquireLock takes the lock or returns ErrLocked immediately.
func AcquireLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("graphify: the lock file could not be opened: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("graphify: the coordinator lock could not be taken: %w", err)
	}
	_, _ = f.Seek(0, 0)
	_ = f.Truncate(0)
	fmt.Fprintf(f, "pid %d\n", os.Getpid())
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	cerr := l.f.Close()
	l.f = nil
	if err != nil {
		return err
	}
	return cerr
}
