//go:build linux

package collector

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// The listening endpoint on Linux.
//
// The Windows collector uses a named pipe because a pipe can carry a DACL and a
// loopback TCP port cannot. The same argument decides this one: a unix socket
// carries file permissions, a loopback port does not, and the collector accepts
// events that are written to a shared ledger under this host's identity. "Any
// local process may enqueue" would be an authorisation hole with a bind()
// pretending to be a boundary.
//
// Two things are enforced rather than assumed.
//
//   - The socket comes up 0600 and is verified to be 0600 afterwards.
//     Permissions are set with umask around the bind, not with a chmod
//     afterwards, because between bind and chmod the socket is connectable.
//   - The parent directory must not be writable by group or others. A 0600
//     socket inside a directory anyone can write to can be unlinked and
//     replaced by somebody else's socket, and the adapters would then be
//     handing lifecycle events to whoever won the race. That is refused, not
//     warned about. A directory this code creates is 0700; one it finds is
//     required only to be unwritable by anyone else, because /run/memgw made by
//     systemd's RuntimeDirectory= is 0755 and that is not a hole -- the socket
//     inside it still admits nobody but its owner.
//
// There is no abstract-namespace option. An abstract socket has no filesystem
// entry and therefore no permissions at all: every process in the network
// namespace can connect to it.
const (
	// DefaultEndpoint is where the adapters look. Under /run because the socket
	// is runtime state that must not survive a reboot: a stale socket file is
	// something to clean up, and /run is cleaned for us.
	DefaultEndpoint = "/run/memgw/collector.sock"

	socketDirPerm  fs.FileMode = 0o700
	socketPerm     fs.FileMode = 0o600
	socketDialWait             = 5 * time.Second
)

// SocketListener accepts local client connections.
type SocketListener struct {
	net.Listener
	path string
}

// Name returns the socket path, for logging.
func (l *SocketListener) Name() string { return l.path }

// Listen creates the socket, refusing anything about its location that would
// make the permissions meaningless.
func Listen(path string) (*SocketListener, error) {
	if path == "" {
		path = DefaultEndpoint
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, socketDirPerm); err != nil {
		return nil, fmt.Errorf("collector: create %s: %w", dir, err)
	}
	// MkdirAll leaves an existing directory's mode alone, so the check below is
	// about the directory we found, not the one we would have made.
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("collector: inspect %s: %w", dir, err)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf(
			"collector: %s is writable by group or others (%04o), so the socket in it could be replaced; make it 0700",
			dir, info.Mode().Perm())
	}

	// A socket left behind by a killed process would make bind fail with
	// "address already in use". Removing it is only safe once we know nothing
	// is listening: a successful dial means a live collector, and two
	// collectors on one spool is the thing this must not do.
	if _, err := os.Stat(path); err == nil {
		if c, derr := net.DialTimeout("unix", path, time.Second); derr == nil {
			c.Close()
			return nil, fmt.Errorf("collector: %s is already served by a running collector", path)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("collector: remove the stale socket %s: %w", path, err)
		}
	}

	// The umask is what makes the socket 0600 from the moment it exists. A
	// chmod after Listen would leave a window in which it is connectable by
	// anyone, and that window is exactly when a racing process is looking.
	umaskMu.Lock()
	old := syscall.Umask(int(^socketPerm & 0o777))
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	umaskMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("collector: listen on %s: %w", path, err)
	}

	// Verify rather than trust: a umask the runtime or a wrapper changed under
	// us would otherwise produce a world-connectable socket silently.
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm()&0o077 != 0 {
		ln.Close()
		os.Remove(path)
		if err != nil {
			return nil, fmt.Errorf("collector: inspect %s: %w", path, err)
		}
		return nil, fmt.Errorf("collector: %s came up as %04o, not 0600", path, st.Mode().Perm())
	}
	return &SocketListener{Listener: ln, path: path}, nil
}

// Dial connects to a running collector.
func Dial(path string, timeout time.Duration) (net.Conn, error) {
	if path == "" {
		path = DefaultEndpoint
	}
	if timeout <= 0 {
		timeout = socketDialWait
	}
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return nil, fmt.Errorf("collector: connect to %s: %w", path, err)
	}
	return conn, nil
}

// umaskMu serialises the umask dance. The umask is process-wide state, so two
// goroutines binding at once could restore each other's value and leave the
// process running with a permissive mask.
var umaskMu sync.Mutex
