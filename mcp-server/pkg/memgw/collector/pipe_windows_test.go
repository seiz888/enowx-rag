//go:build windows

package collector

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The listener tests use a per-test pipe name so a run does not collide with a
// collector somebody left running, and so two tests can run in parallel without
// fighting over one name.
func testPipeName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf(`\\.\pipe\memgw-collector-test-%d-%s`, os.Getpid(), strings.ReplaceAll(t.Name(), "/", "-"))
}

func TestThePipeIsNotOpenToEveryone(t *testing.T) {
	// The ACL is the authorisation boundary for local submission, so it is
	// asserted rather than described. The failure this catches is a well-meant
	// change that adds "Everyone" to fix a permission problem and quietly lets
	// any process on the machine write to the shared ledger.
	sddl, err := DescribeACL()
	if err != nil {
		t.Fatal(err)
	}
	sid, err := CurrentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sddl, sid) {
		t.Fatalf("the DACL does not name this account (%s): %s", sid, sddl)
	}
	if !strings.HasPrefix(sddl, "D:P") {
		t.Fatalf("the DACL is not protected, so it can inherit access from elsewhere: %s", sddl)
	}
	for _, forbidden := range []struct{ sid, who string }{
		{"S-1-1-0", "Everyone"},
		{";WD)", "Everyone"},
		{";AU)", "Authenticated Users"},
		{";IU)", "Interactive Users"},
		{";AN)", "Anonymous"},
		{";WR)", "Restricted"},
	} {
		if strings.Contains(sddl, forbidden.sid) {
			t.Fatalf("the DACL grants access to %s: %s", forbidden.who, sddl)
		}
	}
}

func TestASecondListenerCannotTakeTheName(t *testing.T) {
	// FILE_FLAG_FIRST_PIPE_INSTANCE. Without it, a process that got there first
	// would own some of the connections and the collector would never know.
	name := testPipeName(t)
	l, err := Listen(name)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	second, err := Listen(name)
	if err == nil {
		second.Close()
		t.Fatal("a second listener took a name that was already in use")
	}
	if !strings.Contains(err.Error(), "already exists") && !strings.Contains(err.Error(), name) {
		t.Fatalf("the refusal does not say what went wrong: %v", err)
	}
}

// pipeClient runs one request/response exchange over a real pipe connection.
func pipeClient(t *testing.T, name string, req any) Response {
	t.Helper()
	conn, err := Dial(name, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	line, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// servePipe accepts connections until the listener is closed. It returns a
// counter the test can read, because "the listener answered" and "the listener
// was there" are different claims.
func servePipe(t *testing.T, l *PipeListener, s *Spool) *atomic.Int64 {
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

func TestAnEventSubmittedOverTheRealPipeIsDurable(t *testing.T) {
	name := testPipeName(t)
	l, err := Listen(name)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s := newSpool(t, SpoolConfig{})
	served := servePipe(t, l, s)

	resp := pipeClient(t, name, Request{
		Op:             "submit",
		IdempotencyKey: "claude-code:cp:1",
		Event: json.RawMessage(`{"project_id":"11111111-1111-1111-1111-111111111111",` +
			`"type":"checkpoint.written","payload":{"summary":"s"}}`),
	})
	if !resp.OK || !resp.Durable {
		t.Fatalf("the submission was not accepted: %+v", resp)
	}
	if resp.EventID != EventID("claude-code:cp:1").String() {
		t.Fatalf("the collector answered with an event id it did not derive: %s", resp.EventID)
	}
	if served.Load() != 1 {
		t.Fatalf("the listener served %d connections", served.Load())
	}

	// It is on disk, under the derived id, and it decrypts.
	row, err := s.Claim(t.Context())
	if err != nil || row == nil {
		t.Fatalf("the event submitted over the pipe is not in the spool: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(row.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["event_id"] != resp.EventID {
		t.Fatalf("the stored body carries a different event id: %v", body["event_id"])
	}
}

func TestTheClientMayNotChooseItsOwnEventID(t *testing.T) {
	name := testPipeName(t)
	l, err := Listen(name)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s := newSpool(t, SpoolConfig{})
	servePipe(t, l, s)

	resp := pipeClient(t, name, Request{
		Op:             "submit",
		IdempotencyKey: "k",
		Event: json.RawMessage(`{"event_id":"00000000-0000-0000-0000-000000000001",` +
			`"project_id":"11111111-1111-1111-1111-111111111111","type":"work.planned"}`),
	})
	if resp.OK {
		t.Fatal("a client-chosen event id was accepted; it would change on the client's next restart")
	}
	if resp.Class != "malformed" {
		t.Fatalf("the refusal class is %q", resp.Class)
	}
}

func TestAFullQueueIsAnAnswerAndNotASilentDrop(t *testing.T) {
	name := testPipeName(t)
	l, err := Listen(name)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s := newSpool(t, SpoolConfig{MaxRows: 2, ReserveRows: 1})
	servePipe(t, l, s)

	// One routine event fills the non-reserved part.
	if _, err := s.Enqueue(t.Context(), event("filler", "evidence.recorded", 1)); err != nil {
		t.Fatal(err)
	}
	resp := pipeClient(t, name, Request{
		Op:             "submit",
		IdempotencyKey: "overflow",
		Event:          json.RawMessage(`{"project_id":"11111111-1111-1111-1111-111111111111","type":"evidence.recorded"}`),
	})
	if resp.OK {
		t.Fatal("an event was accepted into a full queue")
	}
	if resp.Class != "queue_full" {
		t.Fatalf("a full queue answered %q; a client cannot tell a full queue from a dead collector", resp.Class)
	}
	// A checkpoint still gets through, which is what the reserve is for.
	resp = pipeClient(t, name, Request{
		Op:             "submit",
		IdempotencyKey: "cp",
		Event:          json.RawMessage(`{"project_id":"11111111-1111-1111-1111-111111111111","type":"checkpoint.written"}`),
	})
	if !resp.OK {
		t.Fatalf("the reserve did not admit a checkpoint: %+v", resp)
	}
}

func TestTheListenerStopsWhenItIsClosed(t *testing.T) {
	name := testPipeName(t)
	l, err := Listen(name)
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
	if resp := pipeClient(t, name, Request{Op: "ping"}); !resp.OK {
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
	if _, err := Dial(name, 500*time.Millisecond); err == nil {
		t.Fatal("the pipe still accepts connections after the listener was closed")
	}
}
