//go:build linux

package adapter

import (
	"context"
	"time"

	"github.com/enowdev/enowx-rag/pkg/memgw/collector"
)

// CollectorSink writes to the local collector over its unix socket.
//
// A fresh connection per submission, for the same reason as on Windows: a hook
// is a short-lived process invoked once per lifecycle moment, so there is no
// second submission to amortise a kept connection over.
type CollectorSink struct {
	endpoint string
	timeout  time.Duration
}

// NewCollectorSink names the socket the local collector listens on. An empty
// path means the default, which is where a unit installed from this repository
// puts it.
func NewCollectorSink(endpoint string, timeout time.Duration) *CollectorSink {
	if timeout <= 0 {
		// The collector is on this machine and answers from a local SQLite
		// commit. If it has not answered in five seconds it is not going to,
		// and the hook must not hold the agent session open waiting.
		timeout = 5 * time.Second
	}
	return &CollectorSink{endpoint: endpoint, timeout: timeout}
}

// Name implements Sink.
func (c *CollectorSink) Name() string { return "collector" }

// Submit implements Sink.
func (c *CollectorSink) Submit(ctx context.Context, s Submission) (Result, error) {
	conn, err := collector.Dial(c.endpoint, c.timeout)
	if err != nil {
		// Not reachable is not the same as refused: the caller decides whether
		// to fall back to the gateway, and it can only decide that if the two
		// are distinguishable.
		return Result{Via: c.Name()}, err
	}
	defer conn.Close()
	return submitOverPipe(conn, s)
}
