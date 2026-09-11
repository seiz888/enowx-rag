//go:build windows

package collector

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The listening endpoint.
//
// A named pipe is used rather than a loopback TCP port for one reason that
// matters: a pipe can be given a DACL, and a TCP port cannot. Any process on
// this machine, running as any user, can connect to 127.0.0.1:<port>; only the
// accounts named in the DACL can open a pipe. Since the collector accepts
// events that are written to a shared ledger under this host's identity, "any
// local process may enqueue" would have been an authorisation hole with a
// loopback bind pretending to be a boundary.
//
// The DACL is protected (the P in the SDDL) so it does not inherit anything,
// and it names exactly three trustees: the account the collector runs as,
// SYSTEM, and the local Administrators group. Administrators are on the list
// because on Windows they can take ownership of the object anyway; excluding
// them would express an intention the operating system does not enforce.
const (
	// DefaultEndpoint is where the adapters look. It is under the standard
	// \\.\pipe namespace, and the name itself is not a secret -- the DACL is
	// what keeps other users out, not obscurity.
	DefaultEndpoint = `\\.\pipe\memgw-collector`

	pipeBufferBytes  = 64 << 10
	pipeMaxInstances = 16
	pipeTimeoutMs    = 5000
)

// PipeListener accepts local client connections.
type PipeListener struct {
	name string
	sa   *windows.SecurityAttributes
	sd   *windows.SECURITY_DESCRIPTOR

	mu     sync.Mutex
	closed bool
	// first is the instance created with FILE_FLAG_FIRST_PIPE_INSTANCE, held
	// open for the lifetime of the listener. Keeping it open is what prevents
	// another process from creating an instance of the same name after the
	// collector has started: while any instance exists, a create without the
	// flag joins the existing pipe, and joining requires the same DACL check.
	pending windows.Handle
}

// Listen creates the pipe.
//
// FILE_FLAG_FIRST_PIPE_INSTANCE is the security-relevant part: if something
// else already owns this name, creation fails and the collector refuses to
// start, rather than quietly becoming the second instance behind a squatter
// that would receive some of the connections.
func Listen(name string) (*PipeListener, error) {
	if name == "" {
		name = DefaultEndpoint
	}
	sd, err := ownerOnlyDescriptor()
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
		InheritHandle:      0,
	}
	l := &PipeListener{name: name, sa: sa, sd: sd, pending: windows.InvalidHandle}

	h, err := l.create(true)
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PIPE_BUSY) ||
			errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return nil, fmt.Errorf("collector: the pipe %s already exists; another collector is running, "+
				"or something else has taken the name: %w", name, err)
		}
		return nil, fmt.Errorf("collector: create %s: %w", name, err)
	}
	l.pending = h
	return l, nil
}

func (l *PipeListener) create(first bool) (windows.Handle, error) {
	namePtr, err := windows.UTF16PtrFromString(l.name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	flags := uint32(windows.PIPE_ACCESS_DUPLEX)
	if first {
		flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	return windows.CreateNamedPipe(namePtr, flags,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		pipeMaxInstances, pipeBufferBytes, pipeBufferBytes, pipeTimeoutMs, l.sa)
}

// Name is the pipe's name, so a caller can tell an operator where to look.
func (l *PipeListener) Name() string { return l.name }

// Accept waits for one client and returns the connection.
//
// The instance handed to the client is retired with it: the next Accept creates
// a fresh instance. That costs a syscall per connection and removes an entire
// class of bug where a half-closed pipe is reused for the next client.
func (l *PipeListener) Accept() (io.ReadWriteCloser, error) {
	// The pending handle is taken, not borrowed: exactly one of Accept and
	// Close ends up owning it, which is what keeps a shutdown racing an accept
	// from closing the same handle twice.
	h, ok := l.take()
	if !ok {
		return nil, net.ErrClosed
	}

	if h == windows.InvalidHandle {
		var err error
		if h, err = l.create(false); err != nil {
			return nil, err
		}
	}
	err := windows.ConnectNamedPipe(h, nil)
	// A client that connected between CreateNamedPipe and ConnectNamedPipe is
	// already connected, which Windows reports as an error and is not one.
	if err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
		windows.CloseHandle(h)
		l.mu.Lock()
		closed := l.closed
		l.mu.Unlock()
		if closed {
			return nil, net.ErrClosed
		}
		return nil, err
	}

	l.mu.Lock()
	closed := l.closed
	l.mu.Unlock()
	if closed {
		// The connection that woke us is the shutdown poke; it is not a client.
		windows.DisconnectNamedPipe(h)
		windows.CloseHandle(h)
		return nil, net.ErrClosed
	}

	// Prepare the next instance now, so a client connecting during the handling
	// of this one is queued by the operating system rather than refused.
	if next, err := l.create(false); err == nil {
		l.mu.Lock()
		if l.closed {
			// Closed while we were creating it: nobody will ever accept on this
			// instance, and leaving it open would keep the name alive.
			l.mu.Unlock()
			windows.CloseHandle(next)
		} else {
			l.pending = next
			l.mu.Unlock()
		}
	}
	return &pipeConn{h: h}, nil
}

// take hands the pending instance to exactly one caller. The second caller gets
// InvalidHandle, and a caller that arrives after Close gets ok == false.
func (l *PipeListener) take() (windows.Handle, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return windows.InvalidHandle, false
	}
	h := l.pending
	l.pending = windows.InvalidHandle
	return h, true
}

