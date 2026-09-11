//go:build !windows && !linux

package adapter

import (
	"context"
	"errors"
	"time"
)

// CollectorSink does not exist on this platform.
//
// The collector needs a local transport the operating system puts an access
// control on -- a named pipe with a DACL on Windows, a unix socket with file
// permissions on Linux -- and a key store the operating system holds. Both
// exist for those two platforms and neither exists here, so this file makes the
// gap a compile-time fact rather than a runtime surprise: a configuration that
// names a collector endpoint is refused when the sink is built, with a message
// that says what is actually missing.
//
// A host on such a platform submits straight to the gateway and loses its
// lifecycle events while the gateway is unreachable.
type CollectorSink struct{ pipe string }

// ErrNoLocalCollector is returned when a collector is asked for on a platform
// that has none.
var ErrNoLocalCollector = errors.New("adapter: this build has no local collector; the collector exists for Windows and Linux, and this is neither")

// NewCollectorSink returns a sink that refuses every submission.
func NewCollectorSink(pipe string, _ time.Duration) *CollectorSink { return &CollectorSink{pipe: pipe} }

// Name implements Sink.
func (c *CollectorSink) Name() string { return "collector" }

// Submit implements Sink.
func (c *CollectorSink) Submit(context.Context, Submission) (Result, error) {
	return Result{Via: c.Name()}, ErrNoLocalCollector
}
