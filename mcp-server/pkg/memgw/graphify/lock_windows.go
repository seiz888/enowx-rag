//go:build windows

package graphify

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// ErrLocked is returned when another process is already rebuilding.
//
// It is a distinct error because the two reasonable responses are different: a
// scheduled rebuild should skip and try later, while an operator asking for one
// should be told somebody else is holding it rather than waiting silently.
var ErrLocked = errors.New("graphify: another rebuild holds the coordinator lock")

// Lock is an exclusive OS lock on one file.
//
// LockFileEx with LOCKFILE_FAIL_IMMEDIATELY, not a lock file whose contents are
// interpreted. The distinction is the whole point on Windows: this lock is a
// property of an open handle, so when the holder dies -- cleanly, by kill, or
// by the machine losing power -- the kernel drops it and the next rebuild
// proceeds. There is no stale lock to reason about and no recovery procedure to
// get wrong.
//
// The repository's fcntl-based fallback is deliberately not used. fcntl locking
// on Windows is emulated and its semantics under crash are not the ones this
// depends on.
type Lock struct {
	f  *os.File
	ov *windows.Overlapped
}

// AcquireLock takes the lock or returns ErrLocked immediately.
func AcquireLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("graphify: the lock file could not be opened: %w", err)
	}
	ov := new(windows.Overlapped)
	err = windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, ov)
	if err != nil {
		f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("graphify: the coordinator lock could not be taken: %w", err)
	}
	// The pid is written for a human reading the file during an incident. It is
	// never read back by this package: the lock is the handle, not the text.
	_, _ = f.Seek(0, 0)
	_ = f.Truncate(0)
	fmt.Fprintf(f, "pid %d\n", os.Getpid())
	return &Lock{f: f, ov: ov}, nil
}

// Release drops the lock. Closing the handle would do it too; unlocking first
// makes the intent explicit and keeps the two platforms symmetric.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, l.ov)
	cerr := l.f.Close()
	l.f = nil
	if err != nil {
		return err
	}
	return cerr
}