// Close stops the listener.
//
// Unblocking Accept is done by connecting to our own pipe rather than by
// closing a handle another thread is blocked in: closing a handle out from
// under a blocking ConnectNamedPipe is documented as leaving the pipe in an
// undefined state, and "undefined" in the shutdown path is how a service comes
// back up with a name it cannot reclaim.
func (l *PipeListener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()

	if conn, err := Dial(l.name, time.Second); err == nil {
		conn.Close()
	}

	l.mu.Lock()
	h := l.pending
	l.pending = windows.InvalidHandle
	l.mu.Unlock()
	if h != windows.InvalidHandle {
		windows.DisconnectNamedPipe(h)
		return windows.CloseHandle(h)
	}
	return nil
}

// pipeConn is one accepted connection.
type pipeConn struct {
	mu     sync.Mutex
	h      windows.Handle
	closed bool
}

func (c *pipeConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var n uint32
	err := windows.ReadFile(c.h, p, &n, nil)
	if err != nil {
		// A client that went away is EOF, not a failure worth logging as one.
		if errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED) {
			return int(n), io.EOF
		}
		return int(n), err
	}
	if n == 0 {
		return 0, io.EOF
	}
	return int(n), nil
}

func (c *pipeConn) Write(p []byte) (int, error) {
	var written int
	for written < len(p) {
		var n uint32
		if err := windows.WriteFile(c.h, p[written:], &n, nil); err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
		written += int(n)
	}
	return written, nil
}

func (c *pipeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	windows.FlushFileBuffers(c.h)
	windows.DisconnectNamedPipe(c.h)
	return windows.CloseHandle(c.h)
}

// Dial opens a client connection. It exists for the adapters and for the tests
// that have to prove the ACL does what it claims.
func Dial(name string, timeout time.Duration) (io.ReadWriteCloser, error) {
	if name == "" {
		name = DefaultEndpoint
	}
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		h, err := windows.CreateFile(namePtr,
			windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
			windows.OPEN_EXISTING, 0, 0)
		if err == nil {
			return &pipeConn{h: h}, nil
		}
		// Busy means every instance is taken this instant, which is a wait, not
		// a refusal. Access denied is a refusal and is returned immediately:
		// retrying an ACL will not change its mind.
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ownerOnlyDescriptor builds the DACL: this account, SYSTEM, Administrators.
//
// The SID of the running account is read from the process token rather than
// assembled from a user name, because a name can be ambiguous across a domain
// and a SID cannot.
func ownerOnlyDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("collector: read the process token: %w", err)
	}
	sid := user.User.Sid.String()

	// D:P            -- a protected DACL: it inherits nothing.
	// (A;;GA;;;SY)   -- SYSTEM, full.
	// (A;;GA;;;BA)   -- built-in Administrators, full.
	// (A;;GA;;;<us>) -- the account the collector runs as.
	// Nothing else is listed, and an unlisted trustee has no access at all.
	sddl := fmt.Sprintf("D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;%s)", sid)
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, fmt.Errorf("collector: build the pipe DACL: %w", err)
	}
	return sd, nil
}

// DescribeACL returns the SDDL actually applied, so a test or an operator can
// read back what is enforced instead of trusting a comment.
func DescribeACL() (string, error) {
	sd, err := ownerOnlyDescriptor()
	if err != nil {
		return "", err
	}
	return sd.String(), nil
}

// CurrentUserSID is used by the tests that assert the DACL names this account
// and not, for instance, Everyone.
func CurrentUserSID() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}
