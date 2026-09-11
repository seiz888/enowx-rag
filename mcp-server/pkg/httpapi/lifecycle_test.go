package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests open a real listener and speak real HTTP to it. A unit test over
// the helpers would prove the struct fields are set; what actually matters is
// whether a request in flight when shutdown starts gets its response, and
// whether a request that arrives after it is refused -- neither of which a
// field check can tell you.

func serverFor(t *testing.T, h http.Handler, opts ServerOptions, hooks Hooks) (*Server, string) {
	t.Helper()
	srv := NewServer("127.0.0.1:0", h, opts, hooks)
	if _, err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	return srv, "http://" + srv.Addr()
}

func TestServeDrainsInFlightRequestAndRefusesNewOnes(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	handler := http.NewServeMux()
	handler.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprint(w, "finished")
	})
	handler.HandleFunc("/fast", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})

	opts := DefaultServerOptions()
	opts.ShutdownTimeout = 2 * time.Second

	var stopped, flushed bool
	var mu sync.Mutex
	hooks := Hooks{
		StopWorkers:  func(context.Context) error { mu.Lock(); stopped = true; mu.Unlock(); return nil },
		FlushMetrics: func(context.Context) error { mu.Lock(); flushed = true; mu.Unlock(); return nil },
	}

	srv, base := serverFor(t, handler, opts, hooks)
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()

	// A request that lands before shutdown must be served normally.
	if body := get(t, base+"/fast"); body != "ok" {
		t.Fatalf("pre-shutdown request returned %q", body)
	}

	// Start a slow request, then trigger shutdown while it is still running.
	slowBody := make(chan string, 1)
	go func() { slowBody <- get(t, base+"/slow") }()
	<-started
	cancel()

	// The listener must stop accepting. A fresh connection is refused; an
	// established keep-alive connection may still be served, which is why this
	// dials rather than reusing the client above.
	waitForRefusal(t, srv.Addr())

	close(release)
	if body := <-slowBody; body != "finished" {
		t.Fatalf("the in-flight request was cut off: %q", body)
	}

	if err := <-serveErr; err != nil {
		t.Fatalf("Serve returned %v; a clean drain must return nil", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !stopped || !flushed {
		t.Fatalf("shutdown hooks did not run: stopWorkers=%v flushMetrics=%v", stopped, flushed)
	}
}

// A drain that does not finish inside the budget is an error, not a log line.
// The process is expected to exit non-zero, because a client cut off mid-write
// cannot tell whether its write committed.
func TestServeReportsAnIncompleteShutdown(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	started := make(chan struct{})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-block
	})
	opts := DefaultServerOptions()
	opts.ShutdownTimeout = 200 * time.Millisecond

	srv, base := serverFor(t, handler, opts, Hooks{})
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()

	go func() { _, _ = http.Get(base + "/stuck") }() //nolint:errcheck // the request is meant to be severed
	<-started
	cancel()

	select {
	case err := <-serveErr:
		if !errors.Is(err, ErrShutdownIncomplete) {
			t.Fatalf("want ErrShutdownIncomplete, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the shutdown budget expired")
	}
}

// A hook failure surfaces. Losing the metrics store quietly is how a process
// ends up reporting a clean exit it did not have.
func TestServeReportsHookFailure(t *testing.T) {
	srv, _ := serverFor(t, http.NotFoundHandler(), DefaultServerOptions(), Hooks{
		FlushMetrics: func(context.Context) error { return errors.New("store already closed") },
	})
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()

	err := <-serveErr
	if err == nil || !strings.Contains(err.Error(), "flush metrics") {
		t.Fatalf("want a flush-metrics failure, got %v", err)
	}
}

// The header timeout is the slowloris defence: a client that opens a connection
// and never finishes its headers must be dropped rather than held forever.
func TestReadHeaderTimeoutClosesASilentClient(t *testing.T) {
	opts := DefaultServerOptions()
	opts.ReadHeaderTimeout = 200 * time.Millisecond

	srv, _ := serverFor(t, http.NotFoundHandler(), opts, Hooks{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// A request line and then silence: the headers never end.
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		// A 408 counts: the server answered rather than hanging.
		return
	} else if errors.Is(err, io.EOF) {
		return
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("the server held a connection whose headers never arrived")
	}
}

func TestDefaultServerOptionsAreBounded(t *testing.T) {
	o := DefaultServerOptions()
	if o.ReadHeaderTimeout <= 0 || o.ReadTimeout <= 0 || o.IdleTimeout <= 0 {
		t.Fatalf("timeouts must be set: %+v", o)
	}
	if o.ShutdownTimeout <= 0 {
		t.Fatal("the shutdown budget must be bounded")
	}
	// WriteTimeout is zero on purpose: /mcp streams, and a global write
	// deadline would sever a legitimately long response. If that ever changes,
	// this test should be the thing that asks why.
	if o.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v; streaming responses need it unset", o.WriteTimeout)
	}
	if o.MaxHeaderBytes <= 0 {
		t.Fatal("header size must be bounded")
	}
}

func get(t *testing.T, url string) string {
	t.Helper()
	// A dedicated transport per call so keep-alive reuse does not mask whether
	// the listener is still accepting.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "error: " + err.Error()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "error: " + err.Error()
	}
	return string(body)
}

// waitForRefusal polls until the listener stops accepting, which is
// asynchronous with respect to the context cancellation that triggered it.
func waitForRefusal(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return
		}
		conn.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the listener was still accepting connections after shutdown began")
}
