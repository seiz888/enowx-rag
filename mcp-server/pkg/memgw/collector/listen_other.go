//go:build !windows && !linux

package collector

import (
	"errors"
	"net"
	"time"
)

// The listening endpoint needs a local transport the operating system puts an
// access control on: a named pipe with a DACL, or a unix socket with file
// permissions. A loopback TCP port is neither, and the collector accepts events
// that are written to a shared ledger under this host's identity.
//
// This build has no such transport implemented, so it refuses rather than
// binding something weaker.

// DefaultEndpoint exists so the shared configuration compiles; it names nothing
// that can be listened on here.
const DefaultEndpoint = ""

var errNoLocalTransport = errors.New("collector: the local endpoint requires a named pipe or a unix socket, which is implemented for Windows and Linux only")

// unusableListener exists only to give Listen a return type with the Name the
// shared service code logs. Nothing ever holds one: Listen always fails.
type unusableListener struct{ net.Listener }

// Name implements the shape the service expects.
func (unusableListener) Name() string { return "" }

// Listen refuses.
func Listen(string) (*unusableListener, error) { return nil, errNoLocalTransport }

// Dial refuses.
func Dial(string, time.Duration) (net.Conn, error) { return nil, errNoLocalTransport }
