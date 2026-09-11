package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// ServerOptions are the HTTP server's timeouts and its shutdown budget.
//
// The server previously ran on http.ListenAndServe with no timeouts and no
// shutdown path at all: a slow client could hold a connection indefinitely, and
// a restart cut in-flight requests mid-response. That is survivable for a
// read-mostly RAG API. It is not survivable for a ledger, where a request cut
// between commit and response leaves the caller unable to tell whether its write
// landed -- exactly the unknown_commit_status the contract works to avoid.
type ServerOptions struct {
	// ReadHeaderTimeout bounds how long a client may take to send headers. This
	// is the slowloris defence and should always be short.
	ReadHeaderTimeout time.Duration
	// ReadTimeout bounds the whole request read.
	ReadTimeout time.Duration
	// WriteTimeout bounds the whole response write. It is zero by default and
	// deliberately so: /mcp is a streamable HTTP endpoint whose responses can
	// legitimately stay open, and a global write deadline would sever them.
	// Bounding response time belongs in per-route middleware, not here.
	WriteTimeout time.Duration
	// IdleTimeout bounds a kept-alive connection between requests.
	IdleTimeout time.Duration
	// MaxHeaderBytes bounds header size.
	MaxHeaderBytes int
	// ShutdownTimeout is how long a graceful shutdown waits for in-flight
	// requests before giving up. The ratified SLO is 2 seconds.
	ShutdownTimeout time.Duration
}

// DefaultServerOptions returns the settings the server runs with.
func DefaultServerOptions() ServerOptions {
	return ServerOptions{
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ShutdownTimeout:   2 * time.Second,
	}
}

// ErrShutdownIncomplete is returned when the drain deadline passed with
// requests still in flight. It is an error, not a log line: a caller that was
// cut off mid-write does not know whether its write committed, and a process
// that exits zero after that has told its supervisor a comfortable lie.
var ErrShutdownIncomplete = errors.New("httpapi: graceful shutdown timed out with requests still in flight")

// Hooks run during shutdown, after the listener stops accepting and after
// in-flight requests have drained. Background workers stop here and metrics are
// flushed here, in that order, because a worker still running when metrics close
// will write to a closed store.
type Hooks struct {
	// StopWorkers stops background work. It is given the remaining shutdown
	// budget and must respect it.
	StopWorkers func(ctx context.Context) error
	// FlushMetrics closes the metrics store. It runs even when StopWorkers
	// failed, because a failure to stop a worker is not a reason to also lose
	// the measurements.
	FlushMetrics func(ctx context.Context) error
}

// Server is a bounded HTTP server with a graceful shutdown path.
type Server struct {
	http  *http.Server
	opts  ServerOptions
	hooks Hooks
	ln    net.Listener
}

// NewServer builds a server around a handler.
func NewServer(addr string, handler http.Handler, opts ServerOptions, hooks Hooks) *Server {
	return &Server{
		http: &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: opts.ReadHeaderTimeout,
			ReadTimeout:       opts.ReadTimeout,
			WriteTimeout:      opts.WriteTimeout,
			IdleTimeout:       opts.IdleTimeout,
			MaxHeaderBytes:    opts.MaxHeaderBytes,
		},
		opts:  opts,
		hooks: hooks,
	}
}

// Listen binds the address without serving. Tests use it to learn the port
// before traffic starts; Serve calls it when it has not already run.
func (s *Server) Listen() (net.Listener, error) {
	if s.ln != nil {
		return s.ln, nil
	}
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return nil, fmt.Errorf("httpapi: listen on %s: %w", s.http.Addr, err)
	}
	s.ln = ln
	return ln, nil
}

// Addr reports the bound address, which is only meaningful after Listen.
func (s *Server) Addr() string {
	if s.ln == nil {
		return s.http.Addr
	}
	return s.ln.Addr().String()
}

// Serve runs until ctx is cancelled, then shuts down gracefully.
//
// The sequence on cancellation is: stop accepting new connections, drain
// in-flight requests within the shutdown budget, stop background workers, flush
// metrics. A drain that does not finish inside the budget returns
// ErrShutdownIncomplete, and the caller is expected to exit non-zero.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := s.Listen()
	if err != nil {
		return err
	}

	serveErr := make(chan error, 1)
	go func() {
		err := s.http.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		// The listener died on its own: a port conflict, a closed fd. Shutdown
		// hooks still run, because workers started before Serve are still going.
		s.runHooks(context.Background())
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.opts.ShutdownTimeout)
	defer cancel()

	drainErr := s.http.Shutdown(shutdownCtx)
	// Shutdown returns once the listener is closed and connections have drained
	// or the deadline passed. Either way the hooks run: workers must stop even
	// when a client refused to let go.
	hookErr := s.runHooks(shutdownCtx)

	if drainErr != nil {
		if errors.Is(drainErr, context.DeadlineExceeded) {
			// Close is the last resort: it severs whatever is left rather than
			// leaving the process alive holding connections forever.
			_ = s.http.Close()
			return ErrShutdownIncomplete
		}
		return fmt.Errorf("httpapi: shutdown: %w", drainErr)
	}
	return hookErr
}

func (s *Server) runHooks(ctx context.Context) error {
	var firstErr error
	if s.hooks.StopWorkers != nil {
		if err := s.hooks.StopWorkers(ctx); err != nil {
			firstErr = fmt.Errorf("httpapi: stop workers: %w", err)
		}
	}
	if s.hooks.FlushMetrics != nil {
		if err := s.hooks.FlushMetrics(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("httpapi: flush metrics: %w", err)
		}
	}
	return firstErr
}
