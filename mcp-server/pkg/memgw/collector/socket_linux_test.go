//go:build linux

package collector

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Each test binds in its own temporary directory rather than in /run, so a run
// cannot collide with a collector somebody left running and does not need root.
func testSocketPath(t *testing.T) string {
	t.Helper()
	// Unix socket paths are limited to about 100 bytes, so the name is short on
	// purpose: a temporary directory plus a long test name overflows sun_path,
	// and the failure looks like "invalid argument".
	return filepath.Join(t.TempDir(), "c.sock")
}

func TestTheSocketIsNotOpenToEveryone(t *testing.T) {
	// The file mode is the authorisation boundary for local submission, the way
	// the DACL is on Windows, so it is asserted rather than described. The
	// failure this catches is a well-meant chmod that fixes a permission
	// problem by letting any process on the machine write to the shared ledger.
	path := testSocketPath(t)
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the socket came up as %04o, not 0600", perm)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dir.Mode().Perm()&0o022 != 0 {
		t.Fatalf("the socket sits in a directory group or others can write (%04o)", dir.Mode().Perm())
	}
}

func TestAWorldWritableDirectoryIsRefused(t *testing.T) {
	// A 0600 socket inside a directory anyone can write to can be unlinked and
	// replaced, and the adapters would then hand lifecycle events to whoever
	// won the race. The permissions on the socket itself say nothing about that.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	l, err := Listen(filepath.Join(dir, "c.sock"))
	if err == nil {
		l.Close()
		t.Fatal("a socket was bound in a world-writable directory")
	}
	if !strings.Contains(err.Error(), "writable by group or others") {
		t.Fatalf("the refusal does not say what went wrong: %v", err)
	}
}

func TestASecondListenerCannotTakeTheSocket(t *testing.T) {
	// Unlike bind(2), which fails with "address already in use" and would be
	// cleared by removing the file, this must distinguish a live collector from
	// a stale socket: two collectors on one spool is the thing it must not do.
	path := testSocketPath(t)
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	second, err := Listen(path)
	if err == nil {
		second.Close()
		t.Fatal("a second listener took a socket that was already served")
	}
	if !strings.Contains(err.Error(), "already served by a running collector") {
		t.Fatalf("the refusal does not say what went wrong: %v", err)
	}
}

func TestAStaleSocketIsReplacedRatherThanFatal(t *testing.T) {
	// What a killed collector leaves behind. Refusing to start until an
	// operator deletes a file would mean the queue stops draining because of
	// bookkeeping.
	path := testSocketPath(t)
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	// Close the listener without removing the file, the way a SIGKILL would.
	if ul, ok := l.Listener.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	if err := l.Listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("the socket file did not survive the close, so there is no stale file to test: %v", err)
	}
	again, err := Listen(path)
	if err != nil {
		t.Fatalf("a stale socket was treated as fatal: %v", err)
	}
	defer again.Close()
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the replacement socket is wrong: %v (%v)", st, err)
	}
}

// socketClient runs one request/response exchange over a real socket.
func socketClient(t *testing.T, path string, req any) Response {
	t.Helper()
	conn, err := Dial(path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	line, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(line, byte('\n'))); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// serveSocket accepts connections until the listener is closed. It returns a
// counter the test can read, because "the listener answered" and "the listener
// was there" are different claims.
func serveSocket(t *testing.T, l *SocketListener, s *Spool) *atomic.Int64 {
	t.Helper()
	var served atomic.Int64
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			served.Add(1)
			go func() {
				defer conn.Close()
				_ = Serve(t.Context(), s, conn, quiet())
			}()
		}
	}()
	return &served
}

func TestAnEventSubmittedOverTheRealSocketIsDurable(t *testing.T) {
	path := testSocketPath(t)
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s := newSpool(t, SpoolConfig{})
	served := serveSocket(t, l, s)

	resp := socketClient(t, path, Request{
		Op:             "submit",
		IdempotencyKey: "hermes:cp:1",
		Event: json.RawMessage(`{"project_id":"11111111-1111-1111-1111-111111111111",` +
			`"type":"checkpoint.written","payload":{"summary":"s"}}`),
	})
	if !resp.OK || !resp.Durable {
		t.Fatalf("the submission was not accepted: %+v", resp)
	}
	if resp.EventID != EventID("hermes:cp:1").String() {
		t.Fatalf("the collector answered with an event id it did not derive: %s", resp.EventID)
	}
	if served.Load() != 1 {
		t.Fatalf("the listener served %d connections", served.Load())
	}

	// It is on disk, under the derived id, and it decrypts.
	row, err := s.Claim(t.Context())
	if err != nil || row == nil {
		t.Fatalf("the event submitted over the socket is not in the spool: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(row.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["event_id"] != resp.EventID {
		t.Fatalf("the stored body carries a different event id: %v", body["event_id"])
	}
}

func TestTheSocketListenerStopsWhenItIsClosed(t *testing.T) {
	path := testSocketPath(t)
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	s := newSpool(t, SpoolConfig{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _ = Serve(t.Context(), s, conn, quiet()) }()
		}
	}()
	// Prove it is listening before proving it stops; otherwise the test would
	// pass against a listener that never started.
	if resp := socketClient(t, path, Request{Op: "ping"}); !resp.OK {
		t.Fatalf("ping: %+v", resp)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Accept did not return after the listener was closed; shutdown is not bounded")
	}
	if _, err := Dial(path, 500*time.Millisecond); err == nil {
		t.Fatal("the socket still accepts connections after the listener was closed")
	}
}
