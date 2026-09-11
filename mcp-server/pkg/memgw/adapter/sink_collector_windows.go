//go:build windows

package adapter

import (
	"context"
	"strings"
	"time"

	"github.com/enowdev/enowx-rag/pkg/memgw/collector"
)

// CollectorSink writes to the local collector over its named pipe.
//
// A fresh connection per submission, deliberately. A hook is a short-lived
// process invoked once per lifecycle moment: there is no second submission to
// amortise a kept connection over, and a connection held across invocations
// would have to survive a process that is expected to exit.
type CollectorSink struct {
	pipe    string
	timeout time.Duration
}

// NewCollectorSink names the pipe the local collector listens on.
func NewCollectorSink(pipe string, timeout time.Duration) *CollectorSink {
	if timeout <= 0 {
		// The collector is on this machine and answers from a local SQLite
		// commit. If it has not answered in five seconds it is not going to,
		// and the hook must not hold the agent session open waiting.
		timeout = 5 * time.Second
	}
	// A configuration that names the pipe without its namespace is the common
	// mistake, and Windows answers it with "the filename, directory name, or
	// volume label syntax is incorrect", which tells nobody anything. Accepting
	// the short form costs one line.
	if pipe != "" && !strings.HasPrefix(pipe, `\\`) {
		pipe = `\\.\pipe\` + pipe
	}
	return &CollectorSink{pipe: pipe, timeout: timeout}
}

// Name implements Sink.
func (c *CollectorSink) Name() string { return "collector" }

// Submit implements Sink.
func (c *CollectorSink) Submit(ctx context.Context, s Submission) (Result, error) {
	conn, err := collector.Dial(c.pipe, c.timeout)
	if err != nil {
		// Not reachable is not the same as refused: the caller decides whether
		// to fall back to the gateway, and it can only decide that if the two
		// are distinguishable.
		return Result{Via: c.Name()}, err
	}
	defer conn.Close()
	return submitOverPipe(conn, s)
}
